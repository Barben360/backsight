package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/testtool"
)

const pkg = "github.com/Barben360/backsight"

// buildFor builds backsight for goos/goarch where goreleaser puts that
// target's binary in dist, with extra ldflags and go build flags, and
// returns its path.
func buildFor(t *testing.T, dist, goos, goarch, ldflags string, extra ...string) string {
	t.Helper()
	name := "backsight"
	if goos == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dist, "backsight_"+goos+"_"+goarch+"_v1", name)
	env := append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	sourcesAreInputs(t, env, extra...)
	// -trimpath as goreleaser builds: the build info then has no -ldflags.
	cmd := exec.Command("go", append(append([]string{"build", "-trimpath"}, extra...), "-o", out, "-ldflags", ldflags, "./cmd/backsight")...)
	cmd.Dir = ".."
	cmd.Env = env
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// sourcesAreInputs stats every file of this module that the build of
// ./cmd/backsight with these flags compiles or embeds (go.mod and go.sum
// too): Go's test cache keys on the files a test opens or stats, never on
// what a go build child reads, so a source change reruns these tests.
func sourcesAreInputs(t *testing.T, env []string, flags ...string) {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := `{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .EmbedFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}`
	cmd := exec.Command("go", append(append([]string{"list", "-deps"}, flags...), "-f", tmpl, "./cmd/backsight")...)
	cmd.Dir, cmd.Env = root, env
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	files := []string{filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum")}
	for _, f := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasPrefix(f, root+string(filepath.Separator)) {
			files = append(files, f)
		}
	}
	if len(files) < 10 {
		t.Fatalf("go list found %d source files of the module", len(files))
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
}

func buildInto(t *testing.T, dist, ldflags string, extra ...string) string {
	t.Helper()
	return buildFor(t, dist, runtime.GOOS, runtime.GOARCH, ldflags, extra...)
}

func verifyKeys(t *testing.T, args ...string) (string, error) {
	t.Helper()
	for _, f := range []string{"../Makefile", "verify-keys.sh"} { // inputs of the cache key, as above
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
	b, err := exec.Command("make", append([]string{"-s", "-C", "..", "verify-keys"}, args...)...).CombinedOutput()
	return string(b), err
}

// needTools skips without make or jq, except on CI, where a skip would
// silently drop the only proof that a test build cannot be released.
func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"make", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s missing on CI: the release check cannot be proved", tool)
			}
			t.Skip(tool + " missing")
		}
	}
}

// sourcePin is the Hindsight image the source pins.
func sourcePin(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../internal/server/render.go")
	m := regexp.MustCompile(`(?m)^const pinnedHindsight = "(.+)"$`).FindSubmatch(src)
	if err != nil || m == nil {
		t.Fatalf("no pinnedHindsight in render.go: %v", err)
	}
	return string(m[1])
}

type versionData struct {
	SigningKeys []struct {
		Role string `json:"role"`
	} `json:"signing_keys"`
	TestKey        bool   `json:"test_key"`
	HindsightImage string `json:"hindsight_image"`
}

func versionOf(t *testing.T, bin string) versionData {
	t.Helper()
	raw, err := exec.Command(bin, "version", "--json").Output()
	var env struct{ Data versionData }
	if err != nil || json.Unmarshal(raw, &env) != nil {
		t.Fatalf("%s version --json: %v\n%s", bin, err, raw)
	}
	return env.Data
}

