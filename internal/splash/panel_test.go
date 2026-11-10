package splash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/termstyle"
)

var testNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func write(t testing.TB, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fixture is a set-up host: config, reach, state, outbox and a doctor
// snapshot, all readable.
func fixture(t testing.TB) Sources {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	write(t, d.ConfigFile(), "schema_version = 1\n[server]\nmode = \"local\"\nrole = \"both\"\nurl = \"http://127.0.0.1:8888\"\n")
	write(t, filepath.Join(d.State, "reach.json"), `{"last_ok":"2026-11-10T11:58:00Z","sessions_waiting":3}`)
	write(t, d.StateFile(), `{"schema_version":1,"update_check":{"latest":"0.1.35-m7","installed":"0.1.34-m7"}}`)
	write(t, filepath.Join(d.OutboxDir(), "notes.jsonl"), "{\"a\":1}\n{\"b\":2}\n")
	write(t, filepath.Join(d.State, "doctor-snapshot.json"), `{"generated_at":"2026-11-09T17:00:00Z","report":{"checks":[`+
		`{"status":"ok"},{"status":"ok"},{"status":"ok"},{"status":"info"},{"status":"warn"}]}}`)
	return Sources{Dirs: d, Now: testNow, Zone: time.UTC, Version: "0.1.34-m7", Hindsight: "1.2.0", Deadline: 2 * time.Second}
}

func texts(ls []Line) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = text(l)
	}
	return out
}

// line is the panel line under label, "" when absent.
func line(ls []Line, label string) string {
	for _, s := range texts(ls) {
		if strings.HasPrefix(s, label+" ") {
			return s
		}
	}
	return ""
}

func roleOf(t *testing.T, ls []Line, label, word string) termstyle.Role {
	t.Helper()
	for _, l := range ls {
		if !strings.HasPrefix(text(l), label+" ") {
			continue
		}
		for _, s := range l {
			if strings.Contains(s.Text, word) && !s.Plain {
				return s.Role
			}
		}
	}
	t.Fatalf("%s: no painted %q", label, word)
	return 0
}

func TestPanelFromFixtures(t *testing.T) {
	ls, kind := Build(fixture(t))
	want := []string{
		"backsight 0.1.34-m7 · Hindsight 1.2.0",
		"─────────────────────────────────────",
		"Server   answering · last answer 2m ago",
		"Setup    host · docker",
		"Queue    3 sessions · 2 notes waiting",
		"Checks   4 ok · 1 warn · 0 fail (last run 19h ago)",
		"Update   0.1.35-m7 available · backsight update apply",
	}
	if got := texts(ls); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if kind != "blocks" {
		t.Fatalf("logo kind %q, want blocks", kind)
	}
	for _, c := range []struct {
		label, word string
		role        termstyle.Role
	}{
		{"Server", "answering", termstyle.OK}, {"Queue", "3 sessions", termstyle.Warn}, {"Queue", "2 notes", termstyle.Warn},
		{"Checks", "4 ok", termstyle.OK}, {"Checks", "1 warn", termstyle.Warn}, {"Checks", "0 fail", termstyle.Secondary},
		{"Update", "available", termstyle.Accent}, {"Update", "backsight update apply", termstyle.Command},
	} {
		if r := roleOf(t, ls, c.label, c.word); r != c.role {
			t.Errorf("%s %q painted %d, want %d", c.label, c.word, r, c.role)
		}
	}
	if ls[0][0].Role != termstyle.Brand || ls[0][0].Text != "backsight" {
		t.Errorf("title starts with %+v, want the brand word", ls[0][0])
	}
}

