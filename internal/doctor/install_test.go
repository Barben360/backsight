package doctor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/reference"
	"github.com/Barben360/backsight/internal/state"
)

func (f *fixture) session(id, project, remote string) {
	put(f.t, filepath.Join(f.env.Dirs.SessionsDir(), id+".json"), `{"project":"`+project+`","remote":"`+remote+`"}`, 0o600)
}

func TestRemotesWarnsOnOneProjectNameWithTwoRemotes(t *testing.T) {
	f := newFixture(t)
	if c := checkRemotes(f.env.Dirs); c.Status != OK {
		t.Errorf("no sessions: %+v", c)
	}
	f.session("s1", "api", "github.com/acme/api")
	f.session("s2", "api", "github.com/acme/api")
	f.session("s3", "billing", "github.com/acme/billing")
	f.session("s4", "notes", "") // outside git: no remote, never a conflict
	f.session("s5", "notes", "")
	// A live record under by-pid is not a session state.
	put(t, filepath.Join(f.env.Dirs.SessionsDir(), "by-pid", "42.json"), `{"project":"api","remote":"github.com/other/api"}`, 0o600)
	if c := checkRemotes(f.env.Dirs); c.Status != OK || !strings.Contains(c.Summary, "3 project") {
		t.Errorf("one remote each: %+v", c)
	}
	f.session("s6", "api", "gitlab.com/other/api")
	c := checkRemotes(f.env.Dirs)
	if c.Status != Warn || !strings.Contains(c.Summary, "two repositories share project:api") {
		t.Fatalf("two remotes: %+v", c)
	}
	if !strings.Contains(c.Detail, "github.com/acme/api (2 sessions)") || !strings.Contains(c.Detail, "gitlab.com/other/api (1 session)") || strings.Contains(c.Detail, "billing") {
		t.Errorf("detail %q", c.Detail)
	}
	if !strings.Contains(c.Fix, "project = ") || !strings.Contains(c.Fix, ".backsight.toml") {
		t.Errorf("fix %q", c.Fix)
	}
	if got := strings.Join(c.Projects, ","); got != "api" {
		t.Errorf("projects %q, want the shared name report scrubs", got)
	}
	// A session without a remote does not count as another repository.
	f.session("s7", "billing", "")
	if c := checkRemotes(f.env.Dirs); !strings.Contains(c.Summary, "project:api") || strings.Contains(c.Summary, "billing") {
		t.Errorf("no remote is not a second repository: %+v", c)
	}
}

func TestRemotesSkipsAnUnreadableSession(t *testing.T) {
	f := newFixture(t)
	put(t, filepath.Join(f.env.Dirs.SessionsDir(), "bad.json"), `{not json`, 0o600)
	f.session("s1", "api", "github.com/acme/api")
	if c := checkRemotes(f.env.Dirs); c.Status != OK {
		t.Errorf("%+v", c)
	}
}

// journalLines writes claude-config journal lines (action, command,
// target, status, run, restores).
func (f *fixture) journal(lines ...string) {
	put(f.t, f.env.Dirs.JournalFile(), strings.Join(lines, "\n")+"\n", 0o600)
}

func jl(action, command, target, status, run, restores string) string {
	return `{"step":"claude-config","action":"` + action + `","command":"` + command + `","target":"` + target + `","status":"` + status + `","run":"` + run + `","restores":"` + restores + `"}`
}

func (f *fixture) wired(profile string) {
	st := state.State{SchemaVersion: state.SchemaVersion, PluginVersion: "0.5.0"}
	if profile != "" {
		st.Client = &state.Client{Profile: profile}
	}
	if err := state.Save(f.env.Dirs.StateFile(), st); err != nil {
		f.t.Fatal(err)
	}
}

func change(path string) claudecfg.Change {
	return claudecfg.Change{Path: path, New: new(`"x"`)}
}

