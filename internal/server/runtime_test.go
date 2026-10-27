package server

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

// Docker CLI 26 exits 0 when it cannot reach the daemon and prints the
// error where the version goes; 29 exits 1. Either way the engine is not
// usable, Docker's own words reach the caller, and no word of the error is
// taken for a version.
func TestDockerEngineUnreachableOnDocker26And29(t *testing.T) {
	ctx := context.Background()
	const denied = `permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.45/info": dial unix /var/run/docker.sock: connect: permission denied` + "\n \n"
	const down = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n \n"
	cases := []struct {
		name, out string
		err       error
		want      string
	}{
		{"26 permission", denied, nil, "permission denied"},
		{"26 daemon down", down, nil, "Cannot connect to the Docker daemon"},
		{"29 permission", denied, errors.New("exit status 1"), "permission denied"},
		{"29 daemon down", down, errors.New("exit status 1"), "Cannot connect to the Docker daemon"},
		{"26 empty", " \n", nil, "no version"},
	}
	for _, c := range cases {
		e, err := Docker{Compose: Compose{Runner: &fakeRunner{out: []byte(c.out), err: c.err}}}.Engine(ctx)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: engine %+v, err %v; want an error containing %q", c.name, e, err, c.want)
		}
		if e.Version != "" {
			t.Errorf("%s: version %q from an error", c.name, e.Version)
		}
	}
	// Versions as each release prints them stay versions.
	for _, v := range []string{"26.1.5", "29.0.1", "28.1.1-rd", "27.3.1+dfsg1"} {
		if e, err := (Docker{Compose: Compose{Runner: &fakeRunner{out: []byte(v + " name=seccomp,profile=builtin \n")}}}).Engine(ctx); err != nil || e.Version != v {
			t.Errorf("%s: %+v %v", v, e, err)
		}
	}
}

// scriptedRunner answers each docker command by its first words.
type scriptedRunner struct {
	calls   [][]string
	answers map[string]struct {
		out string
		err error
	}
}

func (r *scriptedRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	line := strings.Join(args, " ")
	for prefix, a := range r.answers {
		if strings.HasPrefix(line, prefix) {
			return []byte(a.out), a.err
		}
	}
	return nil, nil
}

// renderedProject writes the files Render builds for project p1 in dir.
func renderedProject(t *testing.T, dir string) {
	t.Helper()
	d := Docker{Compose: Compose{Dir: dir, Project: "p1"}, Cfg: config.Config{Server: config.Server{URL: "http://127.0.0.1:8888", UIPort: 9999, Project: "p1"},
		Embeddings: config.Embeddings{Preset: "multilingual"}}}
	f, err := d.Render(config.Secrets{APIKey: "k", UIKey: "u", DBPassword: "p", LLMAPIKey: "l"}, "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(f); err != nil {
		t.Fatal(err)
	}
}

// Delete takes the project down without its volumes, then removes each
// volume backsight's compose.yaml declares and the files Render wrote: a
// volume only the user's override declares stays, and so does the
// override. A failed down removes nothing.
func TestDockerDeleteRemovesServerDataAndItsFiles(t *testing.T) {
	dir := t.TempDir()
	renderedProject(t, dir)
	override := "services:\n  hindsight:\n    volumes:\n      - models:/models\nvolumes:\n  models:\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.override.yaml"), []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	failing := &fakeRunner{out: []byte("Cannot connect to the Docker daemon"), err: errors.New("exit status 1")}
	if err := (Docker{Compose: Compose{Dir: dir, Runner: failing}}).Delete(context.Background()); err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Fatalf("a failed removal: %v", err)
	}
	if len(failing.calls) != 1 {
		t.Errorf("went on after a failed removal: %q", failing.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
		t.Fatal("a failed down removed the files")
	}
	r := &scriptedRunner{}
	if err := (Docker{Compose: Compose{Dir: dir, Runner: r, Project: "p1"}}).Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := []string{"compose", "-p", "p1", "--project-directory", dir, "-f", filepath.Join(dir, "compose.yaml"), "-f", filepath.Join(dir, "compose.override.yaml")}
	// The containers go first with their anonymous volumes (the database
	// image declares one at /var/lib/postgresql), which down would leave.
	want := [][]string{append(append([]string{}, base...), "rm", "--stop", "--force", "-v"), append(append([]string{}, base...), "down", "--remove-orphans"), {"volume", "rm", "p1_pg_data"}, {"volume", "rm", "p1_hf_cache"}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("docker ran\n%q\nwant\n%q", r.calls, want)
	}
	for f, want := range map[string]bool{"compose.yaml": false, ".env": false, "compose.override.yaml": true} {
		if _, err := os.Stat(filepath.Join(dir, f)); (err == nil) != want {
			t.Errorf("%s present: %v, want %v", f, err == nil, want)
		}
	}
}