func TestPanelBadFileDropsOnlyItsLine(t *testing.T) {
	for _, c := range []struct {
		name, file string
		body       *string // nil: removed
		gone       []string
	}{
		{"reach missing", "state/reach.json", nil, []string{"Server", "Queue"}},
		{"reach corrupt", "state/reach.json", ptr("{"), []string{"Server", "Queue"}},
		{"reach oversized", "state/reach.json", ptr(strings.Repeat(" ", reachCap) + "{}"), []string{"Server", "Queue"}},
		{"snapshot missing", "state/doctor-snapshot.json", nil, []string{"Checks"}},
		{"snapshot corrupt", "state/doctor-snapshot.json", ptr("not json"), []string{"Checks"}},
		{"snapshot oversized", "state/doctor-snapshot.json", ptr(strings.Repeat(" ", snapshotCap) + "{}"), []string{"Checks"}},
		{"state corrupt", "state/state.json", ptr("["), []string{"Update"}},
		{"state oversized", "state/state.json", ptr(strings.Repeat(" ", stateCap) + "{}"), []string{"Update"}},
		{"config corrupt", "config/config.toml", ptr("[server"), []string{"Setup"}},
		{"config oversized", "config/config.toml", ptr("#" + strings.Repeat("x", fileCap) + "\n"), []string{"Setup"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := fixture(t)
			root := filepath.Dir(src.Dirs.State)
			p := filepath.Join(root, filepath.FromSlash(c.file))
			if c.body == nil {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			} else {
				write(t, p, *c.body)
			}
			ls, _ := Build(src)
			for _, label := range []string{"Server", "Setup", "Queue", "Checks", "Update"} {
				gone := false
				for _, g := range c.gone {
					gone = gone || g == label
				}
				if got := line(ls, label); (got == "") != gone {
					t.Errorf("%s: line %q, want gone=%v", label, got, gone)
				}
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestPanelOutboxCap(t *testing.T) {
	src := fixture(t)
	write(t, filepath.Join(src.Dirs.OutboxDir(), "notes.jsonl"), strings.Repeat("{}\n", 1500))
	ls, _ := Build(src)
	if got := line(ls, "Queue"); got != "Queue    3 sessions · 1000+ notes waiting" {
		t.Fatalf("got %q", got)
	}
	write(t, filepath.Join(src.Dirs.OutboxDir(), "notes.jsonl"), "{}\n")
	write(t, filepath.Join(src.Dirs.State, "reach.json"), `{"last_ok":"2026-11-10T11:58:00Z","sessions_waiting":1}`)
	ls, _ = Build(src)
	if got := line(ls, "Queue"); got != "Queue    1 session · 1 note waiting" {
		t.Fatalf("got %q", got)
	}
	_ = os.Remove(filepath.Join(src.Dirs.OutboxDir(), "notes.jsonl"))
	write(t, filepath.Join(src.Dirs.State, "reach.json"), `{"last_ok":"2026-11-10T11:58:00Z","sessions_waiting":0}`)
	ls, _ = Build(src)
	if got := line(ls, "Queue"); got != "Queue    0 sessions · 0 notes waiting" {
		t.Fatalf("got %q", got)
	}
	if r := roleOf(t, ls, "Queue", "0 sessions"); r != termstyle.Secondary {
		t.Fatalf("an empty queue painted %d, want dim", r)
	}
}

func TestPanelServerStates(t *testing.T) {
	for _, c := range []struct {
		name, config, reach, breaker, state string
		server, setup                       string
		role                                termstyle.Role
		word                                string
	}{
		{name: "silent recently", reach: `{"last_ok":"2026-11-10T11:00:00Z","failing_since":"2026-11-10T11:30:00Z"}`,
			server: "Server   silent since 2026-11-10 11:30 (30m)", role: termstyle.Warn, word: "silent"},
		{name: "silent long, recall paused", reach: `{"failing_since":"2026-11-10T09:00:00Z"}`, breaker: `{"failures":5,"open_until":"2026-11-10T12:05:00Z"}`,
			server: "Server   silent since 2026-11-10 09:00 (3h) · recall paused", role: termstyle.Fail, word: "silent"},
		{name: "breaker closed again", reach: `{"failing_since":"2026-11-10T09:00:00Z"}`, breaker: `{"failures":5,"open_until":"2026-11-10T11:05:00Z"}`,
			server: "Server   silent since 2026-11-10 09:00 (3h)", role: termstyle.Fail, word: "silent"},
		{name: "never answered", reach: `{"sessions_waiting":0}`, server: "Server   not recorded yet", role: termstyle.Secondary, word: "not recorded"},
		{name: "client", config: "schema_version = 1\n[server]\nmode = \"remote\"\nurl = \"https://nas.example:8888/x\"\n",
			server: "Server   nas.example answering · last answer 2m ago", setup: "Setup    client of nas.example", role: termstyle.OK, word: "answering"},
		{name: "another server", config: "schema_version = 1\n[server]\nmode = \"remote\"\nurl = \"https://nas.example:8888\"\n",
			state:  `{"remote_server":{"id":"a","mismatch":"b"}}`,
			server: "Server   another server answers at nas.example", setup: "Setup    client of nas.example", role: termstyle.Fail, word: "another server"},
		{name: "native host", config: "schema_version = 1\n[server]\nruntime = \"native\"\n", server: "Server   answering · last answer 2m ago", setup: "Setup    host · native", role: termstyle.OK, word: "answering"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := fixture(t)
			if c.config != "" {
				write(t, src.Dirs.ConfigFile(), c.config)
			}
			if c.reach != "" {
				write(t, filepath.Join(src.Dirs.State, "reach.json"), c.reach)
			}
			if c.breaker != "" {
				write(t, filepath.Join(src.Dirs.State, "breaker.json"), c.breaker)
			}
			if c.state != "" {
				write(t, src.Dirs.StateFile(), c.state)
			}
			ls, _ := Build(src)
			if got := line(ls, "Server"); got != c.server {
				t.Errorf("Server: got %q, want %q", got, c.server)
			}
			if c.setup != "" {
				if got := line(ls, "Setup"); got != c.setup {
					t.Errorf("Setup: got %q, want %q", got, c.setup)
				}
			}
			if r := roleOf(t, ls, "Server", c.word); r != c.role {
				t.Errorf("Server %q painted %d, want %d", c.word, r, c.role)
			}
		})
	}
}

func TestPanelServerOnlyHost(t *testing.T) {
	src := fixture(t)
	write(t, src.Dirs.ConfigFile(), "schema_version = 1\n[server]\nrole = \"server\"\nruntime = \"native\"\n")
	_ = os.Remove(filepath.Join(src.Dirs.State, "reach.json"))
	ls, _ := Build(src)
	if got := line(ls, "Setup"); got != "Setup    server only · native" {
		t.Fatalf("Setup: %q", got)
	}
	if line(ls, "Server") != "" || line(ls, "Queue") != "" {
		t.Fatalf("a server-only host shows hooks' lines: %q", texts(ls))
	}
}

func TestPanelUpdateOnlyWhenNewer(t *testing.T) {
	src := fixture(t)
	for _, latest := range []string{"0.1.34-m7", "0.1.33-m7", ""} {
		write(t, src.Dirs.StateFile(), `{"update_check":{"latest":"`+latest+`"}}`)
		if ls, _ := Build(src); line(ls, "Update") != "" {
			t.Errorf("latest %q: an Update line", latest)
		}
	}
}

func TestPanelFreshMachineCreatesNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	src := Sources{Dirs: paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")},
		Now: testNow, Version: "0.1.34-m7", Hindsight: "1.2.0"}
	ls, kind := Build(src)
	want := "backsight 0.1.34-m7 · Hindsight 1.2.0\n─────────────────────────────────────\nSetup    not set up yet · backsight setup"
	if got := strings.Join(texts(ls), "\n"); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if kind != "blocks" {
		t.Fatalf("kind %q", kind)
	}
	if r := roleOf(t, ls, "Setup", "backsight setup"); r != termstyle.Command {
		t.Fatalf("the setup command painted %d", r)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the panel created %s (%v)", root, err)
	}
}

func TestPanelNeverNamesTheProject(t *testing.T) {
	src := fixture(t)
	dir := filepath.Join(t.TempDir(), "zz-project-name")
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	t.Chdir(dir)
	ls, _ := Build(src)
	if j := strings.Join(texts(ls), "\n"); strings.Contains(j, "zz-project-name") {
		t.Fatalf("the panel names the project:\n%s", j)
	}
}

func TestPanelSlowReaderDropsItsLine(t *testing.T) {
	src := fixture(t)
	src.Deadline = 300 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	saved := readFile
	t.Cleanup(func() { readFile = saved })
	readFile = func(path string, limit int64) ([]byte, error) {
		if filepath.Base(path) == "doctor-snapshot.json" {
			<-release
		}
		return saved(path, limit)
	}
	start := time.Now()
	ls, _ := Build(src)
	if line(ls, "Checks") != "" {
		t.Fatalf("a blocked reader's line shows")
	}
	for _, label := range []string{"Server", "Setup", "Queue", "Update"} {
		if line(ls, label) == "" {
			t.Errorf("%s dropped with the slow reader", label)
		}
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Build waited %v for a blocked reader", el)
	}
}

func TestPanelReaderPanicDropsOnlyItsLine(t *testing.T) {
	src := fixture(t)
	saved := readFile
	t.Cleanup(func() { readFile = saved })
	readFile = func(path string, limit int64) ([]byte, error) {
		if filepath.Base(path) == "state.json" {
			panic("boom")
		}
		return saved(path, limit)
	}
	ls, _ := Build(src)
	if line(ls, "Update") != "" || line(ls, "Setup") == "" {
		t.Fatalf("got %q", texts(ls))
	}
}

func TestPanelLogoKindAndMask(t *testing.T) {
	src := fixture(t)
	write(t, src.Dirs.ConfigFile(), "schema_version = 1\n[server]\nmode = \"remote\"\nurl = \"https://nas.example\"\n[terminal]\nlogo = \"braille\"\n")
	src.Mask = func(s string) string { return strings.ReplaceAll(s, "nas.example", "[masked]") }
	ls, kind := Build(src)
	if kind != "braille" {
		t.Fatalf("kind %q, want braille", kind)
	}
	if j := strings.Join(texts(ls), "\n"); strings.Contains(j, "nas.example") || !strings.Contains(j, "client of [masked]") {
		t.Fatalf("mask not applied:\n%s", j)
	}
	write(t, src.Dirs.ConfigFile(), "schema_version = 1\n[terminal]\nlogo = 7\n")
	ls, kind = Build(src)
	if kind != "blocks" || line(ls, "Setup") != "Setup    host · docker" {
		t.Fatalf("logo = 7: kind %q, Setup %q", kind, line(ls, "Setup"))
	}
}

func TestPanelASCII(t *testing.T) {
	src := fixture(t)
	src.ASCII = true
	write(t, src.Dirs.ConfigFile(), "schema_version = 1\n[server]\nmode = \"remote\"\nurl = \"https://café.example\"\n")
	ls, _ := Build(src)
	for _, s := range texts(ls) {
		for _, r := range s {
			if r >= 0x80 {
				t.Fatalf("%q holds %q", s, r)
			}
		}
	}
	if got := line(ls, "Update"); got != "Update   0.1.35-m7 available - backsight update apply" {
		t.Fatalf("got %q", got)
	}
}

// state.json keeps every tombstone and reach.json every waiting session:
// both grow past 64 KiB on a real install, and their lines must stay.
func TestPanelLargeStateAndReachKeepTheirLines(t *testing.T) {
	src := fixture(t)
	tomb := `"` + strings.Repeat("t", 200<<10) + `"`
	write(t, src.Dirs.StateFile(), `{"tombstones":[`+tomb+`],"update_check":{"latest":"0.1.35-m7"}}`)
	ids := make([]string, 3000)
	for i := range ids {
		ids[i] = `"0f8c2d4e-1111-2222-3333-444455556666"`
	}
	write(t, filepath.Join(src.Dirs.State, "reach.json"), `{"last_ok":"2026-11-10T11:58:00Z","sessions_waiting":3000,"waiting_sessions":[`+strings.Join(ids, ",")+`]}`)
	ls, _ := Build(src)
	for _, label := range []string{"Update", "Server", "Queue"} {
		if line(ls, label) == "" {
			t.Errorf("%s dropped with a large file", label)
		}
	}
}

// A mismatch recorded while a client is shown only in mode remote, like
// stats' Server line: a host that left the client role keeps none.
func TestPanelMismatchOnlyOnAClient(t *testing.T) {
	src := fixture(t)
	write(t, src.Dirs.StateFile(), `{"remote_server":{"id":"a","mismatch":"b"}}`)
	ls, _ := Build(src)
	if got := line(ls, "Server"); got != "Server   answering · last answer 2m ago" {
		t.Fatalf("a local host shows %q", got)
	}
}
