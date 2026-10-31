package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/testtool"
)

// superviseEnv, set to a supervise.json path, makes the test binary a
// supervisor (TestKillingTheSupervisorKillsItsChildren's subprocess).
const superviseEnv = "BACKSIGHT_SUPERVISE_TEST_CONFIG"

func TestMain(m *testing.M) {
	testtool.Main()
	if path := os.Getenv(superviseEnv); path != "" {
		raw, err := os.ReadFile(path)
		var cfg Config
		if err == nil {
			err = json.Unmarshal(raw, &cfg)
		}
		if err == nil {
			err = Run(context.Background(), cfg, Options{Version: "test"})
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "supervisor:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	beforeTests()
	os.Exit(m.Run())
}

// beforeTests prepares the process for the tests (Windows: a console, as
// conhost gives the product's supervisor).
var beforeTests = func() {}

// fx is a project's supervise.json with testtool fakes for postgres,
// pg_isready, pg_ctl and the API, all files in dir.
type fx struct {
	t                     *testing.T
	dir                   string
	cfg                   Config
	pg, isReady, ctl, api string
}

// fakes are the scripts fx installs, {dir} standing for fx's directory;
// a field left "" gets the default.
type fakes struct {
	pg, isReady, api string
}

const envFileKey = "BACKSIGHT_TEST_FROM_ENVFILE"

// project is a name no other test, run or user of the VM shares.
func project(name string) string {
	return "bs-test-" + strconv.Itoa(os.Getpid()) + "-" + name
}

func newFx(t *testing.T, name string, f fakes) *fx {
	t.Helper()
	dir := t.TempDir()
	x := &fx{t: t, dir: dir}
	if f.pg == "" {
		// Ready at once, runs until pg_ctl stop writes pg-stop.
		f.pg = "pid " + x.file("pg.pids") + "\nwrite " + x.file("ready") + " up\nstdout postgres up\nenv " + envFileKey + " unset\nwaitfile " + x.file("pg-stop")
	}
	if f.isReady == "" {
		f.isReady = "nofile " + x.file("ready") + " 2"
	}
	if f.api == "" {
		f.api = "pid " + x.file("api.pids") + "\nenv " + envFileKey + " unset\nwaitfile " + x.file("never")
	}
	in := func(script string) string { return strings.ReplaceAll(script, "{dir}", dir) }
	x.pg = testtool.Write(t, dir, "postgres", testtool.Fake{Script: in(f.pg)})
	x.isReady = testtool.Write(t, dir, "pg_isready", testtool.Fake{Script: in(f.isReady)})
	x.ctl = testtool.Write(t, dir, "pg_ctl", testtool.Fake{Script: "write " + x.file("pg-stop") + " stop"})
	x.api = testtool.Write(t, dir, "hindsight-api", testtool.Fake{Script: in(f.api)})
	envFile := x.file("server.env")
	if err := os.WriteFile(envFile, []byte("# the API's\n"+envFileKey+"='from the env file'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	x.cfg = Config{
		Project:   project(name),
		DataDir:   x.file("db"),
		Postgres:  []string{x.pg, "-D", x.file("db"), "-c", "fsync=on"},
		PGIsReady: []string{x.isReady, "-h", "127.0.0.1"},
		PGCtlStop: []string{x.ctl, "stop", "-m", "fast", "-D", x.file("db")},
		API:       []string{x.api},
		Env:       []string{"BACKSIGHT_TEST_TOOL=1"},
		EnvFile:   envFile,
		LogDir:    x.file("logs"),
		Venv:      "1.1.0-abcdefabcdef",
	}
	cleanTransport(t, x.cfg.Project)
	return x
}

func (x *fx) file(name string) string { return filepath.Join(x.dir, name) }

func (x *fx) log(what string) string {
	raw, _ := os.ReadFile(filepath.Join(x.cfg.LogDir, x.cfg.Project+"-"+what+".log"))
	return string(raw)
}

// cleanTransport removes the unix stand-ins' files after the test; the
// first fixture of a project registers it, so it runs after every stop.
func cleanTransport(t *testing.T, project string) {
	if _, seen := cleaned.LoadOrStore(project, true); seen {
		return
	}
	t.Cleanup(func() {
		cleaned.Delete(project)
		dir := filepath.Join(os.TempDir(), "backsight-"+strconv.Itoa(os.Getuid()))
		_ = os.Remove(filepath.Join(dir, project+".lock"))
		_ = os.Remove(filepath.Join(dir, project+".sock"))
	})
}

var cleaned sync.Map

// fast shortens the supervisor's numbers for the test.
func fast(t *testing.T) {
	t.Helper()
	old := tm
	tm.backoffMin, tm.backoffMax = 20*time.Millisecond, 200*time.Millisecond
	tm.readyPoll, tm.readyWait = 10*time.Millisecond, 20*time.Second
	tm.apiStopWait, tm.dbStopWait = 3*time.Second, 10*time.Second
	tm.requestWait = 3 * time.Second
	t.Cleanup(func() { tm = old })
}

// start runs the supervisor in a goroutine; the test's end stops it.
// The channel gets Run's error once.
func (x *fx) start(opts Options) <-chan error {
	x.t.Helper()
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		err := Run(context.Background(), x.cfg, opts)
		close(finished)
		done <- err
	}()
	x.t.Cleanup(func() {
		_, _ = ask(x.cfg.Project, opStop)
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			x.t.Errorf("%s: the supervisor did not stop", x.cfg.Project)
		}
	})
	return done
}

func ask(project, op string) (Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Ask(ctx, project, op)
}

// waitFor polls cond for up to 15 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitStatus waits until the supervisor answers with db and api.
func waitStatus(t *testing.T, project, db, api string) Status {
	t.Helper()
	var st Status
	waitFor(t, fmt.Sprintf("%s: db %s, api %s", project, db, api), func() bool {
		var err error
		st, err = ask(project, opStatus)
		return err == nil && st.DB == db && st.API == api
	})
	return st
}

func readFile(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

func calls(t *testing.T, fake string) int { return len(testtool.Calls(t, fake)) }

func waitExit(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("Run did not return within %s", within)
		return nil
	}
}

func TestSuperviseStartsDBThenAPIAfterReady(t *testing.T) {
	fast(t)
	// postgres never writes ready: the test does.
	x := newFx(t, "ready", fakes{pg: "stdout postgres up\nenv " + envFileKey + " unset\nwaitfile {dir}/pg-stop"})
	x.start(Options{Version: "9.9.9"})
	waitFor(t, "pg_isready asked three times", func() bool { return calls(t, x.isReady) >= 3 })
	st, err := ask(x.cfg.Project, opStatus)
	if err != nil {
		t.Fatal(err)
	}
	if st.DB != stateStarting || st.API == stateRunning || calls(t, x.api) != 0 {
		t.Fatalf("before pg_isready answers: db %q api %q, api started %d times; want the database starting and no API", st.DB, st.API, calls(t, x.api))
	}
	if st.Proto != Proto || st.Version != "9.9.9" || st.PID != os.Getpid() || st.Venv != x.cfg.Venv {
		t.Errorf("status %+v: proto, version, pid or venv wrong", st)
	}
	if err := os.WriteFile(x.file("ready"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	waitFor(t, "the API's start recorded", func() bool { return calls(t, x.api) == 1 })
	if got := testtool.Calls(t, x.pg); !reflect.DeepEqual(got, [][]string{x.cfg.Postgres[1:]}) {
		t.Errorf("postgres argv %q, want %q", got, x.cfg.Postgres[1:])
	}
	// The API gets server.env's pairs; postgres only the tool environment.
	waitFor(t, "the API's output in its log", func() bool { return strings.Contains(x.log("api"), "from the env file") })
	if db := x.log("db"); !strings.Contains(db, "postgres up") || !strings.Contains(db, "unset") {
		t.Errorf("db log %q: want postgres' output, without server.env's pairs", db)
	}
}

// fakeAfter records the restart delays; the first fire of them fire at
// once, the others never.
type fakeAfter struct {
	mu     sync.Mutex
	delays []time.Duration
	fire   int
}

func (f *fakeAfter) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, d)
	c := make(chan time.Time, 1)
	if len(f.delays) <= f.fire {
		c <- time.Time{}
	}
	return c
}

func (f *fakeAfter) got() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration{}, f.delays...)
}