// The volumes are read from the compose file's top-level volumes section,
// whatever they are called; a service's own volumes list is no volume of
// the project's, and a file without the section declares none.
func TestDeclaredVolumes(t *testing.T) {
	compose := "name: p1\n\nservices:\n  db:\n    volumes:\n      - pg_data:/var/lib/pg\n\nvolumes:\n  pg_data:\n  hf_cache:\n  extra: {}\n  # a comment\n\nnetworks:\n  default:\n"
	if got := DeclaredVolumes([]byte(compose)); strings.Join(got, ",") != "pg_data,hf_cache,extra" {
		t.Errorf("volumes %q", got)
	}
	if got := DeclaredVolumes([]byte("services:\n  web:\n    volumes:\n      - x:/x\n")); len(got) != 0 {
		t.Errorf("no top-level section: %q", got)
	}
}

// A data volume already gone is what Delete wants; any other failure to
// remove it is an error, and the files stay for a rerun.
func TestDockerDeleteDataVolumeRemoval(t *testing.T) {
	for _, c := range []struct {
		name, out string
		wantErr   bool
	}{
		{"already gone", "Error response from daemon: get p1_pg_data: no such volume", false},
		{"in use", "Error response from daemon: remove p1_pg_data: volume is in use - [abc]", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range []string{"compose.yaml", ".env"} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r := &scriptedRunner{answers: map[string]struct {
				out string
				err error
			}{"volume rm": {c.out, errors.New("exit status 1")}}}
			err := (Docker{Compose: Compose{Dir: dir, Runner: r, Project: "p1"}}).Delete(context.Background())
			if (err != nil) != c.wantErr || (err != nil && !strings.Contains(err.Error(), "in use")) {
				t.Fatalf("err %v, want error %v", err, c.wantErr)
			}
			if _, statErr := os.Stat(filepath.Join(dir, ".env")); (statErr == nil) != c.wantErr {
				t.Errorf(".env present %v after %v", statErr == nil, err)
			}
		})
	}
}

func TestDockerStopCmd(t *testing.T) {
	dir := t.TempDir()
	if cmd := (Docker{Compose: Compose{Dir: dir, Project: "p1"}}).StopCmd(); !strings.Contains(cmd, "docker compose -p p1 ") || !strings.HasSuffix(cmd, " stop") {
		t.Errorf("stop by hand: %q", cmd)
	}
	// macOS's data directory has a space: the command stays one word per path.
	if cmd := (Docker{Compose: Compose{Dir: "/Users/u/Library/Application Support/backsight/server"}}).StopCmd(); !strings.Contains(cmd, "'/Users/u/Library/Application Support/backsight/server'") {
		t.Errorf("unquoted path: %q", cmd)
	}
}

// streamRunner is a scriptedRunner whose pulls stream progress lines.
type streamRunner struct {
	scriptedRunner
	pullErr error
	stdout  io.Writer
	// failures: the first pulls fail with failLine, then they pass.
	failures int
	failLine string
}

func (r *streamRunner) Stream(_ context.Context, w, errw io.Writer, args ...string) error {
	r.calls = append(r.calls, args)
	r.stdout = w
	_, _ = io.WriteString(w, "a1b2: Pulling fs layer\na1b2: Downloading [=====>     ]  52MB/1GB\n")
	if r.pullErr != nil {
		_, _ = io.WriteString(errw, "Error response from daemon: Get \"https://ghcr.io/v2/\": dial tcp: lookup ghcr.io: no such host\n")
		return r.pullErr
	}
	if r.failures > 0 {
		r.failures--
		_, _ = io.WriteString(errw, r.failLine+"\n")
		return errors.New("exit status 1")
	}
	_, _ = io.WriteString(w, "a1b2: Pull complete\n")
	return nil
}

