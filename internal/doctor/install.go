package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/reference"
	"github.com/Barben360/backsight/internal/state"
)

// InstallProbes are what the install checks ask outside backsight's own
// files; a nil probe skips its part or uses the built-in default.
type InstallProbes struct {
	// ReferenceServer reports whether a Hindsight server answers at url
	// and how to stop it (nil: doctor asks /health, with no stop command).
	ReferenceServer func(ctx context.Context, url string) (RefServer, error)
	// ConfigEdits plans backsight's own edits to the Claude Code files of
	// profile ("" = the default): what `client config apply` would change
	// now (nil: not compared).
	ConfigEdits func(profile string) ([]claudecfg.FileResult, error)
}

// RefServer is a reference server as seen from this machine. Stop and
// Start are commands for the user ("" when unknown).
type RefServer struct {
	Running     bool
	Stop, Start string
}

// verifyFresh: a backup verify older than this no longer proves that
// backsight's backups restore (0067 block 4).
const verifyFresh = 7 * 24 * time.Hour

// installChecks are the M5 block 4 checks (0067): remotes, pending
// restores and edits, and whether the reference server can be stopped.
func installChecks(ctx context.Context, env Env, ref *reference.Install, now time.Time) []Check {
	return []Check{checkRemotes(env.Dirs), checkPendingRestore(env), checkReferenceServer(ctx, env, ref, now)}
}

// checkRemotes warns when one project name was seen with two git remotes
// (decision 0045): two repositories would share one observation pool.
// Session states are kept 30 days, so is what it sees.
func checkRemotes(d paths.Dirs) Check {
	c := Check{ID: "remotes", Status: OK}
	ents, _ := os.ReadDir(d.SessionsDir())
	seen := map[string]map[string]int{} // project → remote → sessions
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d.SessionsDir(), e.Name()))
		if err != nil {
			continue
		}
		var s struct {
			Project string `json:"project"`
			Remote  string `json:"remote"`
		}
		if json.Unmarshal(raw, &s) != nil || s.Project == "" {
			continue
		}
		if seen[s.Project] == nil {
			seen[s.Project] = map[string]int{}
		}
		if s.Remote != "" {
			seen[s.Project][s.Remote]++
		}
	}
	var shared []string
	var detail []string
	for p, remotes := range seen {
		if len(remotes) < 2 {
			continue
		}
		shared = append(shared, p)
		var rs []string
		for r := range remotes {
			rs = append(rs, r)
		}
		sort.Strings(rs)
		for i, r := range rs {
			rs[i] = fmt.Sprintf("%s (%d session%s)", r, remotes[r], map[bool]string{true: "", false: "s"}[remotes[r] == 1])
		}
		detail = append(detail, "project:"+p+": "+strings.Join(rs, ", "))
	}
	if len(shared) == 0 {
		c.Summary = fmt.Sprintf("no project name seen with two git remotes (%d project(s) in recent sessions)", len(seen))
		return c
	}
	sort.Strings(shared)
	sort.Strings(detail)
	c.Status, c.Projects = Warn, shared
	if len(shared) == 1 {
		c.Summary = "two repositories share project:" + shared[0]
	} else {
		c.Summary = fmt.Sprintf("%d project names are shared by several repositories: %s", len(shared), strings.Join(shared, ", "))
	}
	c.Detail = strings.Join(detail, "; ") + "; their memories mix in one pool (decision 0045)"
	c.Fix = `set project = "<another name>" in a .backsight.toml at the root of one repository, and commit it so every clone uses it`
	return c
}

// undoCommands take backsight's edits out on purpose: a file they touched
// last is not expected to hold the edit.
var undoCommands = map[string]bool{"setup --undo": true, "uninstall": true, "client config restore": true}

// checkPendingRestore reports a Claude Code file backsight edited that no
// longer holds the edit, and a pre-image restore that was journaled and
// never finished (SPEC §15).
func checkPendingRestore(env Env) Check {
	c := Check{ID: "pending-restore", Status: OK, Summary: "Claude Code's files hold backsight's edits; no restore pending"}
	lines, err := claudecfg.ReadJournal(env.Dirs.JournalFile())
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot read the config editor's journal", err.Error()
		return c
	}
	var pending []claudecfg.JournalLine
	for _, p := range claudecfg.PendingRestores(lines) {
		// Pre-images pruned or removed: there is nothing left to finish.
		if _, err := claudecfg.LoadManifest(env.Dirs.PreImagesDir(), p.Restores); err == nil {
			pending = append(pending, p)
		}
	}
	if len(pending) > 0 {
		p := pending[len(pending)-1]
		c.Status = Warn
		c.Summary = fmt.Sprintf("a restore of Claude Code's files did not finish (%s, run %s)", p.Command, p.Restores)
		c.Detail = "some files may be back as they were before that run, others not"
		if p.Command == "setup --undo" {
			c.Fix = "backsight setup --undo (resumes where it stopped)"
		} else {
			c.Fix = "backsight client config restore " + p.Restores + " (add --force if it refuses a file changed since)"
		}
		return c
	}

	st, err := state.Load(env.Dirs.StateFile())
	if err != nil || st.PluginVersion == "" || env.ConfigEdits == nil {
		return c // not wired: backsight expects no edit in place
	}
	last := map[string]claudecfg.JournalLine{} // target → its last config-editor line
	for _, l := range lines {
		if l.Step == "claude-config" && l.Target != "" && (l.Action == "edit" || l.Action == "restore") {
			last[l.Target] = l
		}
	}
	profile := ""
	if st.Client != nil {
		profile = st.Client.Profile
	}
	planned, err := env.ConfigEdits(profile)
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot compare Claude Code's files with backsight's edits", err.Error()
		c.Fix = "fix the file named in the detail by hand; backsight never overwrites a file it cannot parse"
		return c
	}
	var lost []string
	for _, fr := range planned {
		l, ok := last[fr.Target]
		if len(fr.Changes) == 0 || !ok || l.Action != "edit" || undoCommands[l.Command] {
			continue
		}
		var keys []string
		for _, ch := range fr.Changes {
			// permissions.allow[+] once, however many entries it adds.
			if !slices.Contains(keys, ch.Path) {
				keys = append(keys, ch.Path)
			}
		}
		lost = append(lost, fr.Target+": "+strings.Join(keys, ", "))
	}
	if len(lost) > 0 {
		c.Status, c.Summary = Warn, fmt.Sprintf("%d file(s) backsight edited no longer hold its edits", len(lost))
		c.Detail = strings.Join(lost, "; ")
		c.Fix = "backsight client config apply (puts them back, each file backed up first; backsight client install also restores the permission rule)"
	}
	return c
}

