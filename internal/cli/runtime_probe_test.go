package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/setup"
)

// engineDocker answers the engine probes and records what reached it.
type engineDocker struct {
	calls   []string
	info    string
	infoErr error
	ps      string
	// A machine that never ran the server: no image, no model cache; a
	// pull takes pullTime and fails with pullErr.
	fresh    bool
	pullTime time.Duration
	pullErr  error
}

func (f *engineDocker) Stream(_ context.Context, w io.Writer, args ...string) error {
	f.calls = append(f.calls, strings.Join(args, " "))
	_, _ = io.WriteString(w, "a1b2: Downloading [=====>      ]  52.4MB/1GB\n")
	time.Sleep(f.pullTime)
	if f.pullErr != nil {
		_, _ = io.WriteString(w, "Error response from daemon: Get \"https://ghcr.io/v2/\": dial tcp: lookup ghcr.io: no such host\n")
		return f.pullErr
	}
	_, _ = io.WriteString(w, "a1b2: Pull complete\n")
	return nil
}

func (f *engineDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case f.fresh && args[0] == "image" && args[1] == "inspect":
		return []byte("Error response from daemon: No such image: " + args[len(args)-1]), errors.New("exit status 1")
	case f.fresh && args[0] == "volume" && args[1] == "inspect":
		return []byte("Error response from daemon: get " + args[2] + ": no such volume"), errors.New("exit status 1")
	case args[0] == "info":
		return []byte(f.info), f.infoErr
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
	cfg := config.Config{Server: config.Server{ComposeDir: filepath.Join(tmp, "server"), Project: "p1", Mode: "local", URL: "http://127.0.0.1:8888", UIPort: 9999}}
	s := setupEnv{dirs: paths.Dirs{Data: tmp}, cfg: &cfg, loc: claudecode.Location{Dir: tmp, Settings: filepath.Join(tmp, "settings.json"), Projects: filepath.Join(tmp, "projects")}}
	p := preflightProbes(context.Background(), s, map[string]string{"api_port": "8888", "ui_port": "9999", "project": "p1"})
	if v, err := p.Docker(context.Background()); err != nil || v != "28.1.1 rootless" {
		t.Errorf("docker %q %v", v, err)
	}
	if v, err := p.Compose(context.Background()); err != nil || v != "2.39.1" {
		t.Errorf("compose %q %v", v, err)
	}
	if len(p.OwnPorts) != 2 || p.OwnPorts[0] != 8888 || p.OwnPorts[1] != 9999 {
		t.Errorf("a running hindsight service does not own its ports (%v); calls %q", p.OwnPorts, f.calls)
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

// The server counts as installed once a compose.yaml backsight rendered
// is there; one without a hindsight service (not backsight's) does not
// count.
func TestSetupProbesServerInstalled(t *testing.T) {
	isolate(t)
	withEngine(t, &engineDocker{info: "28.1.1\n"})
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "server")
	cfg := config.Config{Server: config.Server{ComposeDir: dir, Project: "p1", Mode: "local", URL: "http://127.0.0.1:8888", UIPort: 9999},
		Embeddings: config.Embeddings{Preset: "multilingual"}}
	s := setupEnv{dirs: paths.Dirs{Data: tmp}, cfg: &cfg, loc: claudecode.Location{Dir: tmp, Settings: filepath.Join(tmp, "settings.json"), Projects: filepath.Join(tmp, "projects")}}
	rt := runtimeFor(cfg)
	f, err := rt.Render(config.Secrets{APIKey: "k", UIKey: "u", DBPassword: "p", LLMAPIKey: "l"}, "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Write(f); err != nil {
		t.Fatal(err)
	}
	if p := preflightProbes(context.Background(), s, map[string]string{"api_port": "8888", "ui_port": "9999"}); !p.ServerInstalled {
		t.Error("a rendered compose.yaml does not count as installed")
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: nginx:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := preflightProbes(context.Background(), s, map[string]string{"api_port": "8888", "ui_port": "9999"}); p.ServerInstalled {
		t.Error("a compose.yaml without a hindsight service counts as installed")
	}
}

// Docker CLI 26 exits 0 when the daemon cannot be reached, 29 exits 1:
// on both, pre-flight gives the docker-group fix without the group and the
// start fix with the daemon down, and doctor never shows an error's first
// word as Docker's version.
func TestEngineProbesOnAnUnusableDocker(t *testing.T) {
	isolate(t)
	const denied = "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get \"http://%2Fvar%2Frun%2Fdocker.sock/v1.45/info\": dial unix /var/run/docker.sock: connect: permission denied\n \n"
	const down = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n \n"
	for _, c := range []struct {
		name, info string
		err        error
		class      string
		fix        string
	}{
		{"26 no group", denied, nil, "needs_user", "usermod -aG docker"},
		{"29 no group", denied, errors.New("exit status 1"), "needs_user", "usermod -aG docker"},
		{"26 daemon down", down, nil, "blocked", "start Docker"},
		{"29 daemon down", down, errors.New("exit status 1"), "blocked", "start Docker"},
	} {
		withEngine(t, &engineDocker{info: c.info, infoErr: c.err})
		tmp := t.TempDir()
		s := setupEnv{dirs: paths.Dirs{Data: tmp}, loc: claudecode.Location{Dir: tmp, Settings: filepath.Join(tmp, "settings.json"), Projects: filepath.Join(tmp, "projects")}}
		p := preflightProbes(context.Background(), s, map[string]string{"api_port": "8888", "ui_port": "9999"})
		var got setup.Check
		for _, ch := range setup.Preflight(context.Background(), p) {
			if ch.ID == "docker" {
				got = ch
			}
		}
		if got.Status != setup.Fail || got.Class != c.class || !strings.Contains(got.Fix, c.fix) {
			t.Errorf("%s: pre-flight %+v", c.name, got)
		}
		var e doctor.Env
		realProbes(&e)
		if v, err := e.Docker(context.Background()); err == nil || v != "" {
			t.Errorf("%s: doctor's probe %q %v", c.name, v, err)
		}
	}
}