func TestPendingRestoreWarnsWhenAnEditBacksightMadeNoLongerHolds(t *testing.T) {
	f := newFixture(t)
	settings := filepath.Join(f.home, ".claude", "settings.json")
	f.wired("")
	var asked []string
	planned := []claudecfg.FileResult{{Target: settings, Changes: []claudecfg.Change{change("permissions.allow[+]"), change("permissions.allow[+]"), change("cleanupPeriodDays")}}}
	f.env.ConfigEdits = func(profile string) ([]claudecfg.FileResult, error) {
		asked = append(asked, profile)
		return planned, nil
	}
	// Never edited by backsight (no journal): not judged here.
	if c := checkPendingRestore(f.env); c.Status != OK {
		t.Errorf("no journal: %+v", c)
	}
	f.journal(jl("edit", "client install", settings, "done", "20261024T100000.000000Z", ""))
	c := checkPendingRestore(f.env)
	if c.Status != Warn || !strings.Contains(c.Summary, "no longer hold") || !strings.HasSuffix(c.Detail, settings+": permissions.allow[+], cleanupPeriodDays") ||
		!strings.Contains(c.Fix, "backsight client config apply") {
		t.Fatalf("lost edit: %+v", c)
	}
	if len(asked) != 2 || asked[1] != "" {
		t.Errorf("profiles asked %q", asked)
	}
	// The edits hold: nothing to say.
	planned = []claudecfg.FileResult{{Target: settings}}
	if c := checkPendingRestore(f.env); c.Status != OK {
		t.Errorf("edits hold: %+v", c)
	}
}

