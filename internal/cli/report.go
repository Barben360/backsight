package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/reference"
	"github.com/Barben360/backsight/internal/report"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/stats"
)

// reportDays is the telemetry window the report counts errors over.
const reportDays = 7

// osRelease names the Linux distribution; tests replace it.
var osRelease = func() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

// hostname is the machine's name, scrubbed from the report; tests replace it.
var hostname = os.Hostname

// osUser is the OS user name, scrubbed from the report; tests replace it.
var osUser = func() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return os.Getenv("USERNAME")
}

func newReportCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Print a redacted diagnostic and a pre-filled GitHub issue link (nothing is sent)",
		Long: `report gathers versions, the doctor report, local counts and recent
backsight errors, redacted: no memory text, prompt, transcript, secret
(secrets.toml and the server's .env included) or server answer; your home
shows as ~, backsight's directories outside it as <data>, <config>,
<state>, <bin> and <backups>, your user, machine, bank and remote
server hosts (backsight's and the reference's) as <user>, <host>,
<bank> and <server>, git remotes (whole, scp form, without host, owner)
as <remote> and every project name this machine knows (the bank's tags,
local sessions, cursors, telemetry, .backsight.toml files, transverse
aliases) as <project>. It prints a link
to search existing issues for the same error class and a link that opens
a new issue with a summary filled in, plus the full diagnostic to paste
into it.
It sends nothing: you read it and decide (decision 0002).`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			rep, err := buildReport(cmd, d, home)
			if err != nil {
				return blocked(err)
			}
			return e.emit("report", rep, func() string { return reportText(rep) })
		},
	}
}

func buildReport(cmd *cobra.Command, d paths.Dirs, home string) (report.Report, error) {
	now := time.Now().UTC()
	diag := report.Diagnostic{GeneratedAt: now.Truncate(time.Second), Versions: reportVersions(cmd.Context(), d, home)}

	sc := reportScrubber(d, home)

	doc := runDoctor(cmd, d, home)
	diag.Doctor = report.Doctor{Checks: doc.Checks, Errors: doc.Errors, Warnings: doc.Warnings}
	if t := doc.Server; t != nil {
		srv := &report.Server{Source: t.Source}
		if u, err := url.Parse(t.URL); err == nil {
			srv.Scheme = u.Scheme
			srv.Loopback = hideServerHost(sc, u)
		}
		sc.Name(t.Bank, "<bank>")
		diag.Doctor.Server = srv
	}
	// doctor's reference-server check names the reference server's URL,
	// from ~/.hindsight/coding-agent.json (0067 block 4 review rules).
	if ref, err := reference.Discover(home, claudecode.Locate(home, os.Getenv)); err == nil && ref != nil && ref.URL != "" {
		if u, err := url.Parse(ref.URL); err == nil {
			hideServerHost(sc, u)
		}
		sc.Name(ref.Bank, "<bank>")
	}
	for _, p := range reportProjects(cmd.Context(), d, home, doc) {
		sc.Project(p)
	}

	diag.Counts = reportCounts(d)
	if ev, err := stats.Load(d.TelemetryDir(), now.AddDate(0, 0, -reportDays), now.Add(time.Minute)); err == nil {
		diag.Counts.ErrorClasses, diag.RecentErrors = report.Errors(ev)
	} else {
		diag.Counts.ErrorClasses = map[string]int{"telemetry/unreadable": 1}
	}
	if diag.RecentErrors == nil {
		diag.RecentErrors = []report.ErrorEvent{}
	}
	return report.Build(diag, sc)
}

// hideServerHost hides u's host as <server> unless it is the loopback
// address, and reports whether it is.
func hideServerHost(sc *report.Scrubber, u *url.URL) (loopback bool) {
	host := u.Hostname()
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return true
	}
	sc.Name(host, "<server>")
	return false
}

