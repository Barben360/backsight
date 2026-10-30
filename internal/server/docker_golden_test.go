package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
)

// dockerGoldenConfigs are the configs whose rendered files were recorded
// from 0014c66, before the native runtime (M7 block 1): a Docker install
// renders them byte for byte, whatever the native runtime adds.
func dockerGoldenConfigs() map[string]config.Config {
	loopback := config.Default(paths.Dirs{Data: "/d"}, "/home/u")
	private := config.Default(paths.Dirs{Data: "/d"}, "/home/u")
	private.Network.Exposure, private.Network.BindIP = "private", "100.101.1.2"
	private.LLM.Provider, private.LLM.Model, private.LLM.BaseURL = "ollama", "qwen3:8b", "http://host.docker.internal:11434"
	return map[string]config.Config{"loopback": loopback, "private-ollama": private}
}

func TestDockerRenderIsUnchanged(t *testing.T) {
	for name, cfg := range dockerGoldenConfigs() {
		f, err := RenderImage(cfg, secrets, "box", pinnedHindsight)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		golden(t, "docker-golden/"+name+"/compose.yaml", f.Compose)
		golden(t, "docker-golden/"+name+"/.env", f.Env)
		// The runtime renders the same files.
		rf, err := Docker{Cfg: cfg, Image: pinnedHindsight}.Render(secrets, "box")
		if err != nil || string(rf.Compose) != string(f.Compose) || string(rf.Env) != string(f.Env) {
			t.Errorf("%s: Docker.Render differs from RenderImage (%v)", name, err)
		}
	}
}

func TestHindsightVersionOf(t *testing.T) {
	digest := "sha256:" + string(make64('a'))
	for ref, want := range map[string]string{
		"ghcr.io/vectorize-io/hindsight:1.1.0@" + digest: "1.1.0",
		"ghcr.io/vectorize-io/hindsight:1.2.10":          "1.2.10",
		"ghcr.io/vectorize-io/hindsight@" + digest:       "",      // a digest alone names no version
		"1.1.0-0123456789ab":                             "1.1.0", // a native version: <hindsight>-<lock hash12>
		"1.1.0":                                          "1.1.0",
		"ghcr.io/vectorize-io/hindsight:1.2.0-rc1":       "", // a pre-release is not that release
		"ghcr.io/vectorize-io/hindsight":                 "",
		"localhost:5000/hindsight@" + digest:             "", // a registry port is no tag
		"":                                               "",
	} {
		if got := HindsightVersionOf(ref); got != want {
			t.Errorf("HindsightVersionOf(%q) = %q, want %q", ref, got, want)
		}
	}
	if v := (Docker{Image: "ghcr.io/vectorize-io/hindsight:1.1.0@" + digest}).HindsightVersion(); v != "1.1.0" {
		t.Errorf("Docker.HindsightVersion %q", v)
	}
	cfg := config.Config{Server: config.Server{Digest: digest}}
	if v := (Docker{Cfg: cfg}).HindsightVersion(); v != "" {
		t.Errorf("a config digest gives %q", v)
	}
}

