package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// recoverRT is a runtime whose deployed version and containers a test sets.
type recoverRT struct {
	server.Runtime
	deployed string
	ps       []server.Container
	psErr    error
}

func (r recoverRT) Deployed() (string, error) { return r.deployed, nil }
func (r recoverRT) Status(context.Context) ([]server.Container, error) {
	return r.ps, r.psErr
}

// downAtBoot: Docker started the database at boot, and the API server
// failed to bind its private address (it was not there yet).
var downAtBoot = []server.Container{{Service: "db", State: "running", Health: "healthy"}, {Service: "hindsight", State: "exited"}}

const downAtBootPS = `{"Service":"db","State":"running","Health":"healthy"}` + "\n" + `{"Service":"hindsight","State":"exited","Health":""}`

// withAddrs makes cidrs this machine's addresses.
func withAddrs(t *testing.T, cidrs ...string) {
	t.Helper()
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range cidrs {
			ip, n, _ := net.ParseCIDR(c)
			out = append(out, &net.IPNet{IP: ip, Mask: n.Mask})
		}
		return out, nil
	}
	t.Cleanup(func() { interfaceAddrs = old })
}

func TestExposureNeedsStart(t *testing.T) {
	withAddrs(t, "127.0.0.1/8", "100.101.1.2/32")
	private := config.Default(paths.Dirs{}, t.TempDir())
	private.Network.Exposure, private.Network.BindIP = "private", "100.101.1.2"
	up := recoverRT{deployed: "img", ps: downAtBoot}
	remote := private
	remote.Server.Mode = "remote"
	loopback := private
	loopback.Network.Exposure, loopback.Network.BindIP = "loopback", ""
	late := private
	late.Network.BindIP = "100.101.1.9"
	for _, c := range []struct {
		name  string
		cfg   config.Config
		rt    recoverRT
		start bool
		why   string
	}{
		{"address now present", private, up, true, "100.101.1.2"},
		{"remote server", remote, up, false, "another machine"},
		{"loopback exposure", loopback, up, false, "loopback"},
		{"not deployed", private, recoverRT{ps: downAtBoot}, false, "not installed"},
		{"api running", private, recoverRT{deployed: "img", ps: []server.Container{{Service: "db", State: "running"}, {Service: "hindsight", State: "running"}}}, false, "runs"},
		// server stop stops the database too: a deliberate stop stays.
		{"stopped by hand", private, recoverRT{deployed: "img", ps: []server.Container{{Service: "db", State: "exited"}, {Service: "hindsight", State: "exited"}}}, false, "stopped"},
		{"docker unreadable", private, recoverRT{deployed: "img", psErr: errors.New("Cannot connect to the Docker daemon")}, false, "Docker daemon"},
		{"address absent", late, up, false, "waiting for 100.101.1.9 to appear on this machine"},
	} {
		start, why := exposureNeedsStart(context.Background(), c.cfg, c.rt)
		if start != c.start || !strings.Contains(why, c.why) {
			t.Errorf("%s: start %v, why %q; want %v, %q", c.name, start, why, c.start, c.why)
		}
	}
}

// recoverFixture is a host whose server is exposed on 100.101.1.2 and was
// left down at boot (its API port is closed); spawns lists the detached
// commands started, logs the files their output went to.
type recoverFixture struct {
	home   string
	ps     *psRunner
	spawns [][]string
	logs   []string
}

