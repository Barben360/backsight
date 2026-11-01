//go:build windows

package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/testtool"
)

func TestDoctorNativeHomeOnWindows(t *testing.T) {
	root, err := os.MkdirTemp(`C:\`, "bs-test-home-")
	if err != nil {
		t.Skipf("cannot create a directory under C:\\: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil { // the mode bits mean nothing here
		t.Fatal(err)
	}

	var f NativeFacts
	if err := f.ReadHome(home); err != nil {
		t.Fatal(err)
	}
	c := checkNativeHome(f)
	if c.Status != Error || !strings.Contains(c.Summary, "Users") || !strings.Contains(c.Summary, "other users can read the database") {
		t.Fatalf("a home inheriting C:\\'s ACL: %+v", c)
	}

	if err := fsutil.MakePrivate(home); err != nil {
		t.Fatal(err)
	}
	f = NativeFacts{}
	if err := f.ReadHome(home); err != nil {
		t.Fatal(err)
	}
	if c := checkNativeHome(f); c.Status != OK {
		t.Errorf("a private home: %+v", c)
	}

	if err := (&NativeFacts{}).ReadHome(filepath.Join(root, "absent")); err == nil {
		t.Error("ReadHome of an absent directory succeeded")
	}
}

func TestFreeBytesOnWindows(t *testing.T) {
	n, err := FreeBytes(t.TempDir())
	if err != nil || n == 0 {
		t.Fatalf("FreeBytes = %d, %v, want > 0", n, err)
	}
	if _, err := FreeBytes(filepath.Join(t.TempDir(), "absent", "x")); err == nil {
		t.Error("FreeBytes of a path on no volume succeeded")
	}
}

// A Windows venv keeps its packages in Lib\site-packages: native-venv
// reads them there (a Linux path would read nothing and report every
// package missing). A venv of the lock's versions differs in nothing; one
// package at another version is the difference.
func TestNativeVenvDiffReadsTheWindowsLayout(t *testing.T) {
	venv := t.TempDir()
	sp := filepath.Join(venv, "Lib", "site-packages")
	var one string
	for name, v := range nativelock.Versions() {
		if one == "" {
			one = name
			v = "0.0.0"
		}
		if err := os.MkdirAll(filepath.Join(sp, name+"-"+v+".dist-info"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	diff, err := NativeVenvDiff(venv)
	if err != nil || len(diff) != 1 || diff[0] != one+"=="+nativelock.Versions()[one] {
		t.Fatalf("diff %q, %v, want only %s at its locked version", diff, err, one)
	}
	if err := os.Rename(filepath.Join(sp, one+"-0.0.0.dist-info"), filepath.Join(sp, one+"-"+nativelock.Versions()[one]+".dist-info")); err != nil {
		t.Fatal(err)
	}
	if diff, err := NativeVenvDiff(venv); err != nil || len(diff) != 0 {
		t.Errorf("the lock's venv: diff %q, %v", diff, err)
	}
}

// The config check's fix for a secrets.toml other users can read is, on
// Windows, the PowerShell icacls command native-private gives (never
// chmod), and running it clears the check.
func TestDoctorSecretsFixOnWindows(t *testing.T) {
	f := newFixture(t)
	secrets := f.env.Dirs.SecretsFile()
	put(t, f.env.Dirs.ConfigFile(), "", 0o600)
	put(t, secrets, "api_key = \"fakevalue\"\n", 0o600)
	testtool.ShareWithOthers(t, secrets)
	c := check(t, f.run(), "config")
	expect(t, c, Error, "secrets")
	fix, ok := strings.CutPrefix(c.Fix, "in PowerShell: ")
	if !ok || !strings.HasPrefix(fix, "icacls '"+secrets+"' /inheritance:r /grant:r '*S-1-5-") || !strings.Contains(fix, " /remove:g ") ||
		strings.Contains(c.Fix, "chmod") || strings.Contains(c.Fix, "%") || strings.Contains(c.Summary+c.Detail+c.Fix, "fakevalue") {
		t.Fatalf("config fix %q: want the PowerShell icacls command for %s", c.Fix, secrets)
	}
	if out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", fix).CombinedOutput(); err != nil {
		t.Fatalf("running the fix: %v\n%s", err, out)
	}
	if c := check(t, f.run(), "config"); c.Status != OK {
		t.Errorf("after its fix: %+v", c)
	}
}