// Downloads names the images Docker lacks and a missing model cache;
// Download streams Docker's progress and keeps its last words on failure.
func TestDockerDownloads(t *testing.T) {
	ctx := context.Background()
	oldBackoff := PullBackoff
	PullBackoff = time.Millisecond
	t.Cleanup(func() { PullBackoff = oldBackoff })
	noImage := struct {
		out string
		err error
	}{"Error response from daemon: No such image: x", errors.New("exit status 1")}
	r := &streamRunner{scriptedRunner: scriptedRunner{answers: map[string]struct {
		out string
		err error
	}{
		"image inspect --format {{.Id}} " + HindsightImage: noImage,
		"volume inspect p1_hf_cache":                       {"Error response from daemon: get p1_hf_cache: no such volume", errors.New("exit status 1")},
	}}}
	d := Docker{Compose: Compose{Runner: r, Project: "p1"}}
	images, models, err := d.Downloads(ctx)
	if err != nil || len(images) != 1 || images[0] != HindsightImage || !models {
		t.Fatalf("downloads %v %v %v", images, models, err)
	}
	if mb := DownloadMB(images, models); mb != HindsightImageMB+ModelsMB {
		t.Errorf("%d MB", mb)
	}
	var shown strings.Builder
	if err := d.Download(ctx, images, &shown); err != nil || !strings.Contains(shown.String(), "Downloading [=====>") {
		t.Errorf("progress %q %v", shown.String(), err)
	}
	if last := r.calls[len(r.calls)-1]; strings.Join(last, " ") != "pull "+HindsightImage {
		t.Errorf("pull %q", last)
	}
	// Docker's stdout is the writer itself: a terminal (an *os.File) stays
	// one, and Docker draws its progress bars.
	if r.stdout != io.Writer(&shown) {
		t.Errorf("stdout wrapped: %T", r.stdout)
	}
	r.pullErr = errors.New("exit status 1")
	if err := d.Download(ctx, images, io.Discard); err == nil || !strings.Contains(err.Error(), "no such host") {
		t.Errorf("a failed pull must say why: %v", err)
	}

	// Everything there: nothing to download.
	all := &scriptedRunner{}
	if images, models, err := (Docker{Compose: Compose{Runner: all, Project: "p1"}}).Downloads(ctx); err != nil || len(images) != 0 || models {
		t.Errorf("cached: %v %v %v", images, models, err)
	}
	// Docker unusable: an error, never "everything to download".
	down := &scriptedRunner{answers: map[string]struct {
		out string
		err error
	}{"image inspect": {"Cannot connect to the Docker daemon", errors.New("exit status 1")}}}
	if _, _, err := (Docker{Compose: Compose{Runner: down}}).Downloads(ctx); err == nil {
		t.Error("an unusable Docker reads as a download")
	}
}

// Untagged images are removed by repo@digest: the tag goes, a registry's
// port stays.
func TestImagesRemoveCmdNamesImagesByDigest(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/vectorize-io/hindsight:1.1.0@sha256:abc": "ghcr.io/vectorize-io/hindsight@sha256:abc",
		"localhost:5000/hindsight:dev@sha256:abc":         "localhost:5000/hindsight@sha256:abc",
		"localhost:5000/hindsight@sha256:abc":             "localhost:5000/hindsight@sha256:abc",
		"hindsight:dev":                                   "hindsight:dev",
	} {
		if got := byDigest(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	dir := t.TempDir()
	renderedProject(t, dir)
	cmd := Docker{Compose: Compose{Dir: dir, Project: "p1"}}.ImagesRemoveCmd()
	if !strings.HasPrefix(cmd, "docker image rm ghcr.io/vectorize-io/hindsight@sha256:") || !strings.HasSuffix(cmd, " pgvector/pgvector@sha256:"+strings.SplitN(PostgresImage, "@sha256:", 2)[1]) {
		t.Errorf("%q", cmd)
	}
}

const unpackLine = `failed to extract layer (application/vnd.oci.image.layer.v1.tar+gzip sha256:5031) to overlayfs as "extract-1": ` +
	`failed to unmount /var/lib/containerd/tmpmounts/containerd-mount123: device or resource busy`

// A pull that fails is tried again: Docker keeps the layers it has, and
// the second pull passes (issue 60). Every attempt failing names the cause
// from Docker's last line.
func TestDownloadRetriesAndNamesTheCause(t *testing.T) {
	ctx := context.Background()
	oldBackoff := PullBackoff
	PullBackoff = time.Millisecond
	t.Cleanup(func() { PullBackoff = oldBackoff })
	r := &streamRunner{failures: 1, failLine: unpackLine}
	d := Docker{Compose: Compose{Runner: r, Project: "p1"}}
	var shown strings.Builder
	if err := d.Download(ctx, []string{HindsightImage}, &shown); err != nil || len(r.calls) != 2 || !strings.Contains(shown.String(), "trying again (2/3)") {
		t.Fatalf("err %v, %d pulls\n%s", err, len(r.calls), shown.String())
	}
	for line, want := range map[string]string{
		unpackLine: PullUnpack,
		"Error response from daemon: Get \"https://ghcr.io/v2/\": dial tcp: lookup ghcr.io: no such host": PullNetwork,
		"Error response from daemon: received unexpected HTTP status: 503 Service Unavailable":            PullNetwork,
		"write /var/lib/docker/tmp/x: no space left on device":                                            PullDisk,
		"Error response from daemon: manifest unknown":                                                    "",
	} {
		r := &streamRunner{failures: PullAttempts, failLine: line}
		err := Docker{Compose: Compose{Runner: r, Project: "p1"}}.Download(ctx, []string{PostgresImage}, io.Discard)
		var pe *PullError
		if !errors.As(err, &pe) || pe.Cause() != want || len(r.calls) != PullAttempts || pe.Image != PostgresImage {
			t.Errorf("%q: %v (cause %q, want %q), %d pulls", line, err, causeOf(pe), want, len(r.calls))
		}
	}
}

func causeOf(e *PullError) string {
	if e == nil {
		return "no PullError"
	}
	return e.Cause()
}
