package setup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func atLeast(v, minimum string) bool { return v >= minimum } // enough for these fixed strings

// healthy is a machine where every check passes.
func healthy() Probes {
	return Probes{
		GOOS: "linux", GOARCH: "amd64",
		Docker:    func(context.Context) (string, error) { return "28.1.1", nil },
		Compose:   func(context.Context) (string, error) { return "v2.39.1", nil },
		PortFree:  func(int) bool { return true },
		FreeBytes: func() (uint64, error) { return 50 << 30, nil },
		Claude:    func(context.Context) (string, error) { return "2.1.400", nil },
		MinClaude: "2.1.309", AtLeast: atLeast,
		APIPort: 8888, UIPort: 9999,
	}
}

func byID(cs []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range cs {
		m[c.ID] = c
	}
	return m
}

func TestPreflightPassesOnAHealthyMachine(t *testing.T) {
	cs := Preflight(context.Background(), healthy())
	if class, failed := Verdict(cs); class != "" {
		t.Fatalf("class %q, failed %+v", class, failed)
	}
	for _, id := range []string{"platform", "docker", "compose", "api_port", "ui_port", "disk", "claude", "gpu"} {
		if _, ok := byID(cs)[id]; !ok {
			t.Errorf("no %s check", id)
		}
	}
}

func TestPreflightClassifiesEachFailure(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Probes)
		id     string
		class  string
		action string
	}{
		{"no docker", func(p *Probes) {
			p.Docker = func(context.Context) (string, error) { return "", fmt.Errorf("docker: %w", exec.ErrNotFound) }
		}, "docker", "needs_user", "install"},
		{"docker group", func(p *Probes) {
			p.Docker = func(context.Context) (string, error) {
				return "", errors.New("permission denied while trying to connect to the Docker daemon socket")
			}
		}, "docker", "needs_user", "relogin"},
		{"daemon down", func(p *Probes) {
			p.Docker = func(context.Context) (string, error) { return "", errors.New("Cannot connect to the Docker daemon") }
		}, "docker", "blocked", ""},
		{"compose v1", func(p *Probes) { p.Compose = func(context.Context) (string, error) { return "1.29.2", nil } }, "compose", "needs_user", "install"},
		{"port taken", func(p *Probes) { p.PortFree = func(n int) bool { return n != 8888 } }, "api_port", "blocked", ""},
		{"small disk", func(p *Probes) { p.FreeBytes = func() (uint64, error) { return 2 << 30, nil } }, "disk", "blocked", ""},
		{"no claude", func(p *Probes) {
			p.Claude = func(context.Context) (string, error) { return "", errors.New("not found") }
		}, "claude", "needs_user", "install"},
		{"old claude", func(p *Probes) { p.Claude = func(context.Context) (string, error) { return "2.0.1", nil } }, "claude", "needs_user", "run_as_user"},
		{"no image", func(p *Probes) { p.GOARCH = "riscv64" }, "platform", "blocked", ""},
	}
	for _, c := range cases {
		p := healthy()
		c.mutate(&p)
		cs := Preflight(context.Background(), p)
		got := byID(cs)[c.id]
		if got.Status != Fail || got.Class != c.class || got.Fix == "" {
			t.Errorf("%s: %+v", c.name, got)
			continue
		}
		if (got.Action == nil) != (c.action == "") || (got.Action != nil && got.Action.Kind != c.action) {
			t.Errorf("%s: action %+v, want %q", c.name, got.Action, c.action)
		}
		if class, _ := Verdict(cs); class != c.class {
			t.Errorf("%s: verdict %q", c.name, class)
		}
	}
}

func TestAdoptedServerOwnsItsPortsAndDisk(t *testing.T) {
	p := healthy()
	p.PortFree = func(int) bool { return false }
	p.FreeBytes = func() (uint64, error) { return 3 << 30, nil }
	p.OwnPorts, p.ServerInstalled = []int{8888, 9999}, true
	cs := Preflight(context.Background(), p)
	if class, failed := Verdict(cs); class != "" {
		t.Fatalf("adoption blocked: %+v", failed)
	}
	if byID(cs)["disk"].Status != Warn {
		t.Errorf("low disk on an installed server must warn: %+v", byID(cs)["disk"])
	}
	// A port backsight's server does not publish stays another program's.
	p.UIPort = 9998
	cs = Preflight(context.Background(), p)
	if c := byID(cs)["ui_port"]; c.Status != Fail || !strings.Contains(c.Fix, "--answer ui_port=") {
		t.Errorf("a taken port backsight does not hold passed: %+v", c)
	}
}