func TestChildExitRestartsWithBackoff(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		var b backoff
		var got []time.Duration
		for range 11 {
			got = append(got, b.next(time.Second))
		}
		s := time.Second
		want := []time.Duration{s, 2 * s, 4 * s, 8 * s, 16 * s, 32 * s, 64 * s, 128 * s, 256 * s, 300 * s, 300 * s}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("delays %v, want %v", got, want)
		}
		if d := b.next(59 * time.Second); d != 300*s {
			t.Errorf("after 59 s up: %v, want 300s (no reset before a minute)", d)
		}
		if d := b.next(60 * time.Second); d != s {
			t.Errorf("after 60 s up: %v, want 1s (reset)", d)
		}
	})
	t.Run("restarts", func(t *testing.T) {
		fast(t)
		fa := &fakeAfter{fire: 4}
		tm.after = fa.after
		tm.backoffMin, tm.backoffMax = time.Second, 5*time.Minute
		x := newFx(t, "backoff", fakes{pg: "stdout crashing\nexit 1"})
		x.start(Options{})
		waitFor(t, "five restart delays", func() bool { return len(fa.got()) >= 5 })
		s := time.Second
		if got, want := fa.got(), []time.Duration{s, 2 * s, 4 * s, 8 * s, 16 * s}; !reflect.DeepEqual(got, want) {
			t.Errorf("restart delays %v, want %v", got, want)
		}
		if n := calls(t, x.pg); n != 5 {
			t.Errorf("postgres started %d times, want 5 (the first and four restarts)", n)
		}
		st, err := ask(x.cfg.Project, opStatus)
		if err != nil || st.DB != stateRestarting || calls(t, x.api) != 0 {
			t.Errorf("status %+v (%v), api started %d times: want the database restarting and no API", st, err, calls(t, x.api))
		}
	})
	t.Run("reset after a minute up", func(t *testing.T) {
		fast(t)
		fa := &fakeAfter{fire: 3}
		tm.after = fa.after
		tm.backoffMin, tm.backoffMax = time.Second, 5*time.Minute
		// Every reading of the clock is 61 s after the previous one: each
		// run lasted over a minute.
		var clock atomic.Int64
		tm.now = func() time.Time { return time.Unix(clock.Add(61), 0) }
		x := newFx(t, "reset", fakes{pg: "exit 1"})
		x.start(Options{})
		waitFor(t, "four restart delays", func() bool { return len(fa.got()) >= 4 })
		if got, want := fa.got()[:4], []time.Duration{time.Second, time.Second, time.Second, time.Second}; !reflect.DeepEqual(got, want) {
			t.Errorf("restart delays %v, want %v (each run up over a minute)", got, want)
		}
	})
	t.Run("the API restarts too", func(t *testing.T) {
		fast(t)
		x := newFx(t, "apicrash", fakes{api: "exit 3"})
		x.start(Options{})
		waitFor(t, "three API starts", func() bool { return calls(t, x.api) >= 3 })
		if n := calls(t, x.pg); n != 1 {
			t.Errorf("postgres started %d times while the API crashed, want 1", n)
		}
	})
}