// What the native runtime needs from every runtime, on Docker: the
// database alone, Hindsight's own rename, the files holding the server's
// keys, and no unit file.
func TestDockerRuntimeSurface(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	var rt Runtime = Docker{Compose: Compose{Dir: dir, Runner: r, Project: "p1"}, Cfg: dockerGoldenConfigs()["loopback"]}
	ctx := context.Background()
	if err := rt.StartDatabase(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RenameBank(ctx, "claude-memory", "backsight", true); err != nil {
		t.Fatal(err)
	}
	base := []string{"compose", "-p", "p1", "--project-directory", dir, "-f", filepath.Join(dir, "compose.yaml")}
	want := [][]string{
		append(slices.Clone(base), "up", "-d", "--wait", "db"),
		append(slices.Clone(base), "exec", "-T", "hindsight", "hindsight-admin", "rename-bank", "--from", "claude-memory", "--to", "backsight", "--dry-run"),
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("got  %q\nwant %q", r.calls, want)
	}
	if got := rt.SecretFiles(); !reflect.DeepEqual(got, []string{filepath.Join(dir, ".env")}) {
		t.Errorf("secret files %q", got)
	}
	f, err := rt.Render(secrets, "box")
	if err != nil || f.Units != nil {
		t.Errorf("Docker renders units %v (%v)", f.Units, err)
	}
	ch, err := rt.Write(f)
	if err != nil || ch.Units {
		t.Errorf("Docker changed units: %+v %v", ch, err)
	}
	raw, _ := json.Marshal(ch)
	if strings.Contains(string(raw), "units_changed") {
		t.Errorf("unchanged units reported: %s", raw)
	}
	if raw, _ := json.Marshal(Changes{Units: true}); !strings.Contains(string(raw), `"units_changed":true`) {
		t.Errorf("changed units not reported: %s", raw)
	}
}

// The data's size is the two volumes', as docker system df reports them.
func TestDockerDataSize(t *testing.T) {
	r := &fakeRunner{out: []byte(`[{"Name":"other_pg_data","Size":"9GB"},{"Name":"p1_pg_data","Size":"48.25MB"},` +
		`{"Name":"p1_hf_cache","Size":"1.2GB"},{"Name":"p1_x","Size":"3kB"}]` + "\n")}
	var sz Sizer = Docker{Compose: Compose{Dir: t.TempDir(), Runner: r, Project: "p1"}}
	ctx := context.Background()
	if n, err := sz.DataSize(ctx); err != nil || n != 48_250_000+1_200_000_000 {
		t.Errorf("size %d %v", n, err)
	}
	if !reflect.DeepEqual(r.calls, [][]string{{"system", "df", "-v", "--format", "{{json .Volumes}}"}}) {
		t.Errorf("calls %q", r.calls)
	}
	for out, want := range map[string]int64{"[]\n": 0, `[{"Name":"p1_pg_data","Size":"0B"},{"Name":"p1_hf_cache","Size":"512B"}]`: 512,
		`[{"Name":"p1_pg_data","Size":"3.057GB"}]`: 3_057_000_000} {
		r.out = []byte(out)
		if n, err := sz.DataSize(ctx); err != nil || n != want {
			t.Errorf("%s: %d %v, want %d", out, n, err, want)
		}
	}
	r.out = []byte(`[{"Name":"p1_pg_data","Size":"N/A"}]`)
	if _, err := sz.DataSize(ctx); err == nil || !strings.Contains(err.Error(), "p1_pg_data") {
		t.Errorf("an unknown size: %v", err)
	}
	r.out, r.err = []byte("Cannot connect to the Docker daemon"), errors.New("exit status 1")
	if _, err := sz.DataSize(ctx); err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Errorf("docker failing: %v", err)
	}
}

// The database's SQL runs over any query function: the native runtime's
// database reuses it with its own psql.
func TestDatabaseSQLRunsOverAnyQuery(t *testing.T) {
	var asked []string
	q := func(_ context.Context, sql string) (string, error) {
		asked = append(asked, sql)
		switch {
		case strings.Contains(sql, "json_agg(bank_id"):
			return `["a","b"]`, nil
		case strings.Contains(sql, "async_operations"):
			return "3", nil
		case strings.Contains(sql, "retain_params"):
			return `{"k":"v"}`, nil
		}
		return "7", nil
	}
	ctx := context.Background()
	ids, err := bankIDs(ctx, q)
	if err != nil || !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Errorf("banks %q %v", ids, err)
	}
	n, err := pendingOperations(ctx, q, []string{"a", "o'b"})
	if err != nil || n != 3 || !strings.Contains(asked[len(asked)-1], "bank_id in ('a', 'o''b')") {
		t.Errorf("pending %d %v %q", n, err, asked[len(asked)-1])
	}
	meta, err := documentMetadata(ctx, q, "a", "d1")
	if err != nil || meta["k"] != "v" {
		t.Errorf("metadata %v %v", meta, err)
	}
	c, err := counts(ctx, q, []string{"memory_units"})
	if err != nil || c["memory_units"] != 7 {
		t.Errorf("counts %v %v", c, err)
	}
	// And DB is the Database interface, pending operations included.
	var _ Database = DB{}
}
