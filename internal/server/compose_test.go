package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls [][]string
	out   []byte
	err   error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return f.out, f.err
}

func TestComposeArgs(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	c := Compose{Dir: dir, Runner: r}
	if err := c.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"compose", "-p", "backsight", "--project-directory", dir, "-f", filepath.Join(dir, "compose.yaml"), "up", "-d", "--wait"}
	if !reflect.DeepEqual(r.calls[0], want) {
		t.Errorf("got  %v\nwant %v", r.calls[0], want)
	}
	// A user override is merged after the generated file.
	_ = os.WriteFile(filepath.Join(dir, "compose.override.yaml"), []byte("services: {}\n"), 0o644)
	_ = c.Stop(context.Background())
	want = []string{"compose", "-p", "backsight", "--project-directory", dir, "-f", filepath.Join(dir, "compose.yaml"), "-f", filepath.Join(dir, "compose.override.yaml"), "stop"}
	if !reflect.DeepEqual(r.calls[1], want) {
		t.Errorf("got  %v\nwant %v", r.calls[1], want)
	}
	_ = c.Pull(context.Background())
	if last := r.calls[2]; last[len(last)-1] != "pull" {
		t.Errorf("pull: %v", last)
	}
}

func TestComposeErrorCarriesOutput(t *testing.T) {
	r := &fakeRunner{out: []byte("port is already allocated"), err: errors.New("exit status 1")}
	err := Compose{Dir: t.TempDir(), Runner: r}.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "port is already allocated") {
		t.Errorf("got %v", err)
	}
}

func TestPSParsesBothFormats(t *testing.T) {
	for name, out := range map[string]string{
		"json lines": `{"Service":"db","State":"running","Health":"healthy"}
{"Service":"hindsight","State":"running","Health":""}
`,
		"array": `[{"Service":"db","State":"running","Health":"healthy"},{"Service":"hindsight","State":"running","Health":""}]`,
	} {
		c := Compose{Dir: t.TempDir(), Runner: &fakeRunner{out: []byte(out)}}
		got, err := c.PS(context.Background())
		want := []Container{{Service: "db", State: "running", Health: "healthy"}, {Service: "hindsight", State: "running"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	c := Compose{Dir: t.TempDir(), Runner: &fakeRunner{out: []byte("")}}
	if got, err := c.PS(context.Background()); err != nil || len(got) != 0 {
		t.Errorf("no containers: %+v %v", got, err)
	}
}

func TestVolumeExists(t *testing.T) {
	cases := []struct {
		out    string
		err    error
		exists bool
		fails  bool
	}{
		{`[{"Name":"backsight_pg_data"}]`, nil, true, false},
		{"Error response from daemon: get backsight_pg_data: no such volume", errors.New("exit status 1"), false, false},
		{"Cannot connect to the Docker daemon", errors.New("exit status 1"), false, true},
	}
	for _, c := range cases {
		r := &fakeRunner{out: []byte(c.out), err: c.err}
		got, err := Compose{Dir: t.TempDir(), Runner: r}.VolumeExists(context.Background())
		if got != c.exists || (err != nil) != c.fails {
			t.Errorf("%q: %v %v", c.out, got, err)
		}
		if strings.Join(r.calls[0], " ") != "volume inspect backsight_pg_data" {
			t.Errorf("args %v", r.calls[0])
		}
	}
}

func TestLogsArgs(t *testing.T) {
	r := &fakeRunner{out: []byte("line")}
	out, err := Compose{Dir: t.TempDir(), Runner: r}.Logs(context.Background(), "hindsight", 20)
	if err != nil || out != "line" || strings.Join(r.calls[0][len(r.calls[0])-5:], " ") != "logs --no-color --tail 20 hindsight" {
		t.Errorf("%q %v %v", out, err, r.calls[0])
	}
}
