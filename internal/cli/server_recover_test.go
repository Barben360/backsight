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
	if st, d, fix := detail(); st != doctor.Error || !strings.Contains(d, "backsight server start") || strings.Contains(d, "not on this machine") ||
		!strings.Contains(fix, "backsight server start") {
		t.Errorf("address present: %s %q, fix %q", st, d, fix)
	}
	withAddrs(t, "127.0.0.1/8")
	want := "the private address 100.101.1.2 is not on this machine yet: the server starts once it appears (jobs tick, or the next Claude Code session); to stop depending on it: backsight server expose --loopback"
	// server start would fail on the missing address: not the fix.
	if st, d, fix := detail(); st != doctor.Error || d != want || strings.Contains(fix, "server start") || !strings.Contains(fix, "backsight server expose --loopback") {
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
