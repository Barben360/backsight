package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/supervise"
	"github.com/Barben360/backsight/internal/testtool"
)

const taskTestSID = "S-1-5-21-1111111111-2222222222-3333333333-1001"

// withSID makes the task's principal taskTestSID.
func withSID(t *testing.T) {
	t.Helper()
	old := userSID
	userSID = func() (string, error) { return taskTestSID, nil }
	t.Cleanup(func() { userSID = old })
}

// windowsUser sets the user environment a Windows tool gets (0080's
// list), so that supervise.json is the same on every test host.
func windowsUser(t *testing.T, profile string) {
	t.Helper()
	for k, v := range map[string]string{"SYSTEMROOT": `C:\Windows`, "SystemDrive": "C:", "USERPROFILE": profile,
		"LOCALAPPDATA": profile + `\AppData\Local`, "APPDATA": profile + `\AppData\Roaming`, "COMSPEC": `C:\Windows\system32\cmd.exe`} {
		t.Setenv(k, v)
	}
}

// windowsNative is a native runtime laid out for Windows under data and
// state, BACKSIGHT_HOME carried when home is set: what the task renders
// (no file is written: the paths are Windows' on any host).
func windowsNative(t *testing.T, home, data, state string) Native {
	t.Helper()
	d := paths.Dirs{Data: data, State: state}
	cfg := config.Default(d, `C:\Users\u`)
	cfg.Server.Runtime, cfg.Server.Project = "native", "bs"
	n := Native{Cfg: cfg, Dirs: d, GOOS: "windows", Env: map[string]string{}}
	if home != "" {
		n.Env["BACKSIGHT_HOME"] = home
	}
	return n.WithVersion("1.1.0-0123456789ab").(Native)
}