// finalErr is a refusal, as *server.GuardError is.
type finalErr struct{ msg string }

func (e finalErr) Error() string { return e.msg }
func (finalErr) Final() bool     { return true }

func TestRefusalStartsNothingAndIsReported(t *testing.T) {
	fast(t)
	x := newFx(t, "refused", fakes{})
	secret := "s3cret-in-a-reason"
	reason := "the native database " + x.cfg.DataDir + " is missing: refusing to start an empty memory in its place (" + secret + ")"
	done := x.start(Options{Secrets: []string{secret}, Check: func(context.Context, io.Writer) error {
		return fmt.Errorf("guard: %w", finalErr{reason})
	}})
	st := waitStatus(t, x.cfg.Project, stateRefused, stateStopped)
	want := "guard: " + strings.ReplaceAll(reason, secret, "[redacted]")
	if st.Refusal != want {
		t.Errorf("refusal %q, want %q", st.Refusal, want)
	}
	raw, err := os.ReadFile(x.file(refusedFile))
	if err != nil || strings.TrimSpace(string(raw)) != want {
		t.Errorf("refused file %q (%v), want the reason", raw, err)
	}
	for _, f := range []string{x.pg, x.isReady, x.ctl, x.api} {
		if got := testtool.Calls(t, f); got != nil {
			t.Errorf("%s ran %q: a refusal starts nothing", filepath.Base(f), got)
		}
	}
	if l := x.log("supervisor"); strings.Contains(l, secret) || !strings.Contains(l, "refused") {
		t.Errorf("supervisor log %q: want the refusal, redacted", l)
	}
	// api-start changes nothing; the channel answers until stop.
	if st, err := ask(x.cfg.Project, opAPIStart); err != nil || st.DB != stateRefused || calls(t, x.api) != 0 {
		t.Errorf("api-start on a refusal: %+v %v", st, err)
	}
	if _, err := ask(x.cfg.Project, opStop); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, done, 5*time.Second); err != nil {
		t.Errorf("Run after stop: %v", err)
	}
}

func TestTransientGuardErrorIsRetried(t *testing.T) {
	fast(t)
	x := newFx(t, "retried", fakes{})
	var n atomic.Int32
	x.start(Options{Check: func(context.Context, io.Writer) error {
		if n.Add(1) <= 2 {
			return errors.New("pg_controldata failed for a moment")
		}
		return nil
	}})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	if got := n.Load(); got != 3 {
		t.Errorf("the guard ran %d times, want 3 (two failures, then the start)", got)
	}
	if _, err := os.Stat(x.file(refusedFile)); err == nil {
		t.Error("a transient guard error wrote the refused file")
	}
}

