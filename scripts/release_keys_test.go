package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildInto builds backsight where goreleaser puts this machine's binary
// in dist, with extra ldflags.
func buildInto(t *testing.T, dist, ldflags string) {
	t.Helper()
	out := filepath.Join(dist, "backsight_"+runtime.GOOS+"_"+runtime.GOARCH+"_v1", "backsight")
	cmd := exec.Command("go", "build", "-o", out, "-ldflags", ldflags, "./cmd/backsight")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
}

func verifyKeys(t *testing.T, args ...string) (string, error) {
	t.Helper()
	b, err := exec.Command("make", append([]string{"-s", "-C", "..", "verify-keys"}, args...)...).CombinedOutput()
	return string(b), err
}

// The release pipeline refuses a binary that trusts a test key, and passes
// one that trusts the two published keys only (decision 0067).
func TestVerifyKeysRefusesATestBuild(t *testing.T) {
	for _, tool := range []string{"make", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " missing")
		}
	}
	release := t.TempDir()
	buildInto(t, release, "-X github.com/Barben360/backsight/internal/buildinfo.Version=9.9.9")
	if out, err := verifyKeys(t, "DIST="+release); err != nil {
		t.Fatalf("a release build is refused: %v\n%s", err, out)
	}

	test := t.TempDir()
	buildInto(t, test, "-X github.com/Barben360/backsight/internal/signing.TestKey="+newKey(t).pub)
	out, err := verifyKeys(t, "DIST="+test)
	if err == nil || !strings.Contains(out, `"role":"test"`) {
		t.Errorf("a test build passes the release check: %v\n%s", err, out)
	}

	// goreleaser's post-build hook form: the binary and its target.
	bin := filepath.Join(test, "backsight_"+runtime.GOOS+"_"+runtime.GOARCH+"_v1", "backsight")
	host := runtime.GOOS + "_" + runtime.GOARCH
	if out, err := verifyKeys(t, "BIN="+bin, "TARGET="+host); err == nil {
		t.Errorf("the hook form passes a test build: %s", out)
	}
	if out, err := verifyKeys(t, "BIN="+bin, "TARGET=plan9_mips"); err != nil {
		t.Errorf("another machine's binary is not skipped: %v\n%s", err, out)
	}

	if out, err := verifyKeys(t, "DIST="+t.TempDir()); err == nil || !strings.Contains(out, "no "+host+" binary") {
		t.Errorf("an empty dist passes the release check: %v\n%s", err, out)
	}
}