// What backsight took out on purpose (client config restore, setup
// --undo, uninstall) is not a lost edit, and an unwired client is not
// judged at all.
func TestPendingRestoreIgnoresEditsBacksightUndidOnPurpose(t *testing.T) {
	f := newFixture(t)
	settings := filepath.Join(f.home, ".claude", "settings.json")
	f.wired("")
	f.env.ConfigEdits = func(string) ([]claudecfg.FileResult, error) {
		return []claudecfg.FileResult{{Target: settings, Changes: []claudecfg.Change{change("cleanupPeriodDays")}}}, nil
	}
	for _, last := range []string{
		jl("restore", "client config restore", settings, "done", "20261024T110000.000000Z", ""),
		jl("edit", "setup --undo", settings, "done", "20261024T110000.000000Z", ""),
		jl("edit", "uninstall", settings, "done", "20261024T110000.000000Z", ""),
	} {
		f.journal(jl("edit", "client retention", settings, "done", "20261024T100000.000000Z", ""), last)
		if c := checkPendingRestore(f.env); c.Status != OK {
			t.Errorf("after %s: %+v", last, c)
		}
	}
	// The user re-applied after the restore: judged again.
	f.journal(jl("restore", "client config restore", settings, "done", "20261024T110000.000000Z", ""), jl("edit", "client config apply", settings, "done", "20261024T120000.000000Z", ""))
	if c := checkPendingRestore(f.env); c.Status != Warn {
		t.Errorf("re-applied then lost: %+v", c)
	}
	// Not wired (never installed, or uninstalled): nothing is expected.
	if err := state.Save(f.env.Dirs.StateFile(), state.State{SchemaVersion: state.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	if c := checkPendingRestore(f.env); c.Status != OK {
		t.Errorf("not wired: %+v", c)
	}
}

func TestPendingRestoreUsesTheWiredProfileAndReportsAPlanError(t *testing.T) {
	f := newFixture(t)
	profile := filepath.Join(f.home, ".claude-work")
	f.wired(profile)
	f.journal(jl("edit", "client install", filepath.Join(profile, "settings.json"), "done", "r1", ""))
	var asked string
	f.env.ConfigEdits = func(p string) ([]claudecfg.FileResult, error) {
		asked = p
		return nil, errors.New("settings.json: invalid character")
	}
	c := checkPendingRestore(f.env)
	if asked != profile || c.Status != Warn || !strings.Contains(c.Summary, "cannot") || !strings.Contains(c.Detail, "invalid character") {
		t.Errorf("profile %q, check %+v", asked, c)
	}
}

func TestPendingRestoreWarnsOnARestoreThatNeverFinished(t *testing.T) {
	f := newFixture(t)
	settings := filepath.Join(f.home, ".claude", "settings.json")
	run := "20261024T100000.000000Z"
	put(t, filepath.Join(f.env.Dirs.PreImagesDir(), run, "manifest.json"), `{"ts":"`+run+`","command":"client install","files":[]}`, 0o600)
	f.journal(
		jl("edit", "client install", settings, "done", run, ""),
		jl("restore", "client config restore", "", "started", "", run),
		jl("restore", "client config restore", settings, "done", "20261024T110000.000000Z", ""),
	)
	c := checkPendingRestore(f.env)
	if c.Status != Warn || !strings.Contains(c.Summary, "did not finish") || !strings.Contains(c.Fix, "backsight client config restore "+run) {
		t.Fatalf("unfinished: %+v", c)
	}
	// setup --undo resumes its own.
	f.journal(jl("restore", "setup --undo", "", "started", "", run))
	if c := checkPendingRestore(f.env); c.Status != Warn || !strings.Contains(c.Fix, "backsight setup --undo") {
		t.Errorf("undo: %+v", c)
	}
	// A rerun that finished clears it.
	f.journal(jl("restore", "client config restore", "", "started", "", run), jl("restore", "client config restore", "", "finished", "r2", run))
	if c := checkPendingRestore(f.env); c.Status != OK {
		t.Errorf("finished: %+v", c)
	}
	// A run whose pre-images are gone can no longer be restored: not
	// reported as pending (nothing to finish).
	f.journal(jl("restore", "client config restore", "", "started", "", "20261001T000000.000000Z"))
	if c := checkPendingRestore(f.env); c.Status != OK {
		t.Errorf("pruned run: %+v", c)
	}
}

// refConfig is a backsight config with a backup dir, and a reference
// install pointing at refURL.
func (f *fixture) refConfig(refURL string) (*reference.Install, string) {
	dir := filepath.Join(f.home, "bk")
	put(f.t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:18888\"\n[backup]\ndir = \""+dir+"\"\n", 0o600)
	f.reference(refURL)
	ref, err := reference.Discover(f.home, claudecode.Locate(f.home, f.env.Getenv))
	if err != nil || ref == nil {
		f.t.Fatalf("reference: %v %v", ref, err)
	}
	return ref, dir
}

func (f *fixture) verified(v *state.BackupVerify) {
	st, _ := state.Load(f.env.Dirs.StateFile())
	st.BackupVerify = v
	if err := state.Save(f.env.Dirs.StateFile(), st); err != nil {
		f.t.Fatal(err)
	}
}

func TestReferenceServerIsInfoOnlyOnceANewestArchiveVerifiedWithinAWeek(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ref, dir := f.refConfig("http://127.0.0.1:8888")
	archive := filepath.Join(dir, backup.Name(now.Add(-30*time.Hour)))
	put(t, archive, "x", 0o600)
	var asked string
	f.env.ReferenceServer = func(_ context.Context, url string) (RefServer, error) {
		asked = url
		return RefServer{Running: true, Stop: "docker compose -p hindsight stop", Start: "docker compose -p hindsight start"}, nil
	}
	// Running, no verify yet: not info, and never a warning.
	c := checkReferenceServer(context.Background(), f.env, ref, now)
	if c.Status != OK || !strings.Contains(c.Summary, "still running") || !strings.Contains(c.Detail, "backsight backup verify "+archive) || c.Fix != "" {
		t.Fatalf("no verify: %+v", c)
	}
	if asked != "http://127.0.0.1:8888" {
		t.Errorf("probed %q", asked)
	}
	f.verified(&state.BackupVerify{Archive: archive, Newest: true, OK: true, VerifiedAt: now.Add(-6 * 24 * time.Hour)})
	c = checkReferenceServer(context.Background(), f.env, ref, now)
	if c.Status != Info || !strings.Contains(c.Summary, "can be stopped") || !strings.Contains(c.Fix, "docker compose -p hindsight stop") ||
		!strings.Contains(c.Fix, "data is kept") || !strings.Contains(c.Fix, "docker compose -p hindsight start") {
		t.Fatalf("verified 6 days ago: %+v", c)
	}
	for name, v := range map[string]*state.BackupVerify{
		"older than 7 days":   {Archive: archive, Newest: true, OK: true, VerifiedAt: now.Add(-7*24*time.Hour - time.Minute)},
		"failed":              {Archive: archive, Newest: true, OK: false, VerifiedAt: now.Add(-time.Hour)},
		"not the newest":      {Archive: archive, Newest: false, OK: true, VerifiedAt: now.Add(-time.Hour)},
		"in the future (bad)": {Archive: archive, Newest: true, OK: true, VerifiedAt: now.Add(48 * time.Hour)},
	} {
		f.verified(v)
		if c := checkReferenceServer(context.Background(), f.env, ref, now); c.Status != OK || !strings.Contains(c.Summary, "still running") || c.Fix != "" {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

func TestReferenceServerStoppedOrAbsentOrOursIsOK(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if c := checkReferenceServer(context.Background(), f.env, nil, now); c.Status != OK || !strings.Contains(c.Summary, "no backsight config") {
		t.Errorf("no config: %+v", c)
	}
	ref, dir := f.refConfig("http://127.0.0.1:8888")
	archive := filepath.Join(dir, backup.Name(now.Add(-time.Hour)))
	put(t, archive, "x", 0o600)
	f.verified(&state.BackupVerify{Archive: archive, Newest: true, OK: true, VerifiedAt: now.Add(-time.Hour)})
	f.env.ReferenceServer = func(context.Context, string) (RefServer, error) { return RefServer{}, nil }
	if c := checkReferenceServer(context.Background(), f.env, ref, now); c.Status != OK || !strings.Contains(c.Summary, "not running") {
		t.Errorf("stopped: %+v", c)
	}
	f.env.ReferenceServer = func(context.Context, string) (RefServer, error) {
		return RefServer{}, errors.New("docker: permission denied")
	}
	if c := checkReferenceServer(context.Background(), f.env, ref, now); c.Status != OK || !strings.Contains(c.Detail, "permission denied") {
		t.Errorf("probe error: %+v", c)
	}
	if c := checkReferenceServer(context.Background(), f.env, nil, now); c.Status != OK || !strings.Contains(c.Summary, "no reference server") {
		t.Errorf("no reference: %+v", c)
	}
	// The reference config pointing at backsight's own server (same host
	// and port, written differently) is not a server to stop.
	probed := false
	f.env.ReferenceServer = func(context.Context, string) (RefServer, error) { probed = true; return RefServer{Running: true}, nil }
	own, _ := f.refConfig("http://localhost:18888/")
	if c := checkReferenceServer(context.Background(), f.env, own, now); c.Status != OK || !strings.Contains(c.Summary, "backsight's own server") || probed {
		t.Errorf("own server: %+v (probed %v)", c, probed)
	}
}

func TestReferenceServerWithoutAKnownStopCommandSaysHow(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ref, dir := f.refConfig("http://10.0.0.5:8888")
	archive := filepath.Join(dir, backup.Name(now.Add(-time.Hour)))
	put(t, archive, "x", 0o600)
	f.verified(&state.BackupVerify{Archive: archive, Newest: true, OK: true, VerifiedAt: now.Add(-time.Hour)})
	f.env.ReferenceServer = func(context.Context, string) (RefServer, error) { return RefServer{Running: true}, nil }
	c := checkReferenceServer(context.Background(), f.env, ref, now)
	if c.Status != Info || !strings.Contains(c.Fix, "docker compose stop") || !strings.Contains(c.Fix, "data is kept") {
		t.Errorf("%+v", c)
	}
}

// Run registers the three checks, in that order, after the others.
func TestRunIncludesTheInstallChecks(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	var ids []string
	for _, c := range r.Checks {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, ","); !strings.Contains(got, "remotes,pending-restore,reference-server") {
		t.Errorf("checks %s", got)
	}
}