// The release pipeline refuses a binary that trusts a test key, has the
// test hooks or pins another Hindsight image, and passes one that trusts
// the two published keys only (decision 0067 and its block review).
func TestVerifyKeysRefusesATestBuild(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	needTools(t)
	version := "-X " + pkg + "/internal/buildinfo.Version=9.9.9"
	release := t.TempDir()
	buildInto(t, release, version)
	if out, err := verifyKeys(t, "DIST="+release); err != nil {
		t.Fatalf("a release build is refused: %v\n%s", err, out)
	}

	test := t.TempDir()
	testBin := buildInto(t, test, "-X "+pkg+"/internal/signing.LinkedTestKey="+newKey(t).pub, "-tags", "testkey")
	out, err := verifyKeys(t, "DIST="+test)
	if err == nil || !strings.Contains(out, `"role":"test"`) || !strings.Contains(out, "-tags=testkey") {
		t.Errorf("a test build passes the release check: %v\n%s", err, out)
	}

	// A testhooks build (CI's update proof: a migration that fails on
	// demand, another pin) is never a release either.
	hooks := t.TempDir()
	buildInto(t, hooks, version+" -X "+pkg+"/internal/server.TestImage=example.com/other:9.9.9", "-tags", "testhooks")
	out, err = verifyKeys(t, "DIST="+hooks)
	if err == nil || !strings.Contains(out, `"test_hooks":true`) || !strings.Contains(out, "example.com/other:9.9.9") || !strings.Contains(out, "-tags=testhooks") {
		t.Errorf("a testhooks build passes the release check: %v\n%s", err, out)
	}

	// goreleaser's post-build hook form: the binary and its target.
	host := runtime.GOOS + "_" + runtime.GOARCH
	if out, err := verifyKeys(t, "BIN="+testBin, "TARGET="+host); err == nil {
		t.Errorf("the hook form passes a test build: %s", out)
	}
	if out, err := verifyKeys(t, "DIST="+t.TempDir()); err == nil || !strings.Contains(out, "no "+host+" binary") {
		t.Errorf("an empty dist passes the release check: %v\n%s", err, out)
	}
}

// Another machine's binary cannot run here: its build settings are still
// checked, in the hook form and in dist.
func TestVerifyKeysChecksEveryTarget(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	needTools(t)
	goos, goarch := "windows", "arm64"
	if runtime.GOOS == goos {
		goos = "linux"
	}
	if runtime.GOARCH == goarch {
		goarch = "amd64"
	}
	other := goos + "_" + goarch
	clean := buildFor(t, t.TempDir(), goos, goarch, "-s -w")
	if out, err := verifyKeys(t, "BIN="+clean, "TARGET="+other); err != nil {
		t.Errorf("a release build for %s is refused: %v\n%s", other, err, out)
	}
	for _, tags := range []string{"testkey", "testhooks"} {
		bin := buildFor(t, t.TempDir(), goos, goarch, "-s -w", "-tags", tags)
		if out, err := verifyKeys(t, "BIN="+bin, "TARGET="+other); err == nil || !strings.Contains(out, "-tags="+tags) {
			t.Errorf("a %s build for %s passes: %v\n%s", tags, other, err, out)
		}
	}

	dist := t.TempDir()
	buildInto(t, dist, "-s -w")
	bad := buildFor(t, dist, goos, goarch, "-s -w", "-tags", "testkey")
	if out, err := verifyKeys(t, "DIST="+dist); err == nil || !strings.Contains(out, bad) {
		t.Errorf("a dist with a test build for %s passes: %v\n%s", other, err, out)
	}
}

// Overriding the test key or the pin with -X alone has no effect: only
// the testkey or testhooks tag, which go version -m shows for every
// target, opens them.
func TestLinkerFlagsAloneCannotChangeTrustOrPin(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	needTools(t)
	dist := t.TempDir()
	bin := buildInto(t, dist, "-X "+pkg+"/internal/signing.TestKey="+newKey(t).pub+" -X "+pkg+"/internal/signing.Current="+newKey(t).pub+
		" -X "+pkg+"/internal/signing.Next="+newKey(t).pub+" -X "+pkg+"/internal/server.HindsightImage=example.com/evil:1.0.0")
	v := versionOf(t, bin)
	if len(v.SigningKeys) != 2 || v.TestKey || v.HindsightImage != sourcePin(t) {
		t.Errorf("-X changed the release binary: %+v (want the source pin %s)", v, sourcePin(t))
	}
	if out, err := verifyKeys(t, "DIST="+dist); err != nil { // the published keys, by id
		t.Errorf("%v\n%s", err, out)
	}
	// The tag is what lets a test build trust its key.
	tagged := buildInto(t, t.TempDir(), "-X "+pkg+"/internal/signing.LinkedTestKey="+newKey(t).pub, "-tags", "testkey")
	if v := versionOf(t, tagged); len(v.SigningKeys) != 3 || !v.TestKey {
		t.Errorf("a testkey build does not trust its key: %+v", v)
	}
}