func recoverHost(t *testing.T) *recoverFixture {
	t.Helper()
	fx := &recoverFixture{home: devInstall(t, true), ps: &psRunner{ps: downAtBootPS}}
	raw := "[server]\nurl = \"http://127.0.0.1:" + strconv.Itoa(closedPort(t)) + "\"\nui_port = 19999\n\n[network]\nexposure = \"private\"\nbind_ip = \"100.101.1.2\"\n"
	if err := os.WriteFile(filepath.Join(fx.home, "backsight", "config", "config.toml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(composeDir(fx.home), 0o700)
	if err := os.WriteFile(filepath.Join(composeDir(fx.home), "compose.yaml"), []byte("services:\n  hindsight:\n    image: hindsight@sha256:abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withAddrs(t, "127.0.0.1/8", "100.101.1.2/32")
	oldRunner, oldSpawn := composeRunner, spawnLogged
	composeRunner = fx.ps
	spawnLogged = func(out *os.File, args ...string) error {
		fx.spawns = append(fx.spawns, args)
		if out != nil {
			fx.logs = append(fx.logs, out.Name())
		}
		return nil
	}
	t.Cleanup(func() { composeRunner, spawnLogged = oldRunner, oldSpawn })
	return fx
}

func TestTickStartsALateExposedServer(t *testing.T) {
	fx := recoverHost(t)
	_, out, errOut := run(t, "jobs", "tick")
	if len(fx.spawns) != 1 || strings.Join(fx.spawns[0], " ") != "server start" {
		t.Fatalf("spawns %q\n%s%s", fx.spawns, out, errOut)
	}
	if want := filepath.Join(pathsOf(fx.home).State, "logs", "server-start.log"); len(fx.logs) != 1 || fx.logs[0] != want {
		t.Errorf("output to %q, want %s", fx.logs, want)
	}
	if !strings.Contains(out, "server start spawned") {
		t.Errorf("the tick does not say it started the server:\n%s", out)
	}
	// The address not there yet: nothing is started.
	withAddrs(t, "127.0.0.1/8")
	_, out, _ = run(t, "jobs", "tick")
	if len(fx.spawns) != 1 {
		t.Errorf("spawned without the address: %q\n%s", fx.spawns, out)
	}
}

func TestSessionStartStartsServerAtMostEvery5Min(t *testing.T) {
	fx := recoverHost(t)
	_ = os.MkdirAll(filepath.Join(fx.home, "billing"), 0o700)
	start := func() []byte {
		t.Helper()
		// The 5-minute stamp alone: the backoff has its own test.
		_ = os.Remove(filepath.Join(pathsOf(fx.home).State, serverStartBackoff))
		raw, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": filepath.Join(fx.home, "billing"), "source": "startup"})
		out, err := handleHook(context.Background(), "session-start", raw)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	t0 := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	withClock(t, t0)
	start()
	if len(fx.spawns) != 1 || strings.Join(fx.spawns[0], " ") != "server start" {
		t.Fatalf("first session: spawns %q", fx.spawns)
	}
	withClock(t, t0.Add(4*time.Minute))
	calls := len(fx.ps.calls)
	start()
	if len(fx.spawns) != 1 || len(fx.ps.calls) != calls {
		t.Errorf("within 5 minutes: spawns %q, docker asked %d more time(s)", fx.spawns, len(fx.ps.calls)-calls)
	}
	withClock(t, t0.Add(10*time.Minute))
	recovered := start()
	if len(fx.spawns) != 2 {
		t.Errorf("after 5 minutes: spawns %q", fx.spawns)
	}
	// What the session sees is the same as without a recovery.
	withAddrs(t, "127.0.0.1/8")
	withClock(t, t0.Add(20*time.Minute))
	plain := start()
	if len(fx.spawns) != 2 {
		t.Errorf("spawned without the address: %q", fx.spawns)
	}
	if string(recovered) != string(plain) {
		t.Errorf("the recovery changed the hook's output:\n%s\n---\n%s", recovered, plain)
	}
}

// doctor's server check says why a privately exposed server is down.
func TestDoctorNamesTheLateAddress(t *testing.T) {
	recoverHost(t)
	old := probes
	probes = func(e *doctor.Env) {
		old(e)
		e.ExposureDetail = func() (string, string) { return exposureDetail(e.Dirs, e.Home) }
	}
	t.Cleanup(func() { probes = old })
	detail := func() (doctor.Status, string, string) {
		t.Helper()
		_, out, _ := run(t, "doctor", "--json")
		var env struct {
			Data doctor.Report `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, c := range env.Data.Checks {
			if c.ID == "server" {
				return c.Status, c.Detail, c.Fix
			}
		}
		t.Fatalf("no server check:\n%s", out)
		return "", "", ""
	}
	// The call's own error stays in the detail (a timeout reads apart from a refusal).
	if st, d, fix := detail(); st != doctor.Error || !strings.Contains(d, "backsight server start") || strings.Contains(d, "not on this machine") ||
		!strings.Contains(d, "hindsight server unreachable") || !strings.Contains(fix, "backsight server start") {
		t.Errorf("address present: %s %q, fix %q", st, d, fix)
	}
	withAddrs(t, "127.0.0.1/8")
	want := "the private address 100.101.1.2 is not on this machine yet: the server starts once it appears (jobs tick, or the next Claude Code session); to stop depending on it: backsight server expose --loopback (hindsight server unreachable"
	// server start would fail on the missing address: not the fix.
	if st, d, fix := detail(); st != doctor.Error || !strings.HasPrefix(d, want) || strings.Contains(fix, "server start") || !strings.Contains(fix, "backsight server expose --loopback") {
		t.Errorf("address absent: %s %q, fix %q", st, d, fix)
	}
}

// No server start while a restore did not finish (whatever its step: the
// start would resume it with nobody there) or while a command holds
// server.lock (it restarts the server itself), from jobs tick or a session.
func TestLateServerWaitsForRestoreAndServerLock(t *testing.T) {
	fx := recoverHost(t)
	d := pathsOf(fx.home)
	_ = os.MkdirAll(filepath.Dir(d.RestoreMarker()), 0o700)
	session := func() {
		t.Helper()
		_ = os.Remove(filepath.Join(d.State, serverCheckStamp))
		raw, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": fx.home, "source": "startup"})
		if _, err := handleHook(context.Background(), "session-start", raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []string{"", "key", "purge"} {
		if err := os.WriteFile(d.RestoreMarker(), []byte(`{"command":"restore","step":"`+step+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, out, _ := run(t, "jobs", "tick")
		session()
		if len(fx.spawns) != 0 {
			t.Fatalf("restore marker at step %q: spawns %q\n%s", step, fx.spawns, out)
		}
	}
	_ = os.Remove(d.RestoreMarker())
	l, _, err := state.Acquire(filepath.Join(d.LocksDir(), "server.lock"), "server expose")
	if err != nil {
		t.Fatal(err)
	}
	_, out, _ := run(t, "jobs", "tick")
	session()
	if len(fx.spawns) != 0 {
		t.Fatalf("server.lock held: spawns %q\n%s", fx.spawns, out)
	}
	_ = l.Release()
	if _, out, _ := run(t, "jobs", "tick"); len(fx.spawns) != 1 {
		t.Errorf("nothing held: spawns %q\n%s", fx.spawns, out)
	}
}

// readStartBackoff reads server-start-backoff.json; ok false when absent.
func readStartBackoff(t *testing.T, d paths.Dirs) (b struct {
	Attempts int       `json:"attempts"`
	NextAt   time.Time `json:"next_at"`
}, ok bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(d.State, "server-start-backoff.json"))
	if errors.Is(err, os.ErrNotExist) {
		return b, false
	}
	if err != nil || json.Unmarshal(raw, &b) != nil {
		t.Fatalf("backoff file: %v %s", err, raw)
	}
	return b, true
}

// A server start that does not bring the server up is not repeated at
// every look: 15 minutes, then twice as long each time, at most 6 hours.
// A look that finds the server running, stopped by hand, or no longer
// exposed on a private address starts the count over.
func TestLateServerStartBacksOffAndResets(t *testing.T) {
	fx := recoverHost(t)
	d := pathsOf(fx.home)
	cfg, err := config.Load(d.ConfigFile(), d, fx.home)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	look := func(at time.Time, c config.Config) int {
		t.Helper()
		withClock(t, at)
		n := len(fx.spawns)
		startLateServer(context.Background(), d, c)
		return len(fx.spawns) - n
	}
	for _, s := range []struct {
		after time.Duration
		spawn int
	}{{0, 1}, {10 * time.Minute, 0}, {15 * time.Minute, 1}, {40 * time.Minute, 0}, {45 * time.Minute, 1}} {
		if got := look(t0.Add(s.after), cfg); got != s.spawn {
			t.Fatalf("t0+%s: %d spawn(s), want %d", s.after, got, s.spawn)
		}
	}
	// Each further attempt at its due time, up to 10.
	for b, _ := readStartBackoff(t, d); b.Attempts < 10; b, _ = readStartBackoff(t, d) {
		if look(b.NextAt.Add(-time.Minute), cfg) != 0 || look(b.NextAt, cfg) != 1 {
			t.Fatalf("attempt %d: not spawned exactly at its due time %s", b.Attempts+1, b.NextAt)
		}
	}
	b, _ := readStartBackoff(t, d)
	if wait := b.NextAt.Sub(now()); wait != 6*time.Hour {
		t.Errorf("after %d attempts the next is due in %s, want 6h", b.Attempts, wait)
	}
	// The server runs: the file goes, and the next failure spawns at once.
	fx.ps.ps = `{"Service":"db","State":"running","Health":"healthy"}` + "\n" + `{"Service":"hindsight","State":"running","Health":"healthy"}`
	due := b.NextAt
	if look(due, cfg) != 0 {
		t.Fatal("spawned while the server runs")
	}
	if _, ok := readStartBackoff(t, d); ok {
		t.Fatal("the backoff survived a running server")
	}
	fx.ps.ps = downAtBootPS
	if look(due, cfg) != 1 {
		t.Fatal("the failure after a running server waited")
	}
	if b, _ := readStartBackoff(t, d); b.Attempts != 1 || !b.NextAt.Equal(due.Add(15*time.Minute)) {
		t.Errorf("counted again from %+v", b)
	}
	// A next_at beyond 6 hours is a clock set back: it holds nothing.
	_ = os.WriteFile(filepath.Join(d.State, "server-start-backoff.json"), []byte(`{"attempts":3,"next_at":"`+due.Add(7*time.Hour).Format(time.RFC3339)+`"}`), 0o600)
	if look(due, cfg) != 1 {
		t.Fatal("a next_at 7 hours ahead held the start")
	}
	// Stopped by hand (database down too), or no longer private: the
	// count starts over as well.
	loopback := cfg
	loopback.Network.Exposure, loopback.Network.BindIP = "loopback", ""
	for _, c := range []struct {
		name string
		ps   string
		cfg  config.Config
	}{
		{"server stop", `{"Service":"db","State":"exited","Health":""}` + "\n" + `{"Service":"hindsight","State":"exited","Health":""}`, cfg},
		{"loopback exposure", downAtBootPS, loopback},
	} {
		b, _ := readStartBackoff(t, d)
		fx.ps.ps = c.ps
		if look(b.NextAt, c.cfg) != 0 {
			t.Fatalf("%s: spawned", c.name)
		}
		if _, ok := readStartBackoff(t, d); ok {
			t.Errorf("%s: the backoff survived", c.name)
		}
		fx.ps.ps = downAtBootPS
		if look(b.NextAt, cfg) != 1 {
			t.Fatalf("%s: the next failure waited", c.name)
		}
	}
}

// A successful server start (the one spawned, or the user's) ends the
// streak: after a 6 h wait, the server comes up and later goes down again
// (a reboot with the address late), and the next look starts it at once.
func TestServerStartEndsTheBackoff(t *testing.T) {
	fx := recoverHost(t)
	d := pathsOf(fx.home)
	cfg, err := config.Load(d.ConfigFile(), d, fx.home)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	for {
		withClock(t, at)
		startLateServer(context.Background(), d, cfg)
		b, _ := readStartBackoff(t, d)
		if b.NextAt.Sub(at) == 6*time.Hour {
			break
		}
		at = b.NextAt
	}
	streak := len(fx.spawns)
	deployFile(t, fx.home, server.HindsightImage) // on this binary's pin
	withCompose(t)
	if code, out, errOut := run(t, "server", "start"); code != output.OK {
		t.Fatalf("server start: exit %d\n%s%s", code, out, errOut)
	}
	if _, ok := readStartBackoff(t, d); ok {
		t.Fatal("the backoff survived a successful server start")
	}
	composeRunner = fx.ps // down again
	withClock(t, at.Add(5*time.Hour))
	if startLateServer(context.Background(), d, cfg); len(fx.spawns) != streak+1 {
		t.Errorf("down again within the old wait: %d spawn(s), want 1", len(fx.spawns)-streak)
	}
}

// If the backoff cannot be recorded, the server is started all the same
// (a bookkeeping failure never keeps it down), and the log says so.
func TestLateServerStartsWhenItsBackoffCannotBeRecorded(t *testing.T) {
	fx := recoverHost(t)
	d := pathsOf(fx.home)
	cfg, err := config.Load(d.ConfigFile(), d, fx.home)
	if err != nil {
		t.Fatal(err)
	}
	// A non-empty directory where the file goes: unwritable, unreadable.
	if err := os.MkdirAll(filepath.Join(d.State, "server-start-backoff.json", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	withClock(t, time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC))
	got := startLateServer(context.Background(), d, cfg)
	if len(fx.spawns) != 1 || !strings.Contains(got, "server start spawned") || !strings.Contains(got, "backoff") {
		t.Fatalf("spawns %q, result %q", fx.spawns, got)
	}
	raw, _ := os.ReadFile(filepath.Join(d.State, "logs", "server-start.log"))
	if !strings.Contains(string(raw), "backoff") {
		t.Errorf("server-start.log does not say the backoff was not recorded:\n%s", raw)
	}
}

// An unreadable backoff file starts the count over: the start is made and
// the file holds the first attempt.
func TestUnreadableStartBackoffStartsOver(t *testing.T) {
	fx := recoverHost(t)
	d := pathsOf(fx.home)
	cfg, err := config.Load(d.ConfigFile(), d, fx.home)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	withClock(t, at)
	_ = os.MkdirAll(d.State, 0o700)
	if err := os.WriteFile(filepath.Join(d.State, "server-start-backoff.json"), []byte(`{"attempts":4,"next_at":`), 0o600); err != nil {
		t.Fatal(err)
	}
	startLateServer(context.Background(), d, cfg)
	if b, _ := readStartBackoff(t, d); len(fx.spawns) != 1 || b.Attempts != 1 || !b.NextAt.Equal(at.Add(15*time.Minute)) {
		t.Errorf("spawns %q, backoff %+v", fx.spawns, b)
	}
}

// jobs tick goes through the backoff: a start at t0, none at t0+10 min,
// the next at t0+15 min.
func TestTickBacksOffALateServerStart(t *testing.T) {
	fx := recoverHost(t)
	t0 := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	for _, s := range []struct {
		after time.Duration
		want  int
	}{{0, 1}, {10 * time.Minute, 1}, {15 * time.Minute, 2}} {
		withClock(t, t0.Add(s.after))
		if _, out, errOut := run(t, "jobs", "tick"); len(fx.spawns) != s.want {
			t.Fatalf("t0+%s: %d spawn(s) in all, want %d\n%s%s", s.after, len(fx.spawns), s.want, out, errOut)
		}
	}
}

// server-start.log over 1 MiB moves to server-start.log.1 (replacing an
// earlier one) before the next start writes to it; at 1 MiB it stays.
func TestServerStartLogRotates(t *testing.T) {
	old := spawnLogged
	spawnLogged = func(*os.File, ...string) error { return nil }
	t.Cleanup(func() { spawnLogged = old })
	d := paths.Dirs{State: t.TempDir()}
	log := filepath.Join(d.State, "logs", "server-start.log")
	_ = os.MkdirAll(filepath.Dir(log), 0o700)
	lines := func(p string) int {
		t.Helper()
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(raw), "\n")
	}

	full := strings.Repeat("x", 1<<20-1) + "\n"
	if err := os.WriteFile(log, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spawnServerStart(d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log + ".1"); !errors.Is(err, os.ErrNotExist) || lines(log) != 2 {
		t.Fatalf("1 MiB exactly: rotated (%v) or not appended (%d lines)", err, lines(log))
	}

	if err := os.WriteFile(log, []byte(full+"y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log+".1", []byte("an earlier rotation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spawnServerStart(d); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(log + ".1"); err != nil || fi.Size() != 1<<20+1 {
		t.Fatalf("server-start.log.1: %v %v, want %d bytes", fi, err, 1<<20+1)
	}
	if n := lines(log); n != 1 {
		t.Errorf("new server-start.log holds %d lines, want 1", n)
	}

	// The rename fails (server-start.log.1 a non-empty directory): the
	// start is still made, its line appended; the next start rotates.
	_ = os.Remove(log + ".1")
	_ = os.MkdirAll(filepath.Join(log+".1", "x"), 0o700)
	if err := os.WriteFile(log, []byte(full+"y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spawnServerStart(d); err != nil {
		t.Fatalf("a failed rotation stopped the start: %v", err)
	}
	if fi, err := os.Stat(log); err != nil || fi.Size() <= 1<<20+1 {
		t.Fatalf("not appended to the unrotated log: %v %v", fi, err)
	}
	_ = os.RemoveAll(log + ".1")
	if err := spawnServerStart(d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log + ".1"); err != nil || lines(log) != 1 {
		t.Errorf("the next start did not rotate: %v, %d lines", err, lines(log))
	}
}