// decodeUTF16 reads a task.xml back, checking its byte order mark.
func decodeUTF16(t *testing.T, raw []byte) string {
	t.Helper()
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xFE || len(raw)%2 != 0 {
		t.Fatalf("task.xml is not UTF-16LE with a byte order mark: % x", raw[:min(len(raw), 4)])
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 2; i < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

// The default install's files, byte for byte: supervise.json runs the
// copied postgres on loopback with fsync on and the venv's API, and
// task.xml is UTF-16LE (what schtasks /XML reads) running the shim's
// server supervise under conhost --headless.
func TestTaskXMLGolden(t *testing.T) {
	withSID(t)
	windowsUser(t, `C:\Users\u`)
	n := windowsNative(t, "", `C:\Users\u\AppData\Local\backsight`, `C:\Users\u\AppData\Local\backsight\state`)
	f, err := taskHost{n: n}.Files(8890, n.Version())
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 2 {
		t.Errorf("files %v, want supervise.json and task.xml", slices.Sorted(func(y func(string) bool) {
			for k := range f {
				y(k)
			}
		}))
	}
	golden(t, "task-golden/default.xml", f["task.xml"])
	golden(t, "task-golden/supervise.json", f["supervise.json"])
	doc := decodeUTF16(t, f["task.xml"])
	for _, want := range []string{"<UserId>" + taskTestSID + "</UserId>", "<LogonType>InteractiveToken</LogonType>",
		"<Command>%SystemRoot%\\System32\\conhost.exe</Command>",
		`<Arguments>--headless C:\Users\u\AppData\Local\backsight\bin\current.exe server supervise</Arguments>`} {
		if !strings.Contains(doc, want) {
			t.Errorf("task.xml lacks %s:\n%s", want, doc)
		}
	}
	var cfg supervise.Config
	if err := json.Unmarshal(f["supervise.json"], &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Postgres[0] != `C:\Users\u\AppData\Local\backsight\server\native\pg\18.1.0\bin\postgres.exe` ||
		!slices.Contains(cfg.Postgres, "fsync=on") || !slices.Contains(cfg.Postgres, "listen_addresses=127.0.0.1") {
		t.Errorf("postgres %q", cfg.Postgres)
	}
	if want := `C:\Users\u\AppData\Local\backsight\server\native\venv\1.1.0-0123456789ab\Scripts\hindsight-api.exe`; !reflect.DeepEqual(cfg.API, []string{want}) {
		t.Errorf("api %q, want %s", cfg.API, want)
	}
}

// A profile path with a space, & and non-ASCII, and BACKSIGHT_HOME, reach
// task.xml escaped and quoted (CommandLineToArgv's round trip:
// native_task_windows_test.go).
func TestTaskXMLOddPathGolden(t *testing.T) {
	withSID(t)
	windowsUser(t, `C:\Users\Jé & Co`)
	home := `C:\Users\Jé & Co\AppData\Local\backsight home`
	n := windowsNative(t, home, home+`\data`, home+`\state`)
	f, err := taskHost{n: n}.Files(8890, n.Version())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "task-golden/odd-path.xml", f["task.xml"])
	want := `<Arguments>--headless "C:\Users\Jé &amp; Co\AppData\Local\backsight home\data\bin\current.exe" server supervise ` +
		`--backsight-home "C:\Users\Jé &amp; Co\AppData\Local\backsight home"</Arguments>`
	if doc := decodeUTF16(t, f["task.xml"]); !strings.Contains(doc, want) {
		t.Errorf("task.xml lacks\n%s\n%s", want, doc)
	}
}

// A % in the shim's or BACKSIGHT_HOME's path is refused (Task Scheduler
// would expand it): no task.xml for another path.
func TestTaskFilesRefuseAPercent(t *testing.T) {
	withSID(t)
	windowsUser(t, `C:\Users\u`)
	for _, home := range []string{`C:\Users\a%USERNAME%b`, `C:\100%`} {
		n := windowsNative(t, home, home+`\data`, home+`\state`)
		if f, err := (taskHost{n: n}).Files(8890, n.Version()); err == nil || !strings.Contains(err.Error(), "BACKSIGHT_HOME") {
			t.Errorf("%s: %v (%d files), want a refusal naming BACKSIGHT_HOME", home, err, len(f))
		}
	}
}

// Neither supervise.json nor task.xml holds a secret, nor a proxy's
// password: the API's secrets come from server.env.
func TestSuperviseJSONHasNoSecret(t *testing.T) {
	withSID(t)
	windowsUser(t, `C:\Users\u`)
	t.Setenv("HTTPS_PROXY", "http://me:proxypassword@proxy:3128")
	withFreePort(t, 8890)
	n := windowsNative(t, `C:\bs`, `C:\bs\data`, `C:\bs\state`)
	f, err := n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.Env), "fakedbpassword") {
		t.Fatal("server.env lacks the database password: the check below would prove nothing")
	}
	files := map[string]string{"supervise.json": string(f.Units["supervise.json"]), "task.xml": decodeUTF16(t, f.Units["task.xml"])}
	for name, content := range files {
		if content == "" {
			t.Fatalf("%s not rendered", name)
		}
		for _, s := range []string{secrets.APIKey, secrets.UIKey, secrets.DBPassword, secrets.LLMAPIKey, "proxypassword"} {
			if strings.Contains(content, s) {
				t.Errorf("%s holds the secret %q:\n%s", name, s, content)
			}
		}
	}
	var cfg supervise.Config
	if err := json.Unmarshal(f.Units["supervise.json"], &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.EnvFile != `C:\bs\data\server\native\server.env` {
		t.Errorf("env_file %q: the API's secrets come from server.env", cfg.EnvFile)
	}
}

// events is what the task host did, in order: schtasks calls, pipe
// requests, detached starts.
type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, s)
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.list...)
}

func (e *events) index(s string) int { return slices.Index(e.all(), s) }

// fakeTask is Task Scheduler and the supervisor's pipe for the task
// host's tests.
type fakeTask struct {
	ev         *events
	mu         sync.Mutex
	registered bool
	up         bool
	st         supervise.Status
	runStarts  bool // /Run starts the supervisor (a console logon)
	runFails   bool // /Run fails
	queryFails int  // the next /Query calls failing for another reason than a missing task
	stopPolls  int  // status answers after a stop before the pipe goes
	notes      []string
	detached   [][]string
}