// checkReferenceServer says, as information only, that a reference server
// still running can be stopped: once a backup verify of backsight's newest
// archive passed within 7 days (0067 block 4, SPEC §11.5). Every other
// case is ok: running a fallback is never a fault.
func checkReferenceServer(ctx context.Context, env Env, ref *reference.Install, now time.Time) Check {
	c := Check{ID: "reference-server", Status: OK}
	cfg, err := config.Load(env.Dirs.ConfigFile(), env.Dirs, env.Home)
	if err != nil {
		c.Summary = "skipped: no backsight config"
		return c
	}
	if ref == nil || ref.URL == "" {
		c.Summary = "no reference server configured"
		return c
	}
	if sameServer(ref.URL, cfg.Server.URL) {
		c.Summary = "the reference config points at backsight's own server"
		return c
	}
	probe := env.ReferenceServer
	if probe == nil {
		probe = func(ctx context.Context, u string) (RefServer, error) {
			cl, err := hindsight.New(u, "", env.Timeout)
			if err != nil {
				return RefServer{}, err
			}
			_, err = cl.Health(ctx)
			return RefServer{Running: err == nil}, nil
		}
	}
	srv, err := probe(ctx, ref.URL)
	if err != nil {
		c.Summary, c.Detail = "cannot tell whether the reference server at "+ref.URL+" runs", err.Error()
		return c
	}
	if !srv.Running {
		c.Summary = "the reference server at " + ref.URL + " is not running"
		return c
	}
	c.Summary = "the reference server at " + ref.URL + " is still running"
	var newest string
	if list, err := backup.List(cfg.Backup.Dir); err == nil && len(list) > 0 {
		newest = list[0].Path
	}
	st, _ := state.Load(env.Dirs.StateFile())
	v := st.BackupVerify
	age := time.Duration(0)
	if v != nil {
		age = now.Sub(v.VerifiedAt)
	}
	why := ""
	switch {
	case v == nil:
		why = "no backup verify recorded yet"
	case !v.OK:
		why = "the last backup verify (" + v.VerifiedAt.UTC().Format("2006-01-02") + ") failed"
	case !v.Newest:
		why = "the last backup verify was of " + filepath.Base(v.Archive) + ", not the newest archive then"
	case age < 0 || age > verifyFresh:
		why = "the last backup verify (" + v.VerifiedAt.UTC().Format("2006-01-02") + ") is older than 7 days"
	}
	if why != "" {
		c.Detail = why + "; it can be stopped once a verify of backsight's newest archive passes"
		if newest != "" {
			c.Detail += ": backsight backup verify " + newest
		}
		return c
	}
	c.Status = Info
	c.Summary = "the reference server at " + ref.URL + " can be stopped: backsight's newest archive passed backup verify on " + v.VerifiedAt.UTC().Format("2006-01-02")
	c.Detail = "backsight's own server holds the memory now; the reference stays a fallback until you stop it"
	if srv.Stop != "" {
		c.Fix = srv.Stop + " (its data is kept: the database volume stays"
		if srv.Start != "" {
			c.Fix += ", and " + srv.Start + " brings it back"
		}
		c.Fix += ")"
	} else {
		c.Fix = "stop it where it was started (docker compose stop in its directory); its data is kept: stop leaves the database volume in place"
	}
	return c
}

// sameServer compares two server URLs by scheme, host and port
// (localhost is the loopback address).
func sameServer(a, b string) bool {
	norm := func(raw string) (string, bool) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			return "", false
		}
		host, port := u.Hostname(), u.Port()
		if port == "" {
			port = map[string]string{"https": "443"}[u.Scheme]
			if port == "" {
				port = "80"
			}
		}
		if host == "localhost" {
			host = "127.0.0.1"
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			host = "127.0.0.1"
		}
		return u.Scheme + "://" + net.JoinHostPort(host, port), true
	}
	na, okA := norm(a)
	nb, okB := norm(b)
	return okA && okB && na == nb
}