// reportScrubber knows what identifies this machine and its user: the
// home and backsight's roots (the config's backup and compose directories
// too), the machine and user names, and every value of secrets.toml and
// of the server's .env, which may follow no pattern redaction knows.
func reportScrubber(d paths.Dirs, home string) *report.Scrubber {
	sc := report.NewScrubber(home)
	cfg, _ := config.Load(d.ConfigFile(), d, home) // on error: the defaults
	for _, r := range []struct{ dir, with string }{
		{d.Data, "<data>"}, {d.Config, "<config>"}, {d.State, "<state>"}, {d.Bin, "<bin>"},
		{cfg.Backup.Dir, "<backups>"}, {cfg.Server.ComposeDir, "<compose>"},
	} {
		if filepath.IsAbs(r.dir) {
			sc.Root(r.dir, r.with)
		}
	}
	for _, v := range secretValues(d.SecretsFile(), filepath.Join(cfg.Server.ComposeDir, ".env")) {
		sc.Secret(v)
	}
	if h, err := hostname(); err == nil {
		sc.Name(h, "<host>")
		if short, _, ok := strings.Cut(h, "."); ok {
			sc.Name(short, "<host>")
		}
	}
	// A remote holds its project's name: longer, it goes first.
	for _, r := range sessionRemotes(d) {
		sc.Remote(r)
	}
	if u := osUser(); u != "" {
		sc.Name(u, "<user>")
		// Windows names DOMAIN\user.
		if i := strings.LastIndex(u, `\`); i >= 0 {
			sc.Name(u[i+1:], "<user>")
		}
	}
	return sc
}

// sessionRemotes are the git remotes the session states recorded
// (doctor's remotes check shows them; decision 0045).
func sessionRemotes(d paths.Dirs) []string {
	var out []string
	for _, raw := range jsonFiles(d.SessionsDir()) {
		var s struct {
			Remote string `json:"remote"`
		}
		if json.Unmarshal(raw, &s) == nil && s.Remote != "" && !slices.Contains(out, s.Remote) {
			out = append(out, s.Remote)
		}
	}
	return out
}

// jsonFiles reads dir's *.json files, the unreadable ones skipped.
func jsonFiles(dir string) [][]byte {
	m, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out [][]byte
	for _, f := range m {
		if raw, err := os.ReadFile(f); err == nil {
			out = append(out, raw)
		}
	}
	return out
}

// reportProjects is every project name this machine knows, for report to
// hide (backsight issue 266): the configured transverse aliases; the
// projects of the session states (their injections' too), of the live
// records under by-pid and of what the .backsight.toml files of their
// directories name (project and related); the projects of the cursors and
// of the telemetry; and the bank's project:* tags when doctor reached the
// server. Doctor's checks list the names they show themselves
// (doctor.Check.Projects). Transcripts are not read.
func reportProjects(ctx context.Context, d paths.Dirs, home string, doc doctor.Report) []string {
	cfg, _ := config.Load(d.ConfigFile(), d, home) // on error: the defaults
	var out []string
	add := func(ps ...string) {
		for _, p := range ps {
			if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	add(cfg.Projects.Transverse...)
	for _, raw := range jsonFiles(d.SessionsDir()) {
		var s struct {
			Project  string `json:"project"`
			Injected []struct {
				Project string `json:"project"`
			} `json:"injected"`
		}
		if json.Unmarshal(raw, &s) == nil {
			add(s.Project)
			for _, i := range s.Injected {
				add(i.Project)
			}
		}
	}
	// Read whatever the transverse setting: an invalid config that empties
	// it resolves with the default aliases (the resolver needs one).
	transverse := cfg.Projects.Transverse
	if len(transverse) == 0 || transverse[0] == "" {
		transverse = config.Default(d, home).Projects.Transverse
	}
	res := project.New(home, transverse)
	for _, raw := range jsonFiles(filepath.Join(d.SessionsDir(), "by-pid")) {
		var l session.Live
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		add(l.Project)
		if filepath.IsAbs(l.Cwd) {
			add(res.Project(l.Cwd))
			add(res.Related(l.Cwd)...)
		}
	}
	for _, raw := range jsonFiles(d.CursorsDir()) {
		// Per-project documents name conversation:<sid>@<project>, and
		// their cursors the session's home and per-project segments.
		var c struct {
			Document string         `json:"document"`
			Home     string         `json:"home"`
			Project  string         `json:"project"`
			Segments map[string]int `json:"segments"`
		}
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		add(c.Home, c.Project)
		if _, p, ok := strings.Cut(c.Document, "@"); ok {
			add(p)
		}
		for p := range c.Segments {
			add(p)
		}
	}
	add(telemetryProjects(d.TelemetryDir())...)
	add(bankProjects(ctx, doc)...)
	return out
}

// telemetryProjects are the projects every telemetry line names, over the
// whole telemetry (doctor and older errors may name any of them).
func telemetryProjects(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		rd := bufio.NewReader(fh)
		for {
			line, err := rd.ReadBytes('\n')
			var e struct {
				Project string `json:"project"`
			}
			if json.Unmarshal(line, &e) == nil && e.Project != "" && !seen[e.Project] {
				seen[e.Project] = true
				out = append(out, e.Project)
			}
			if err != nil {
				break
			}
		}
		_ = fh.Close()
	}
	return out
}

// bankProjects are the bank's project:* tags, asked only when doctor's
// server check reached the server, with doctor's per-call timeout. A
// failure keeps what came back: the local names are hidden anyway.
func bankProjects(ctx context.Context, doc doctor.Report) []string {
	t := doc.Server
	if t == nil {
		return nil
	}
	reached := false
	for _, c := range doc.Checks {
		reached = reached || c.ID == "server" && (c.Status == doctor.OK || c.Status == doctor.Warn)
	}
	if !reached {
		return nil
	}
	cl, err := hindsight.New(t.URL, string(t.Token), hindsightTimeout)
	if err != nil {
		return nil
	}
	tags, _ := cl.Tags(ctx, t.Bank, "project:")
	out := make([]string, 0, len(tags))
	for _, tg := range tags {
		out = append(out, strings.TrimPrefix(tg.Tag, "project:"))
	}
	return out
}

// envSecretKey names the .env entries that hold a secret even unquoted.
var envSecretKey = regexp.MustCompile(`(?i)(key|pass|secret|token)`)

// secretValues reads every value of secrets.toml (whatever its mode or
// keys: reading is not showing) and the secrets of the server's .env
// (single-quoted values and secret-named keys), so the report can scrub
// them. A file that does not parse is read line by line.
func secretValues(secretsFile, envFile string) []string {
	var out []string
	if raw, err := os.ReadFile(secretsFile); err == nil {
		var tree map[string]any
		if toml.Unmarshal(raw, &tree) == nil {
			out = appendStrings(out, tree)
		} else {
			for _, l := range strings.Split(string(raw), "\n") {
				if _, v, ok := strings.Cut(l, "="); ok {
					out = append(out, strings.Trim(strings.TrimSpace(v), `"'`))
				}
			}
		}
	}
	if raw, err := os.ReadFile(envFile); err == nil {
		for _, l := range strings.Split(string(raw), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
			if !ok || strings.HasPrefix(k, "#") {
				continue
			}
			v = strings.TrimSpace(v)
			quoted := len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0]
			if quoted || envSecretKey.MatchString(k) {
				out = append(out, strings.Trim(v, `"'`))
			}
		}
	}
	return out
}