func (f *fakeTask) sys(c []string) ([]byte, error) {
	f.ev.add(strings.Join(c, " "))
	f.mu.Lock()
	defer f.mu.Unlock()
	notFound := func() ([]byte, error) {
		return []byte("ERROR: The system cannot find the file specified."), errors.New("exit status 1")
	}
	switch {
	case slices.Contains(c, "/Query"):
		if f.queryFails > 0 {
			f.queryFails--
			return []byte("ERROR: The RPC server is unavailable."), errors.New("exit status 1")
		}
		if !f.registered {
			return notFound()
		}
	case slices.Contains(c, "/Create"):
		f.registered = true
	case !f.registered:
		return notFound()
	case slices.Contains(c, "/Delete"):
		f.registered = false
	case slices.Contains(c, "/Run"):
		if f.runFails {
			return []byte("ERROR: Access is denied."), errors.New("exit status 1")
		}
		if f.runStarts {
			f.start()
		}
	}
	return nil, nil
}

func (f *fakeTask) start() {
	f.up = true
	f.st = supervise.Status{Proto: supervise.Proto, PID: 4242, DB: "running", API: "running"}
}

func (f *fakeTask) ask(_ context.Context, project, op string) (supervise.Status, error) {
	f.ev.add("pipe " + op)
	f.mu.Lock()
	defer f.mu.Unlock()
	if project != "bs" {
		return supervise.Status{}, errors.New("asked project " + project)
	}
	if !f.up {
		return supervise.Status{}, supervise.ErrNotRunning
	}
	st := f.st
	switch op {
	case "stop":
		f.st.DB, f.st.API = "stopped", "stopped"
		if f.stopPolls == 0 {
			f.up = false
		}
	case "status":
		if f.st.DB == "stopped" && f.st.API == "stopped" {
			if f.stopPolls--; f.stopPolls <= 0 {
				f.up = false
			}
		}
	case "api-stop":
		f.st.API = "stopped"
		st = f.st
	case "api-start":
		f.st.API = "running"
		st = f.st
	}
	return st, nil
}

