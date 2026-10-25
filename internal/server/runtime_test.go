package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
)

// Deployed reads back the image Render wrote for the hindsight service,
// never the database's, and the version a runtime deploys follows
// WithVersion.
func TestDockerDeployedIsTheRenderedHindsightImage(t *testing.T) {
	dir := t.TempDir()
	d := Docker{Compose: Compose{Dir: dir}, Cfg: config.Config{Server: config.Server{URL: "http://127.0.0.1:8888", UIPort: 9999}, Embeddings: config.Embeddings{Preset: "multilingual"}}}
	if v, err := d.Deployed(); err != nil || v != "" {
		t.Fatalf("nothing rendered: %q %v", v, err)
	}
	other := "ghcr.io/vectorize-io/hindsight:1.2.0@sha256:" + string(make64('c'))
	rt := d.WithVersion(other)
	if rt.Version() != other || d.Version() != HindsightImage {
		t.Fatalf("versions %q %q", rt.Version(), d.Version())
	}
	f, err := rt.Render(config.Secrets{APIKey: "k", UIKey: "u", DBPassword: "p", LLMAPIKey: "l"}, "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Write(f); err != nil {
		t.Fatal(err)
	}
	if v, err := d.Deployed(); err != nil || v != other {
		t.Errorf("deployed %q %v, want %q", v, err, other)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if ServiceImage(raw, "db") != PostgresImage {
		t.Errorf("db image %q", ServiceImage(raw, "db"))
	}
}

func make64(c byte) []byte {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return b
}

// Each lifecycle call the commands make reaches Compose as the same
// command they used to run directly.
func TestDockerRuntimeDrivesCompose(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	d := Docker{Compose: Compose{Dir: dir, Runner: r, Project: "p1"}}
	ctx := context.Background()
	_ = d.Stop(ctx)
	_ = d.FetchAll(ctx)
	_, _ = d.Status(ctx)
	_, _ = d.Logs(ctx, "hindsight", 20)
	_, _ = d.DataExists(ctx)
	_, _ = d.Orchestrator(ctx)
	base := []string{"compose", "-p", "p1", "--project-directory", dir, "-f", filepath.Join(dir, "compose.yaml")}
	want := [][]string{
		append(append([]string{}, base...), "stop"),
		append(append([]string{}, base...), "pull"),
		append(append([]string{}, base...), "ps", "--all", "--format", "json"),
		append(append([]string{}, base...), "logs", "--no-color", "--tail", "20", "hindsight"),
		{"volume", "inspect", "p1_pg_data"},
		{"compose", "version", "--short"},
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("got  %q\nwant %q", r.calls, want)
	}
	if d.DataName() != "the database volume p1_pg_data" || d.DataRemoveCmd() != "docker volume rm p1_pg_data" {
		t.Errorf("data %q, %q", d.DataName(), d.DataRemoveCmd())
	}
}

func TestDockerEngine(t *testing.T) {
	ctx := context.Background()
	eng := func(out string, err error) (Engine, error) {
		return Docker{Compose: Compose{Runner: &fakeRunner{out: []byte(out), err: err}}}.Engine(ctx)
	}
	if e, err := eng("28.1.1 name=seccomp,profile=builtin name=rootless name=cgroupns\n", nil); err != nil || e != (Engine{Version: "28.1.1", Rootless: true}) {
		t.Errorf("rootless: %+v %v", e, err)
	}
	if e, err := eng("28.1.1 name=seccomp,profile=builtin\n", nil); err != nil || e != (Engine{Version: "28.1.1"}) {
		t.Errorf("rootful: %+v %v", e, err)
	}
	// The daemon's own words reach the caller, which tells a missing
	// group membership from a stopped daemon.
	if _, err := eng("permission denied while trying to connect to the Docker daemon socket", errors.New("exit status 1")); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("permission: %v", err)
	}
	if _, err := eng("", &exec.Error{Name: "docker", Err: exec.ErrNotFound}); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("not installed: %v", err)
	}
}
