package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/selfcheck"
)

// sameTarget: on Windows link is a copy of target (a symlink needs a
// privilege a standard user lacks), elsewhere a symlink to it.
func sameTarget(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		if got, err := os.Readlink(link); err != nil || got != target {
			t.Errorf("%s → %q (%v), want %s", link, got, err, target)
		}
		return
	}
	got, err1 := os.ReadFile(link)
	want, err2 := os.ReadFile(target)
	if err1 != nil || err2 != nil || string(got) != string(want) {
		t.Errorf("%s is not a copy of %s (%v, %v)", link, target, err1, err2)
	}
}

func installShimRun(t *testing.T, version string) installedShim {
	t.Helper()
	code, out, errOut := run(t, "__install-shim", version, "--json")
	var env struct {
		Data installedShim `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d %v\n%s\n%s", code, err, out, errOut)
	}
	return env.Data
}

// install.ps1's last step: the binary it verified installs itself under
// <data>/bin/<version> with the signed checksums beside it, the shim runs
// it, the command on PATH runs the shim, and on Windows <bin> joins the
// user PATH (the test's throwaway key, TestMain) once.
func TestInstallShimInstallsThisBinaryUnderItsVersion(t *testing.T) {
	home := isolate(t)
	src := t.TempDir()
	exe := fakeRelease(t, src, "9.9.9", true)
	d := pathsOf(home)
	r := installShimRun(t, "9.9.9")

	name := "backsight"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(d.Data, "bin", "9.9.9", name)
	shim := filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS))
	if r.Version != "9.9.9" || r.Target != target || r.Shim != shim || r.Command != pathCommand(d.Bin) || r.Bin != d.Bin {
		t.Errorf("reported %+v", r)
	}
	for _, f := range [][2]string{{exe, target},
		{filepath.Join(src, selfcheck.ChecksumsFile), filepath.Join(d.Data, "bin", "9.9.9", selfcheck.ChecksumsFile)},
		{filepath.Join(src, selfcheck.SignatureFile), filepath.Join(d.Data, "bin", "9.9.9", selfcheck.SignatureFile)}} {
		want, _ := os.ReadFile(f[0])
		got, err := os.ReadFile(f[1])
		if err != nil || len(want) == 0 || string(got) != string(want) {
			t.Errorf("%s is not %s (%v)", f[1], f[0], err)
		}
	}
	sameTarget(t, shim, target)
	sameTarget(t, pathCommand(d.Bin), shim)
	if r.UserPathAdded != (runtime.GOOS == "windows") {
		t.Errorf("user PATH added: %v on %s", r.UserPathAdded, runtime.GOOS)
	}
	// Again (the same install run twice): nothing added to the user PATH.
	if again := installShimRun(t, "9.9.9"); again.UserPathAdded {
		t.Error("the user PATH got <bin> a second time")
	}
	sameTarget(t, pathCommand(d.Bin), shim)
}

// The version names a directory: one that could leave <data>/bin, or that
// install.ps1 itself refuses, is refused before anything is written.
func TestInstallShimRefusesAnUnexpectedVersion(t *testing.T) {
	home := isolate(t)
	fakeRelease(t, t.TempDir(), "9.9.9", true)
	for _, v := range []string{"..", ".9", "", "9/../../x", `9\..\x`, "9 9", "9:x"} {
		code, _, errOut := run(t, "__install-shim", v)
		if code != output.Usage || !strings.Contains(errOut, "unexpected release version") {
			t.Errorf("version %q: exit %d, want %d and the reason\n%s", v, code, output.Usage, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(pathsOf(home).Data, "bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("data/bin written for a refused version (%v)", err)
	}
}