// taskFixture is the task host on this host's own paths (files written
// for real), Task Scheduler and the pipe faked; waits are short.
func taskFixture(t *testing.T) (taskHost, *fakeTask) {
	t.Helper()
	withSID(t)
	base := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(base, "data"), Config: filepath.Join(base, "config"), State: filepath.Join(base, "state")}
	cfg := config.Default(d, base)
	cfg.Server.Runtime, cfg.Server.Project = "native", "bs"
	f := &fakeTask{ev: &events{}}
	n := Native{Cfg: cfg, Dirs: d, Env: map[string]string{"BACKSIGHT_HOME": base}, Sys: &fakeSys{Out: f.sys},
		Note: func(s string) { f.notes = append(f.notes, s) }}
	if err := os.MkdirAll(n.Root(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(n.Root(), "task.xml"), []byte("<Task/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := []any{superviseAsk, startSupervisor, taskRunWait, superviseStopWait, supervisePoll}
	superviseAsk = f.ask
	startSupervisor = func(args ...string) error {
		f.ev.add("detached " + strings.Join(args, " "))
		f.mu.Lock()
		defer f.mu.Unlock()
		f.detached = append(f.detached, args)
		f.start()
		return nil
	}
	taskRunWait, superviseStopWait, supervisePoll = 300*time.Millisecond, time.Second, 5*time.Millisecond
	t.Cleanup(func() {
		superviseAsk = old[0].(func(context.Context, string, string) (supervise.Status, error))
		startSupervisor = old[1].(func(...string) error)
		taskRunWait, superviseStopWait, supervisePoll = old[2].(time.Duration), old[3].(time.Duration), old[4].(time.Duration)
	})
	h := taskHost{n: n}
	withHost(t, h) // n.pending() reads the task host's markers
	return h, f
}

// A first start registers the task from task.xml, enables it and runs it;
// the pipe answering is the supervisor running. A second start finds both
// and changes nothing.
func TestStartRegistersEnablesAndRunsTheTask(t *testing.T) {
	h, f := taskFixture(t)
	f.runStarts = true
	ctx := context.Background()
	if err := h.StartDB(ctx, false); err != nil {
		t.Fatal(err)
	}
	xmlPath := filepath.Join(h.n.Root(), "task.xml")
	want := []string{`schtasks /Query /TN \bs\server`, `schtasks /Create /F /XML ` + xmlPath + ` /TN \bs\server`,
		`schtasks /Change /TN \bs\server /ENABLE`, "pipe status", `schtasks /Run /TN \bs\server`, "pipe status"}
	if got := f.ev.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("StartDB did\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(f.notes) != 0 || len(f.detached) != 0 {
		t.Errorf("the task ran, yet notes %q and detached starts %q", f.notes, f.detached)
	}
	if err := h.StartAPI(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.ev = &events{}
	if err := h.StartDB(ctx, false); err != nil {
		t.Fatal(err)
	}
	want = []string{`schtasks /Query /TN \bs\server`, `schtasks /Change /TN \bs\server /ENABLE`, "pipe status"}
	if got := f.ev.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("StartDB with the task registered and the supervisor running did\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// task.xml changed under a registered task (restart-pending's task
// marker): registered again. A supervisor restart stops the running one
// and waits until its pipe is gone before starting another.
func TestStartRegistersAgainAndRestarts(t *testing.T) {
	h, f := taskFixture(t)
	f.registered, f.runStarts = true, true
	f.start()
	f.stopPolls = 3
	if err := os.WriteFile(h.n.pendingFile(), []byte("supervisor\ntask\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withHost(t, h)
	if err := h.StartDB(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	got := f.ev.all()
	if !slices.Contains(got, `schtasks /Create /F /XML `+filepath.Join(h.n.Root(), "task.xml")+` /TN \bs\server`) {
		t.Errorf("task.xml marked changed, the task not registered again:\n%s", strings.Join(got, "\n"))
	}
	stop, run := slices.Index(got, "pipe stop"), slices.Index(got, `schtasks /Run /TN \bs\server`)
	if stop < 0 || run < stop {
		t.Fatalf("no stop before the new start:\n%s", strings.Join(got, "\n"))
	}
	if statuses := strings.Count(strings.Join(got[stop:run], "\n"), "pipe status"); statuses < 3 {
		t.Errorf("started again after %d status polls: the old supervisor still answered (3 polls)", statuses)
	}
}

// With no console logon the task's /Run starts nothing: the supervisor is
// started detached under conhost --headless (the task's own action), and
// the user is told it lasts until logoff.
func TestStartWithoutALogonStartsDetached(t *testing.T) {
	h, f := taskFixture(t)
	if err := h.StartDB(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	run, det := f.ev.index(`schtasks /Run /TN \bs\server`), slices.IndexFunc(f.ev.all(), func(e string) bool { return strings.HasPrefix(e, "detached ") })
	if run < 0 || det < run {
		t.Fatalf("the task was not run before the detached start:\n%s", strings.Join(f.ev.all(), "\n"))
	}
	want := scheduler.HeadlessArgs(h.n.shim(), "server", "supervise", "--backsight-home", h.n.Env["BACKSIGHT_HOME"])
	if len(f.detached) != 1 || !slices.Equal(f.detached[0], want) {
		t.Errorf("detached starts %q, want one of %q", f.detached, want)
	}
	if !slices.Equal(f.notes, []string{NoLogonNote}) {
		t.Errorf("notes %q, want the no-logon note", f.notes)
	}
}

// A detached supervisor that never answers fails the start, naming its
// log; one that cannot start fails it with why.
func TestStartFailsWhenTheDetachedSupervisorNeverAnswers(t *testing.T) {
	h, f := taskFixture(t)
	startSupervisor = func(args ...string) error { f.ev.add("detached"); return nil }
	err := h.StartDB(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "did not answer") || !strings.Contains(err.Error(), "bs-supervisor.log") {
		t.Errorf("StartDB = %v, want the supervisor's silence and its log", err)
	}
	if len(f.notes) != 0 {
		t.Errorf("notes %q on a failed start", f.notes)
	}
	startSupervisor = func(...string) error { return errors.New("access denied") }
	if err := h.StartDB(context.Background(), false); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("StartDB = %v, want the start's error", err)
	}
	f.runFails = true
	if err := h.StartDB(context.Background(), false); err == nil || !strings.Contains(err.Error(), "the task did not run") ||
		!strings.Contains(err.Error(), "Access is denied") || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("StartDB = %v, want both the /Run's error and the detached start's", err)
	}
}

// The database alone arms nothing: a running supervisor stops its API;
// else one starts detached with --no-api, the task never run.
func TestStartDatabaseAloneArmsNothing(t *testing.T) {
	h, f := taskFixture(t)
	ctx := context.Background()
	if err := h.startDBAlone(ctx); err != nil {
		t.Fatal(err)
	}
	want := scheduler.HeadlessArgs(h.n.shim(), "server", "supervise", "--no-api", "--backsight-home", h.n.Env["BACKSIGHT_HOME"])
	if len(f.detached) != 1 || !slices.Equal(f.detached[0], want) {
		t.Errorf("detached starts %q, want %q", f.detached, want)
	}
	for _, e := range f.ev.all() {
		if strings.HasPrefix(e, "schtasks") {
			t.Errorf("the database alone ran %s", e)
		}
	}
	f.ev = &events{}
	if err := h.startDBAlone(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.ev.all(); !reflect.DeepEqual(got, []string{"pipe status", "pipe api-stop"}) || f.st.API != "stopped" {
		t.Errorf("with a supervisor running: %q, API %s; want its API stopped", got, f.st.API)
	}
}

// StartAPI asks the running supervisor; a restart stops the API first
// (server.env changed). No supervisor: an error, nothing started.
func TestStartAPIThroughThePipe(t *testing.T) {
	h, f := taskFixture(t)
	ctx := context.Background()
	if err := h.StartAPI(ctx, false); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("StartAPI without a supervisor = %v", err)
	}
	f.start()
	f.ev = &events{}
	if err := h.StartAPI(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := f.ev.all(); !reflect.DeepEqual(got, []string{"pipe status", "pipe api-stop", "pipe api-start"}) {
		t.Errorf("StartAPI restart did %q", got)
	}
	f.ev = &events{}
	if err := h.StartAPI(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := f.ev.all(); !reflect.DeepEqual(got, []string{"pipe status", "pipe api-start"}) {
		t.Errorf("StartAPI did %q", got)
	}
	// An API not started yet, or one the supervisor StartDB just restarted
	// started, already reads the new server.env: no second restart.
	f.st.API = "starting"
	f.ev = &events{}
	if err := h.StartAPI(ctx, true); err != nil || !reflect.DeepEqual(f.ev.all(), []string{"pipe status", "pipe api-start"}) {
		t.Errorf("StartAPI restart of a starting API: %v %q", err, f.ev.all())
	}
	if err := os.WriteFile(h.n.pendingFile(), []byte("supervisor\napi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.ev = &events{}
	if err := h.StartAPI(ctx, true); err != nil || !reflect.DeepEqual(f.ev.all(), []string{"pipe status", "pipe api-start"}) {
		t.Errorf("StartAPI restart after the supervisor's restart: %v %q", err, f.ev.all())
	}
	f.ev = &events{}
	if err := h.StopAPI(ctx); err != nil || !reflect.DeepEqual(f.ev.all(), []string{"pipe api-stop"}) {
		t.Errorf("StopAPI: %v %q", err, f.ev.all())
	}
}

// Stop disables the task before it stops the supervisor (the 5-minute
// trigger would start it again), and returns once the pipe is gone.
func TestStopDisablesTheTaskFirst(t *testing.T) {
	h, f := taskFixture(t)
	f.registered = true
	f.start()
	f.stopPolls = 3
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.ev.all()
	disable, stop := slices.Index(got, `schtasks /Change /TN \bs\server /DISABLE`), slices.Index(got, "pipe stop")
	if disable < 0 || stop < 0 || disable > stop {
		t.Fatalf("Stop did\n%s\nwant /Change /DISABLE before the pipe's stop", strings.Join(got, "\n"))
	}
	if f.up || got[len(got)-1] != "pipe status" {
		t.Errorf("Stop returned while the supervisor still answered:\n%s", strings.Join(got, "\n"))
	}
	if h.StopCmd() != "backsight server stop" {
		t.Errorf("StopCmd = %q", h.StopCmd())
	}
}

// A supervisor still answering after the stop's bound fails Stop, naming
// its log; nothing registered and nothing running is no error.
func TestStopEdges(t *testing.T) {
	h, f := taskFixture(t)
	f.registered = true
	f.start()
	f.stopPolls = 1 << 30
	err := h.Stop(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still runs") || !strings.Contains(err.Error(), "bs-supervisor.log") {
		t.Errorf("Stop of a supervisor that never stops = %v", err)
	}
	h2, _ := taskFixture(t)
	if err := h2.Stop(context.Background()); err != nil {
		t.Errorf("Stop with nothing registered or running: %v", err)
	}
	if err := h2.Remove(context.Background()); err != nil {
		t.Errorf("Remove with nothing registered or running: %v", err)
	}
	// A /Change failing on a registered task is Stop's failure.
	h3, f3 := taskFixture(t)
	f3.registered = true
	h3.n.Sys = &fakeSys{Out: func(c []string) ([]byte, error) {
		if slices.Contains(c, "/Change") {
			return []byte("ERROR: Access is denied."), errors.New("exit status 1")
		}
		return f3.sys(c)
	}}
	if err := h3.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "Access is denied") {
		t.Errorf("Stop with /DISABLE refused on a registered task = %v", err)
	}
}

// A /Query failing for another reason than a missing task (the service
// busy) never skips the /DISABLE: the 5-minute trigger would start the
// server again.
func TestStopDisablesThroughAFailingQuery(t *testing.T) {
	h, f := taskFixture(t)
	f.registered = true
	f.queryFails = 5
	f.start()
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ev.index(`schtasks /Change /TN \bs\server /DISABLE`) < 0 {
		t.Errorf("a failing /Query skipped the /DISABLE:\n%s", strings.Join(f.ev.all(), "\n"))
	}
}

// Remove stops, then unregisters.
func TestRemoveUnregistersAfterTheStop(t *testing.T) {
	h, f := taskFixture(t)
	f.registered = true
	f.start()
	if err := h.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.ev.all()
	stop, del := slices.Index(got, "pipe stop"), slices.Index(got, `schtasks /Delete /F /TN \bs\server`)
	if stop < 0 || del < stop || f.registered {
		t.Errorf("Remove did\n%s\nwant the stop, then /Delete", strings.Join(got, "\n"))
	}
}

func TestStatesFromThePipe(t *testing.T) {
	h, f := taskFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		db, api string
		want    map[string]string
		status  []string
	}{
		{"running", "running", map[string]string{"db": "active", "hindsight": "active"}, []string{"running", "running"}},
		{"starting", "starting", map[string]string{"db": "activating", "hindsight": "activating"}, []string{"starting", "starting"}},
		{"restarting", "restarting", map[string]string{"db": "activating/auto-restart", "hindsight": "activating/auto-restart"}, []string{"restarting", "restarting"}},
		{"refused", "stopped", map[string]string{"db": "failed", "hindsight": "inactive"}, []string{"exited", "stopped"}},
		{"running", "stopped", map[string]string{"db": "active", "hindsight": "inactive"}, []string{"running", "stopped"}},
	} {
		f.up, f.st = true, supervise.Status{DB: c.db, API: c.api}
		got, err := h.States(ctx)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("db %s, api %s: %v %v, want %v", c.db, c.api, got, err, c.want)
		}
		list, err := h.n.Status(ctx)
		if err != nil || len(list) != 2 || list[0].State != c.status[0] || list[1].State != c.status[1] {
			t.Errorf("db %s, api %s: Status %+v %v, want %v", c.db, c.api, list, err, c.status)
		}
	}
	f.up = false
	if got, err := h.States(ctx); err != nil || !reflect.DeepEqual(got, map[string]string{"db": "inactive", "hindsight": "inactive"}) {
		t.Errorf("no supervisor: %v %v", got, err)
	}
	superviseAsk = func(context.Context, string, string) (supervise.Status, error) {
		return supervise.Status{}, errors.New("pipe trouble")
	}
	if got, err := h.States(ctx); err == nil {
		t.Errorf("a failing pipe: %v, want its error", got)
	}
	if err := os.Remove(filepath.Join(h.n.Root(), "task.xml")); err != nil {
		t.Fatal(err)
	}
	if got, err := h.States(ctx); err != nil || len(got) != 0 {
		t.Errorf("never rendered: %v %v, want no state", got, err)
	}
}

func TestDeployedReadsSuperviseJSON(t *testing.T) {
	h, _ := taskFixture(t)
	withHost(t, h)
	n := h.n
	if v, err := n.Deployed(); err != nil || v != "" {
		t.Errorf("no supervise.json: %q %v", v, err)
	}
	f, err := h.Files(8890, "1.0.9-00000000abcd")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(n.Root(), "supervise.json"), f["supervise.json"], 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := n.Deployed(); err != nil || v != "1.0.9-00000000abcd" {
		t.Errorf("Deployed = %q %v, want supervise.json's venv", v, err)
	}
	if err := os.WriteFile(filepath.Join(n.Root(), "supervise.json"), []byte(`{"project":"bs"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := n.Deployed(); err == nil {
		t.Errorf("a supervise.json naming no venv: %q, want an error", v)
	}
}

func TestTaskLogsAreTheLogFilesTails(t *testing.T) {
	h, _ := taskFixture(t)
	dir := h.n.LogDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"bs-db.log.1": "d1\nd2\n", "bs-db.log": "d3\nd4\n", "bs-api.log": "a1\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if got, err := h.Logs(ctx, "db", 3); err != nil || got != "d2\nd3\nd4\n" {
		t.Errorf("db logs %q %v", got, err)
	}
	if got, err := h.Logs(ctx, "hindsight", 10); err != nil || got != "a1\n" {
		t.Errorf("api logs %q %v", got, err)
	}
	if _, err := h.Logs(ctx, "supervisor", 10); err == nil {
		t.Error("an unknown service has logs")
	}
}

func TestTaskEngineIsTaskScheduler(t *testing.T) {
	h, f := taskFixture(t)
	if _, err := h.Engine(context.Background()); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("Engine with no task = %v", err)
	}
	f.registered = true
	if e, err := h.Engine(context.Background()); err != nil || e.Version != "Task Scheduler" {
		t.Errorf("Engine = %+v %v", e, err)
	}
}

// Ruling 2 of Task 9: Delete gates on the host's files, not the systemd
// units: on Windows it stops the supervisor and disables the task while
// everything still exists, removes the files through the config editor,
// unregisters the task, then removes Root.
func TestDeleteStopsAndUnregistersTheTaskFirst(t *testing.T) {
	h, f := taskFixture(t)
	withHost(t, h)
	n := h.n
	files, err := h.Files(8890, n.Version())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Write(Files{Env: []byte("K=v\n"), Units: files}); err != nil {
		t.Fatal(err)
	}
	f.registered = true
	f.start()
	var whileStopping, stopped bool
	superviseAsk = func(ctx context.Context, project, op string) (supervise.Status, error) {
		if op == "stop" && !stopped {
			stopped = true
			whileStopping = exists(n.Root()) && exists(filepath.Join(n.Root(), "supervise.json")) && exists(filepath.Join(n.Root(), "task.xml"))
		}
		return f.ask(ctx, project, op)
	}
	if err := n.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.ev.all()
	disable, stop, del := slices.Index(got, `schtasks /Change /TN \bs\server /DISABLE`), slices.Index(got, "pipe stop"), slices.Index(got, `schtasks /Delete /F /TN \bs\server`)
	if disable < 0 || stop < disable || del < stop {
		t.Errorf("Delete did\n%s\nwant disable, stop, then /Delete", strings.Join(got, "\n"))
	}
	if !whileStopping {
		t.Error("the supervisor was not stopped while its files and Root still existed")
	}
	if exists(n.Root()) || f.registered {
		t.Errorf("Root stays (%v) or the task stays registered (%v)", exists(n.Root()), f.registered)
	}
	journal, err := os.ReadFile(n.Dirs.JournalFile())
	if err != nil || !strings.Contains(string(journal), "supervise.json") {
		t.Errorf("the files were not removed through the config editor: %v", err)
	}
}

// A retried delete (the files gone after a failed /Delete), or files
// removed by hand: the task is still disabled, the supervisor stopped and
// the task unregistered before Root goes, never orphaned.
func TestDeleteWithoutTheFilesStillStopsAndUnregisters(t *testing.T) {
	h, f := taskFixture(t)
	n := h.n
	if err := os.Remove(filepath.Join(n.Root(), "task.xml")); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(n.Root(), "home", "data"), 0o600)
	f.registered = true
	f.start()
	var rootWhileStopping bool
	superviseAsk = func(ctx context.Context, project, op string) (supervise.Status, error) {
		if op == "stop" && f.up {
			rootWhileStopping = exists(n.Root())
		}
		return f.ask(ctx, project, op)
	}
	if err := n.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.ev.all()
	disable, stop, del := slices.Index(got, `schtasks /Change /TN \bs\server /DISABLE`), slices.Index(got, "pipe stop"), slices.Index(got, `schtasks /Delete /F /TN \bs\server`)
	if disable < 0 || stop < disable || del < stop {
		t.Errorf("Delete without the files did\n%s\nwant disable, stop, then /Delete", strings.Join(got, "\n"))
	}
	if !rootWhileStopping || f.up || f.registered || exists(n.Root()) {
		t.Errorf("Root while stopping %v, supervisor up %v, task registered %v, Root after %v", rootWhileStopping, f.up, f.registered, exists(n.Root()))
	}
}

// Ruling 2 of Task 9: Write keys restart-pending on the task host's
// files: server.env restarts the API, supervise.json the supervisor,
// task.xml registers the task again; Start acts on each and clears the
// marker once the API answered.
func TestTaskRestartPending(t *testing.T) {
	testtool.SkipWindows(t, "Task 10: Start's guard scans /proc for strays until then")
	n, _, _ := nativeFixture(t, secrets)
	n.GOOS = ""
	h, f := taskFixture(t)
	h.n.Cfg, h.n.Dirs, h.n.Tools, h.n.Proc, h.n.Env = n.Cfg, n.Dirs, n.Tools, n.Proc, n.Env
	h.n.Cfg.Server.Project = "bs" // fakeTask's
	n = h.n
	withHost(t, h)
	completeVenv(t, n, n.Version())
	write := func() Changes {
		t.Helper()
		files, err := n.Render(secrets, "box")
		if err != nil {
			t.Fatal(err)
		}
		ch, err := n.Write(files)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	write()
	recordInstance(t, n, "7702339718603615110")
	if exists(n.pendingFile()) {
		t.Fatal("a first install leaves restart-pending")
	}
	f.registered, f.runStarts = true, true
	f.start()
	pending := func() string { raw, _ := os.ReadFile(n.pendingFile()); return string(raw) }
	ctx := context.Background()

	n.Cfg.LLM.Model = "deepseek-other" // server.env alone
	write()
	if got := pending(); got != "api\n" {
		t.Errorf("server.env changed: restart-pending %q, want api", got)
	}
	f.ev = &events{}
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.ev.all(); slices.Contains(got, "pipe stop") || !slices.Contains(got, "pipe api-stop") || !slices.Contains(got, "pipe api-start") {
		t.Errorf("Start after a server.env change did\n%s\nwant the API alone restarted", strings.Join(got, "\n"))
	}
	if exists(n.pendingFile()) {
		t.Error("restart-pending stays after the restart")
	}

	n.Env["BACKSIGHT_HOME"] += "-moved" // the task's action alone
	write()
	if got := pending(); got != "task\n" {
		t.Errorf("task.xml changed: restart-pending %q, want task", got)
	}
	f.ev = &events{}
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.ev.all(); !slices.ContainsFunc(got, func(e string) bool { return strings.Contains(e, "/Create") }) || slices.Contains(got, "pipe stop") {
		t.Errorf("Start after a task.xml change did\n%s\nwant the task registered again, the supervisor kept", strings.Join(got, "\n"))
	}

	n = n.WithVersion("1.1.0-ffffffffffff").(Native) // supervise.json: another venv
	h.n = n
	withHost(t, h)
	completeVenv(t, n, n.Version())
	write()
	if got := pending(); got != "supervisor\n" {
		t.Errorf("supervise.json changed: restart-pending %q, want supervisor", got)
	}
	f.ev = &events{}
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.ev.all(); !slices.Contains(got, "pipe stop") || !slices.Contains(got, `schtasks /Run /TN \bs\server`) || slices.Contains(got, "pipe api-stop") {
		t.Errorf("Start after a supervise.json change did\n%s\nwant the supervisor stopped and started again, its new API not restarted", strings.Join(got, "\n"))
	}
	if exists(n.pendingFile()) {
		t.Error("restart-pending stays after the restart")
	}
}