func appendStrings(out []string, v any) []string {
	switch t := v.(type) {
	case string:
		return append(out, t)
	case map[string]any:
		for _, x := range t {
			out = appendStrings(out, x)
		}
	case []any:
		for _, x := range t {
			out = appendStrings(out, x)
		}
	}
	return out
}

func reportVersions(ctx context.Context, d paths.Dirs, home string) report.Versions {
	v := report.Versions{Binary: buildinfo.Version, Commit: buildinfo.Commit, Date: buildinfo.Date, Go: runtime.Version(),
		HindsightPin: server.HindsightImage, OS: runtime.GOOS, Arch: runtime.GOARCH, OSRelease: osRelease()}
	if st, err := state.Load(d.StateFile()); err == nil {
		v.Plugin = st.PluginVersion
	}
	if cfg, err := config.Load(d.ConfigFile(), d, home); err == nil {
		v.HindsightDeployed, _ = runtimeFor(cfg).Deployed()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := claudeRunner.Run(ctx, nil, "--version")
	switch {
	case errors.Is(err, errClaudeMissing):
		v.ClaudeCode = "not found"
	case err != nil:
		v.ClaudeCode = "unknown"
	default:
		v.ClaudeCode = strings.Fields(string(out) + " unknown")[0]
	}
	return v
}

func reportCounts(d paths.Dirs) report.Counts {
	c := report.Counts{Days: reportDays}
	c.SessionsSent = countFiles(d.CursorsDir(), "*.json")
	c.SessionsTracked = countFiles(d.SessionsDir(), "*.json")
	if q, err := retain.QuarantinedSessions(d.CursorsDir()); err == nil {
		c.SessionsQuarantined = len(q)
	}
	ob := retain.Outbox{Path: filepath.Join(d.OutboxDir(), "notes.jsonl"), QuarantinePath: filepath.Join(d.QuarantineDir(), "notes.jsonl")}
	c.NotesQueued, _ = ob.Pending()
	c.NotesQuarantined, _ = ob.Quarantined()
	return c
}

func countFiles(dir, pattern string) int {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	return len(m)
}

func reportText(r report.Report) string {
	var b strings.Builder
	b.WriteString("backsight report: nothing was sent. Read the diagnostic below, then decide.\n\n")
	if r.Class != "" {
		b.WriteString("Error class: " + r.Class + "\n")
		b.WriteString("Existing issues with this class (look here first):\n  " + r.SearchURL + "\n")
	} else {
		b.WriteString("doctor found no error and the telemetry holds no recent one.\n")
	}
	b.WriteString("New issue with the title and a summary filled in (you submit it, or not):\n  " + r.IssueURL + "\n")
	if r.Omitted > 0 {
		b.WriteString("  (" + strconv.Itoa(r.Omitted) + " summary line(s) left out to keep the link short)\n")
	}
	b.WriteString("\nPaste this block into the issue, under \"Full diagnostic\":\n\n")
	b.WriteString(r.Paste)
	return b.String()
}