func TestRemoteServerSkipsTheDockerChecks(t *testing.T) {
	p := healthy()
	p.Remote = true
	p.Docker = func(context.Context) (string, error) { t.Fatal("docker probed for a remote server"); return "", nil }
	cs := Preflight(context.Background(), p)
	for _, id := range []string{"api_port", "ui_port", "compose", "disk"} {
		if _, ok := byID(cs)[id]; ok {
			t.Errorf("%s checked for a remote server", id)
		}
	}
	if byID(cs)["docker"].Status != Info {
		t.Errorf("%+v", byID(cs)["docker"])
	}
}

func TestNeedsUserWinsTheVerdict(t *testing.T) {
	p := healthy()
	p.Docker = func(context.Context) (string, error) { return "", errors.New("Cannot connect") }
	p.Claude = func(context.Context) (string, error) { return "", errors.New("missing") }
	class, failed := Verdict(Preflight(context.Background(), p))
	if class != "needs_user" || len(failed) != 2 {
		t.Errorf("class %q failed %d", class, len(failed))
	}
}

func TestRootlessAndGPUAreReported(t *testing.T) {
	p := healthy()
	p.Docker = func(context.Context) (string, error) { return "28.1.1 [name=rootless]", nil }
	p.GPUs = []string{"AMD Radeon"}
	cs := byID(Preflight(context.Background(), p))
	if !strings.Contains(cs["docker"].Summary, "rootless") || !strings.Contains(cs["gpu"].Summary, "AMD Radeon") {
		t.Errorf("%+v %+v", cs["docker"], cs["gpu"])
	}
}

func TestNeedsUserWinsWhateverTheOrder(t *testing.T) {
	p := healthy()
	p.Docker = func(context.Context) (string, error) { return "", fmt.Errorf("x: %w", exec.ErrNotFound) } // needs_user, first
	p.FreeBytes = func() (uint64, error) { return 1 << 30, nil }                                          // blocked, later
	if class, failed := Verdict(Preflight(context.Background(), p)); class != "needs_user" || len(failed) != 2 {
		t.Errorf("class %q, %d failed", class, len(failed))
	}
}

// A machine without a systemd user session (a container, an su shell)
// cannot load the timers: the check fails as the user's choice (exit 10)
// naming scheduler=off and what stops without it; scheduler=off skips it.
func TestNoUserSessionIsTheUsersChoice(t *testing.T) {
	p := healthy()
	p.UserScheduler = func(context.Context) error { return errors.New("Failed to connect to bus: No medium found") }
	cs := Preflight(context.Background(), p)
	c := byID(cs)["scheduler"]
	if c.Status != Fail || c.Class != "needs_user" || !strings.Contains(c.Summary, "No medium found") {
		t.Fatalf("%+v", c)
	}
	for _, s := range []string{"scheduler=off", "jobs tick", "jobs maintain", "backup create", "login session"} {
		if !strings.Contains(c.Fix, s) {
			t.Errorf("fix lacks %q: %s", s, c.Fix)
		}
	}
	if c.Action == nil || !strings.Contains(c.Action.Cmd, "--answer scheduler=off") {
		t.Errorf("action %+v", c.Action)
	}
	if class, _ := Verdict(cs); class != "needs_user" {
		t.Errorf("verdict %q", class)
	}
	p.SchedulerOff = true
	if _, ok := byID(Preflight(context.Background(), p))["scheduler"]; ok {
		t.Error("scheduler=off still checks the user session")
	}
	p.SchedulerOff, p.UserScheduler = false, func(context.Context) error { return nil }
	if c := byID(Preflight(context.Background(), p))["scheduler"]; c.Status != OK {
		t.Errorf("reachable: %+v", c)
	}
}

// A user session that does not answer in time is retried from a login
// session (blocked), scheduler=off only the last resort.
func TestSlowUserSessionIsRetried(t *testing.T) {
	p := healthy()
	p.UserScheduler = func(context.Context) error { return fmt.Errorf("probe: %w", ErrSessionSlow) }
	c := byID(Preflight(context.Background(), p))["scheduler"]
	if c.Status != Fail || c.Class != "blocked" || c.Action != nil || !strings.HasPrefix(c.Fix, "retry, from a login session") || !strings.Contains(c.Fix, "jobs tick") {
		t.Errorf("%+v", c)
	}
}
