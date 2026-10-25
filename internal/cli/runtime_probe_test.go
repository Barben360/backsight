package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/paths"
)

// engineDocker answers the engine probes and records what reached it.
type engineDocker struct {
	calls []string
	info  string
	ps    string
}

func (f *engineDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case args[0] == "info":
		return []byte(f.info), nil
	case args[0] == "compose" && args[1] == "version":
		return []byte("2.39.1\n"), nil
	case strings.Contains(strings.Join(args, " "), " ps "):
		return []byte(f.ps), nil
	}
	return nil, nil
}

func withEngine(t *testing.T, f *engineDocker) {
	t.Helper()
	old := composeRunner
	composeRunner = f
	t.Cleanup(func() { composeRunner = old })
}

// Setup's pre-flight reads Docker, Compose and its own server through the
// server runtime: a rootless engine is reported, and a running hindsight
// service makes the taken ports ours.
func TestSetupProbesAskTheServerRuntime(t *testing.T) {
	isolate(t)
	f := &engineDocker{info: "28.1.1 name=seccomp name=rootless\n", ps: `{"Service":"hindsight","State":"running","Health":""}` + "\n"}
	withEngine(t, f)
	tmp := t.TempDir()
	cfg := config.Config{Server: config.Server{ComposeDir: filepath.Join(tmp, "server"), Project: "p1", Mode: "local"}}
	s := setupEnv{dirs: paths.Dirs{Data: tmp}, cfg: &cfg, loc: claudecode.Location{Dir: tmp, Settings: filepath.Join(tmp, "settings.json"), Projects: filepath.Join(tmp, "projects")}}
	p := preflightProbes(context.Background(), s, map[string]string{"api_port": "8888", "ui_port": "9999"})
	if v, err := p.Docker(context.Background()); err != nil || v != "28.1.1 rootless" {
		t.Errorf("docker %q %v", v, err)
	}
	if v, err := p.Compose(context.Background()); err != nil || v != "2.39.1" {
		t.Errorf("compose %q %v", v, err)
	}
	if !p.OwnServer {
		t.Errorf("a running hindsight service is not ours; calls %q", f.calls)
	}
	if p.ServerInstalled {
		t.Error("no compose.yaml, yet the server counts as installed")
	}
}

// doctor's Docker probe is the runtime's engine version.
func TestDoctorDockerProbeAsksTheServerRuntime(t *testing.T) {
	f := &engineDocker{info: "27.3.1 name=seccomp\n"}
	withEngine(t, f)
	var e doctor.Env
	realProbes(&e)
	if v, err := e.Docker(context.Background()); err != nil || v != "27.3.1" {
		t.Errorf("docker %q %v", v, err)
	}
	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "info ") {
		t.Errorf("calls %q", f.calls)
	}
}