// What the guard writes (the strays it stops) is in the supervisor's log,
// a stamped line each and redacted, never on the supervisor's stderr
// (nobody reads a task's).
func TestGuardOutputGoesToTheSupervisorLog(t *testing.T) {
	fast(t)
	x := newFx(t, "guardlog", fakes{})
	secret := "pw-in-a-stray-line"
	x.start(Options{Secrets: []string{secret}, Check: func(_ context.Context, log io.Writer) error {
		fmt.Fprintf(log, "a postgres (pid 4242) runs on %s outside the supervisor: stopping it (%s)\n", x.cfg.DataDir, secret)
		fmt.Fprintf(log, "a postgres (pid 4343) runs too\n")
		return nil
	}})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	l := x.log("supervisor")
	stamped := regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ a postgres \(pid 4242\) runs on .* outside the supervisor: stopping it \(\[redacted\]\)\r?$`)
	if !stamped.MatchString(l) || !regexp.MustCompile(`(?m)^\S+ a postgres \(pid 4343\) runs too\r?$`).MatchString(l) {
		t.Errorf("supervisor log lacks the guard's two lines, stamped:\n%s", l)
	}
	if strings.Contains(l, secret) {
		t.Errorf("supervisor log holds the secret:\n%s", l)
	}
}

// A refused file from an earlier run goes once the guard passes.
func TestPassingGuardRemovesTheRefusedFile(t *testing.T) {
	fast(t)
	x := newFx(t, "unrefused", fakes{})
	if err := os.WriteFile(x.file(refusedFile), []byte("old reason\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	x.start(Options{Check: func(context.Context, io.Writer) error { return nil }})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	if _, err := os.Stat(x.file(refusedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("refused file still there after the guard passed: %v", err)
	}
}

func TestStopStopsAPIThenDBAndExitsZero(t *testing.T) {
	fast(t)
	x := newFx(t, "stop", fakes{})
	done := x.start(Options{})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	if _, err := ask(x.cfg.Project, opStop); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, done, 15*time.Second); err != nil {
		t.Fatalf("Run after stop: %v, want nil (exit 0)", err)
	}
	if got := testtool.Calls(t, x.ctl); !reflect.DeepEqual(got, [][]string{x.cfg.PGCtlStop[1:]}) {
		t.Errorf("pg_ctl argv %q, want %q", got, x.cfg.PGCtlStop[1:])
	}
	l := x.log("supervisor")
	apiStopped, dbStopping, dbStopped := strings.Index(l, "api stopped"), strings.Index(l, "db stopping"), strings.Index(l, "db stopped")
	if apiStopped < 0 || dbStopping < apiStopped || dbStopped < dbStopping {
		t.Errorf("supervisor log: want api stopped, then db stopping, then db stopped:\n%s", l)
	}
	if strings.Contains(l, "killing") {
		t.Errorf("postgres was killed, not stopped by pg_ctl:\n%s", l)
	}
	if _, err := ask(x.cfg.Project, opStatus); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Ask after the stop: %v, want ErrNotRunning", err)
	}
	if n := calls(t, x.pg); n != 1 {
		t.Errorf("postgres started %d times, want 1: a stop restarts nothing", n)
	}
}

// A postgres that ignores pg_ctl is killed once dbStopWait is over.
func TestStopKillsAPostgresThatOutlivesPgCtl(t *testing.T) {
	fast(t)
	tm.dbStopWait = 300 * time.Millisecond
	x := newFx(t, "stopkill", fakes{pg: "write {dir}/ready x\nwaitfile {dir}/never"})
	done := x.start(Options{NoAPI: true})
	waitStatus(t, x.cfg.Project, stateRunning, stateStopped)
	if _, err := ask(x.cfg.Project, opStop); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if l := x.log("supervisor"); !strings.Contains(l, "killing") {
		t.Errorf("supervisor log: want postgres killed after pg_ctl:\n%s", l)
	}
}

func TestAPIStopKeepsTheDatabase(t *testing.T) {
	fast(t)
	x := newFx(t, "apistop", fakes{})
	x.start(Options{})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	waitFor(t, "the API's start recorded", func() bool { return calls(t, x.api) == 1 })
	st, err := ask(x.cfg.Project, opAPIStop)
	if err != nil || st.API != stateStopped || st.DB != stateRunning {
		t.Fatalf("api-stop answered %+v (%v): want the API stopped, the database running", st, err)
	}
	time.Sleep(300 * time.Millisecond) // a restart would come within backoffMin
	if n := calls(t, x.api); n != 1 {
		t.Errorf("the API started %d times after api-stop, want 1: it stays stopped", n)
	}
	if got := testtool.Calls(t, x.ctl); got != nil {
		t.Errorf("pg_ctl ran %q: api-stop keeps the database", got)
	}
	if st := waitStatus(t, x.cfg.Project, stateRunning, stateStopped); st.DB != stateRunning {
		t.Errorf("status %+v", st)
	}
	st, err = ask(x.cfg.Project, opAPIStart)
	if err != nil || st.API != stateRunning {
		t.Fatalf("api-start answered %+v (%v)", st, err)
	}
	waitFor(t, "the API started again after api-start", func() bool { return calls(t, x.api) == 2 })
	if n := calls(t, x.pg); n != 1 {
		t.Errorf("postgres started %d times, want 1", n)
	}
}

// A database that exits takes the API down first; the API comes back
// once the database is ready again.
func TestDatabaseExitStopsTheAPIFirst(t *testing.T) {
	fast(t)
	x := newFx(t, "dbcrash", fakes{})
	x.start(Options{})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	waitFor(t, "the API's start recorded", func() bool { return calls(t, x.api) == 1 })
	// postgres crashes.
	waitFor(t, "postgres' pid", func() bool { return len(strings.Fields(readFile(x.file("pg.pids")))) == 1 })
	pid, _ := strconv.Atoi(strings.TrimSpace(readFile(x.file("pg.pids"))))
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "postgres restarted", func() bool { return calls(t, x.pg) >= 2 })
	l := x.log("supervisor")
	if exited, apiStopped := strings.Index(l, "db exited"), strings.Index(l, "api stopped"); exited < 0 || apiStopped < exited {
		t.Errorf("supervisor log: want db exited, then api stopped:\n%s", l)
	}
	waitFor(t, "the API started again", func() bool { return calls(t, x.api) >= 2 })
}

func TestSecondSupervisorExitsWithoutStartingAnything(t *testing.T) {
	fast(t)
	first := newFx(t, "single", fakes{})
	first.start(Options{})
	waitStatus(t, first.cfg.Project, stateRunning, stateRunning)

	second := newFx(t, "single", fakes{})
	checked := false
	start := time.Now()
	// Bounded, so a second supervisor that does run fails the test
	// instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, second.cfg, Options{Check: func(context.Context, io.Writer) error { checked = true; return nil }})
	if err != nil || time.Since(start) > 2*time.Second {
		t.Errorf("the second supervisor returned %v after %s, want nil within 2 s", err, time.Since(start))
	}
	if checked {
		t.Error("the second supervisor ran the guard")
	}
	for _, f := range []string{second.pg, second.isReady, second.ctl, second.api} {
		if got := testtool.Calls(t, f); got != nil {
			t.Errorf("the second supervisor ran %s %q", filepath.Base(f), got)
		}
	}
	if st, err := ask(first.cfg.Project, opStatus); err != nil || st.DB != stateRunning || st.API != stateRunning {
		t.Errorf("the first supervisor after the second: %+v %v", st, err)
	}
	if n := calls(t, first.pg); n != 1 {
		t.Errorf("the first postgres started %d times, want 1", n)
	}
}

func TestTwoProjectsSuperviseSideBySide(t *testing.T) {
	fast(t)
	a, b := newFx(t, "side-a", fakes{}), newFx(t, "side-b", fakes{})
	doneA := a.start(Options{})
	b.start(Options{})
	waitStatus(t, a.cfg.Project, stateRunning, stateRunning)
	waitStatus(t, b.cfg.Project, stateRunning, stateRunning)
	if _, err := ask(a.cfg.Project, opStop); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, doneA, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if st, err := ask(b.cfg.Project, opStatus); err != nil || st.DB != stateRunning || st.API != stateRunning {
		t.Errorf("project b after a stopped: %+v %v, want both running", st, err)
	}
	if got := testtool.Calls(t, b.ctl); got != nil {
		t.Errorf("stopping a ran b's pg_ctl %q", got)
	}
}

func TestSupervisorLogsCarryNoSecret(t *testing.T) {
	fast(t)
	pw, llm := "s3cr@t/pw 123456", "sk-llm-key-abcdef123456"
	dsn := (&url.URL{Scheme: "postgresql", User: url.UserPassword("hindsight", pw), Host: "127.0.0.1:5432", Path: "/hindsight"}).String()
	x := newFx(t, "secrets", fakes{
		pg:  "write {dir}/ready x\nstdout FATAL: password authentication failed: " + pw + "\nstderr connecting to " + dsn + "\nwaitfile {dir}/pg-stop",
		api: "env HINDSIGHT_API_DATABASE_URL\nenv HINDSIGHT_API_LLM_API_KEY\nwaitfile {dir}/never",
	})
	if err := os.WriteFile(x.cfg.EnvFile, []byte("HINDSIGHT_API_DATABASE_URL='"+dsn+"'\nHINDSIGHT_API_LLM_API_KEY="+llm+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var tries atomic.Int32
	x.start(Options{Secrets: []string{pw, llm}, Check: func(context.Context, io.Writer) error {
		if tries.Add(1) == 1 {
			return errors.New("pg_controldata failed with " + pw) // logged, retried
		}
		return nil
	}})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	waitFor(t, "the children's output", func() bool {
		return strings.Contains(x.log("db"), "connecting to") && strings.Count(x.log("api"), "\n") >= 2
	})
	escaped := strings.TrimPrefix(url.UserPassword("", pw).String(), ":")
	for _, name := range []string{"db", "api", "supervisor"} {
		l := x.log(name)
		for _, leak := range []string{pw, escaped, llm} {
			if strings.Contains(l, leak) {
				t.Errorf("%s log carries %q:\n%s", name, leak, l)
			}
		}
		if !strings.Contains(l, "[redacted]") {
			t.Errorf("%s log has no [redacted]:\n%s", name, l)
		}
	}
	if !strings.Contains(x.log("db"), "postgresql://hindsight:[redacted]@127.0.0.1:5432/hindsight") {
		t.Errorf("db log: want the URL with its password redacted:\n%s", x.log("db"))
	}
	// No secret on any argv.
	for _, f := range []string{x.pg, x.isReady, x.ctl, x.api} {
		for _, argv := range testtool.Calls(t, f) {
			if a := strings.Join(argv, " "); strings.Contains(a, pw) || strings.Contains(a, llm) || strings.Contains(a, escaped) {
				t.Errorf("%s ran with a secret on its argv: %q", filepath.Base(f), argv)
			}
		}
	}
}

func TestLogRotatesAt10MiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p-db.log")
	r, err := openRotating(path)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 1023) + "\n")
	const n = 11 * 1024 // 11 MiB of 1 KiB lines
	for range n {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	cur, err1 := os.Stat(path)
	old, err2 := os.Stat(path + ".1")
	if err1 != nil || err2 != nil {
		t.Fatalf("after 11 MiB: %v, %v; want the log and its .1", err1, err2)
	}
	if old.Size() != maxLog || cur.Size() != n*1024-maxLog {
		t.Errorf("sizes .1 %d, current %d; want %d and %d (rotated at 10 MiB)", old.Size(), cur.Size(), maxLog, n*1024-maxLog)
	}
	// Reopened, it appends and rotates again, dropping the older .1.
	r, err = openRotating(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 * 1024 {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Close()
	cur, _ = os.Stat(path)
	old, _ = os.Stat(path + ".1")
	if want := int64(n*1024 - maxLog + 10*1024*1024 - maxLog); cur.Size() != want || old.Size() != maxLog {
		t.Errorf("after a second rotation: current %d .1 %d, want %d and %d", cur.Size(), old.Size(), want, maxLog)
	}
}

// A secret split across two writes is still redacted: lines are whole.
func TestRedactorJoinsASecretSplitAcrossWrites(t *testing.T) {
	var b strings.Builder
	l := newLineRedactor(&b, []string{"split-secret-value"})
	_, _ = l.Write([]byte("a split-sec"))
	_, _ = l.Write([]byte("ret-value b\nlast split-secret-value"))
	_ = l.Close()
	if got, want := b.String(), "a [redacted] b\nlast [redacted]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAskWithoutSupervisorIsNotRunning(t *testing.T) {
	p := project("nobody")
	cleanTransport(t, p)
	if _, err := ask(p, opStatus); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Ask with no supervisor: %v, want ErrNotRunning", err)
	}
	if _, err := ask(p, "reboot"); err == nil || errors.Is(err, ErrNotRunning) {
		t.Errorf("Ask with an unknown request: %v, want a refusal", err)
	}
}

// The numbers of 0080's Global Constraints, as the product runs them.
func TestDefaultTimings(t *testing.T) {
	want := map[string][2]time.Duration{
		"readiness poll":    {tm.readyPoll, 100 * time.Millisecond},
		"readiness timeout": {tm.readyWait, 300 * time.Second},
		"API stop wait":     {tm.apiStopWait, 30 * time.Second},
		"DB stop wait":      {tm.dbStopWait, 60 * time.Second},
		"backoff start":     {tm.backoffMin, time.Second},
		"backoff cap":       {tm.backoffMax, 5 * time.Minute},
		"backoff reset":     {tm.resetAfter, 60 * time.Second},
	}
	for name, v := range want {
		if v[0] != v[1] {
			t.Errorf("%s %v, want %v", name, v[0], v[1])
		}
	}
	if outputWait != 2*time.Second {
		t.Errorf("output wait %v, want 2s", outputWait)
	}
}

// A postgres that never answers pg_isready is killed after the readiness
// timeout and started again with the backoff.
func TestReadinessTimeoutKillsAndRestartsPostgres(t *testing.T) {
	fast(t)
	tm.readyWait = 300 * time.Millisecond
	fa := &fakeAfter{fire: 1}
	tm.after = fa.after
	x := newFx(t, "readytimeout", fakes{isReady: "exit 2"})
	x.start(Options{})
	waitFor(t, "postgres started again", func() bool { return calls(t, x.pg) >= 2 })
	if got := fa.got(); len(got) == 0 || got[0] != tm.backoffMin {
		t.Errorf("restart delays %v, want the backoff's first, %v", got, tm.backoffMin)
	}
	l := x.log("supervisor")
	if !strings.Contains(l, "pg_isready did not answer within 300ms: killing it") || !strings.Contains(l, "db exited") {
		t.Errorf("supervisor log: want the timeout, the kill, the exit:\n%s", l)
	}
	if n := calls(t, x.api); n != 0 {
		t.Errorf("the API started %d times on a database that never answered", n)
	}
}

// An API that ignores the interrupt is killed once apiStopWait is over.
func TestAPIIgnoringTheInterruptIsKilled(t *testing.T) {
	fast(t)
	tm.apiStopWait = 500 * time.Millisecond
	x := newFx(t, "stubborn", fakes{api: "ignoresignals\npid {dir}/api.pids\nwaitfile {dir}/never"})
	x.start(Options{})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	waitFor(t, "the API's pid", func() bool { return strings.TrimSpace(readFile(x.file("api.pids"))) != "" })
	start := time.Now()
	st, err := ask(x.cfg.Project, opAPIStop)
	if err != nil || st.API != stateStopped {
		t.Fatalf("api-stop: %+v %v", st, err)
	}
	if d := time.Since(start); d < tm.apiStopWait {
		t.Errorf("api-stop answered after %s, before the %s the API is given", d, tm.apiStopWait)
	}
	if l := x.log("supervisor"); !strings.Contains(l, "api killed, still running 500ms after the interrupt") {
		t.Errorf("supervisor log: want the kill after the wait:\n%s", l)
	}
}

// A line longer than maxLine is written in parts as it comes. A secret's
// form cut by the write that takes the buffer past maxLine is held back
// whole (the tail kept for what follows), so the log never carries it in
// two pieces that read as the secret; a whole one in the same line is
// redacted once.
func TestRedactorNeverCutsASecretInALongLine(t *testing.T) {
	secret := "long-line s3cr@t"
	userinfo := strings.TrimPrefix(url.UserPassword("", secret).String(), ":") // %20, %40
	forms := []string{secret, url.QueryEscape(secret), url.PathEscape(secret), userinfo}
	for _, form := range forms {
		path := filepath.Join(t.TempDir(), "p-api.log")
		r, err := openRotating(path)
		if err != nil {
			t.Fatal(err)
		}
		l := newLineRedactor(r, []string{secret})
		half := len(form) / 2
		// First write: past maxLine, ending inside the form.
		_, _ = l.Write([]byte(strings.Repeat("a", maxLine+100) + form[:half]))
		if len(l.buf) > l.keep {
			t.Fatalf("form %q: %d bytes held after the first write, want at most %d", form, len(l.buf), l.keep)
		}
		// Then the rest of it, more text, a whole one, the line's end.
		_, _ = l.Write([]byte(form[half:] + strings.Repeat("b", 100)))
		_, _ = l.Write([]byte(form + "c\n"))
		_ = l.Close()
		_ = r.Close()
		got := readFile(path)
		for _, f := range forms {
			if strings.Contains(got, f) {
				t.Errorf("form %q: the log carries %q", form, f)
			}
		}
		if n := strings.Count(got, "[redacted]"); n != 2 {
			t.Errorf("form %q: %d [redacted], want 2 (the cut one, the whole one)", form, n)
		}
		if want := strings.Repeat("a", maxLine+100) + "[redacted]" + strings.Repeat("b", 100) + "[redacted]c\n"; got != want {
			t.Errorf("form %q: the log is not the line with its two forms redacted (%d bytes, want %d)", form, len(got), len(want))
		}
	}
}

// slowWriter delays each write: a log writer behind a busy disk, so the
// last output arrives after the child exited.
type slowWriter struct{ io.WriteCloser }

func (w slowWriter) Write(p []byte) (int, error) {
	time.Sleep(300 * time.Millisecond)
	return w.WriteCloser.Write(p)
}

// A child's last words, printed as it stops, are in its log once Run
// returned, though they reach it after the child exited.
func TestLastOutputReachesTheLog(t *testing.T) {
	fast(t)
	tm.wrapOutput = func(w io.WriteCloser) io.WriteCloser { return slowWriter{w} }
	x := newFx(t, "lastwords", fakes{
		pg:  "write {dir}/ready x\nwaitfile {dir}/pg-stop\nstdout database system is shut down",
		api: "waitfile {dir}/never",
	})
	done := x.start(Options{NoAPI: true})
	waitStatus(t, x.cfg.Project, stateRunning, stateStopped)
	if _, err := ask(x.cfg.Project, opStop); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, done, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if l := x.log("db"); !strings.Contains(l, "database system is shut down") {
		t.Errorf("db log lost postgres' last line:\n%s", l)
	}
}

// A request the supervisor does not know, or cannot read, is answered
// with the status: its Proto tells a newer client what it speaks, and Ask
// never takes an answering supervisor for a missing one.
func TestUnknownRequestIsAnsweredWithTheStatus(t *testing.T) {
	fast(t)
	x := newFx(t, "unknown", fakes{})
	x.start(Options{Version: "8.8.8"})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	for _, line := range []string{`{"op":"reboot"}`, `not json`} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := dial(ctx, x.cfg.Project)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write([]byte(line + "\n"))
		raw, err := io.ReadAll(c)
		_ = c.Close()
		cancel()
		var st Status
		if err != nil || json.Unmarshal(raw, &st) != nil || st.Proto != Proto || st.Version != "8.8.8" || st.DB != stateRunning {
			t.Errorf("request %q answered %q (%v), want the status", line, raw, err)
		}
	}
	if n := calls(t, x.ctl); n != 0 {
		t.Errorf("an unknown request ran pg_ctl %d times", n)
	}
}

// server.env is read at each API start, and its secrets with it: a key
// rotated between api-stop and api-start is redacted too.
func TestAPIStartRefreshesTheRedactedSecrets(t *testing.T) {
	fast(t)
	oldKey, newKey := "sk-old-key-1234567890", "sk-new-key-0987654321"
	x := newFx(t, "rotate", fakes{api: "env HINDSIGHT_API_LLM_API_KEY\nwaitfile {dir}/never"})
	write := func(key string) {
		if err := os.WriteFile(x.cfg.EnvFile, []byte("HINDSIGHT_API_LLM_API_KEY="+key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(oldKey)
	x.start(Options{}) // neither key among Options.Secrets
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	waitFor(t, "the API's first output", func() bool { return strings.Count(x.log("api"), "\n") >= 1 })
	if _, err := ask(x.cfg.Project, opAPIStop); err != nil {
		t.Fatal(err)
	}
	write(newKey)
	if _, err := ask(x.cfg.Project, opAPIStart); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the API's second output", func() bool { return strings.Count(x.log("api"), "\n") >= 2 })
	for _, name := range []string{"api", "supervisor"} {
		l := x.log(name)
		if strings.Contains(l, oldKey) || strings.Contains(l, newKey) {
			t.Errorf("%s log carries a key:\n%s", name, l)
		}
	}
	if got := strings.Count(x.log("api"), "[redacted]"); got != 2 {
		t.Errorf("api log has %d [redacted], want 2:\n%s", got, x.log("api"))
	}
}

// A periodic secret in a long line costs one pass per part and never
// stops the line from being written as it comes (a cut walked back to 0
// would hold it all).
func TestRedactorHandlesAPeriodicSecretPromptly(t *testing.T) {
	var b strings.Builder
	l := newLineRedactor(nopWriteCloser{&b}, []string{"aaaaaaaa"})
	start := time.Now()
	for range 64 {
		_, _ = l.Write([]byte(strings.Repeat("a", 64<<10)))
	}
	// Written as it comes, not held until the line ends: memory stays
	// bounded whatever the line's length.
	if written, held := b.Len(), len(l.buf); written < 3<<20 || held > maxLine+l.keep {
		t.Errorf("after 4 MiB without a newline: %d bytes written, %d held; want the parts written, at most %d held", written, held, maxLine+l.keep)
	}
	_, _ = l.Write([]byte("\n"))
	_ = l.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("4 MiB of a periodic secret took %s", d)
	}
	got := b.String()
	if strings.Contains(got, "aaaaaaaa") || !strings.HasSuffix(got, "\n") || strings.Count(got, "[redacted]") < (4<<20)/8-1024 {
		t.Errorf("%d bytes, %d [redacted], secret left %v: want the line written, the secret gone", len(got), strings.Count(got, "[redacted]"), strings.Contains(got, "aaaaaaaa"))
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// Without a console (a supervisor started detached) the API cannot be
// interrupted: it is killed at once, and the log says why.
func TestNoConsoleKillsTheAPIAtOnce(t *testing.T) {
	fast(t)
	tm.apiStopWait = 10 * time.Second
	tm.interrupt = func(*os.Process) error { return errors.New("no console") }
	x := newFx(t, "noconsole", fakes{})
	x.start(Options{})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	start := time.Now()
	if st, err := ask(x.cfg.Project, opAPIStop); err != nil || st.API != stateStopped {
		t.Fatalf("api-stop: %+v %v", st, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("api-stop took %s: no wait for an interrupt that could not be sent", d)
	}
	l := x.log("supervisor")
	if !strings.Contains(l, "api: no console to interrupt it: killed") || strings.Contains(l, "after the interrupt") {
		t.Errorf("supervisor log: want the no-console kill:\n%s", l)
	}
}

// The loop answers stop, then returns, and the channel closes: the answer
// already given still reaches the client, never an EOF taken for "not
// running".
func TestAnswerGivenBeforeTheChannelClosesIsSent(t *testing.T) {
	for i := range 500 {
		s := &supervisor{ops: make(chan opRequest)}
		done := make(chan struct{})
		go func() {
			r := <-s.ops
			r.reply <- Status{DB: stateStopped}
			close(done)
		}()
		if st, ok := s.answer(opStop, done); !ok || st.DB != stateStopped {
			t.Fatalf("try %d: answer %+v, ok %v: the stop's answer was dropped", i, st, ok)
		}
	}
}

// A long buffer that redaction shrinks below what is held back (a long
// secret, repeated) is held, not cut at a negative offset.
func TestRedactorHoldsABufferRedactionShrinks(t *testing.T) {
	secret := strings.Repeat("k", 2000) // longer than keepTail: keep grows to it
	var b strings.Builder
	l := newLineRedactor(nopWriteCloser{&b}, []string{secret})
	_, _ = l.Write([]byte(strings.Repeat(secret, 33))) // 66 000 bytes, 330 once redacted
	_, _ = l.Write([]byte("\n"))
	_ = l.Close()
	if want := strings.Repeat("[redacted]", 33) + "\n"; b.String() != want {
		t.Errorf("got %d bytes, want %d: the 33 copies redacted", b.Len(), len(want))
	}
}
