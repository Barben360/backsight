//go:build windows

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/selfcheck"
	"github.com/Barben360/backsight/internal/testtool"
)

// sleeper makes a long-running program (a fake that sleeps) at dir\backsight.exe
// whose bytes are unique to tag, and returns its path.
func sleeper(t *testing.T, dir, tag string) string {
	t.Helper()
	p := testtool.Write(t, dir, "backsight", testtool.Fake{Script: "sleep 60000", NoLog: true})
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(tag) // an overlay: the image still runs, the bytes differ
	_ = f.Close()
	return p
}

// startShim runs the shim as a session does and returns once its image is
// mapped (a running image cannot be deleted or replaced).
func startShim(t *testing.T, shim string) *exec.Cmd {
	t.Helper()
	// the fake finds its sidecar next to the executable's own name
	side := strings.TrimSuffix(shim, ".exe") + ".fake.json"
	raw := []byte(`{"Script":"sleep 60000","NoLog":true}`)
	if err := os.WriteFile(side, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shim)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func olds(t *testing.T, shim string) []string {
	t.Helper()
	m, _ := filepath.Glob(shim + ".old-*")
	for i := range m {
		m[i] = filepath.Base(m[i])
	}
	return m
}

func sameBytes(t *testing.T, a, b string) bool {
	t.Helper()
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	if err1 != nil || err2 != nil {
		t.Fatalf("%v %v", err1, err2)
	}
	return bytes.Equal(x, y)
}

func TestPointShimTwiceWhileTheOldCopiesRun(t *testing.T) {
	dir := t.TempDir()
	v1, v2, v3 := sleeper(t, filepath.Join(dir, "v1"), "1"), sleeper(t, filepath.Join(dir, "v2"), "2"), sleeper(t, filepath.Join(dir, "v3"), "3")
	shim := filepath.Join(dir, "bin", "current.exe")

	if err := pointShim(v1, shim); err != nil {
		t.Fatal(err)
	}
	first := startShim(t, shim)
	if err := pointShim(v2, shim); err != nil {
		t.Fatalf("first swap while the shim runs: %v", err)
	}
	if !sameBytes(t, shim, v2) || len(olds(t, shim)) != 1 {
		t.Errorf("after one swap: shim is v2 %v, old copies %v", sameBytes(t, shim, v2), olds(t, shim))
	}
	second := startShim(t, shim)
	if err := pointShim(v3, shim); err != nil {
		t.Fatalf("second swap while both older copies run: %v", err)
	}
	got := olds(t, shim)
	if len(got) != 2 || got[0] != "current.exe.old-1" || got[1] != "current.exe.old-2" || !sameBytes(t, shim, v3) {
		t.Errorf("after two swaps, old copies %v, shim is v3 %v", got, sameBytes(t, shim, v3))
	}
	if !sameBytes(t, shim+".old-1", v1) || !sameBytes(t, shim+".old-2", v2) {
		t.Error("an old copy is not the version it ran")
	}

	for _, c := range []*exec.Cmd{first, second} {
		_ = c.Process.Kill()
		_ = c.Wait()
	}
	if err := pointShim(v1, shim); err != nil {
		t.Fatalf("swap after both exited: %v", err)
	}
	if got := olds(t, shim); len(got) != 0 {
		t.Errorf("old copies nobody holds must be removed, left %v", got)
	}
}

func TestPointShimLeavesTheShimWhenTheCopyFails(t *testing.T) {
	dir := t.TempDir()
	v1 := sleeper(t, filepath.Join(dir, "v1"), "1")
	shim := filepath.Join(dir, "bin", "current.exe")
	if err := pointShim(v1, shim); err != nil {
		t.Fatal(err)
	}
	if err := pointShim(filepath.Join(dir, "missing.exe"), shim); err == nil {
		t.Fatal("a missing target must fail")
	}
	if !sameBytes(t, shim, v1) {
		t.Error("the shim must still be the previous version")
	}
	if got := olds(t, shim); len(got) != 0 {
		t.Errorf("old copies %v", got)
	}
}

func TestPointShimCarriesTheChecksumsAndDropsStaleOnes(t *testing.T) {
	dir := t.TempDir()
	v1, v2 := sleeper(t, filepath.Join(dir, "v1"), "1"), sleeper(t, filepath.Join(dir, "v2"), "2")
	for _, n := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		_ = os.WriteFile(filepath.Join(dir, "v1", n), []byte("v1 "+n), 0o644)
	}
	shim := filepath.Join(dir, "bin", "current.exe")
	if err := pointShim(v1, shim); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		if b, _ := os.ReadFile(filepath.Join(dir, "bin", n)); string(b) != "v1 "+n {
			t.Errorf("%s beside the shim: %q", n, b)
		}
	}
	if err := pointShim(v2, shim); err != nil { // v2 has none: a dev build
		t.Fatal(err)
	}
	for _, n := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		if _, err := os.Stat(filepath.Join(dir, "bin", n)); err == nil {
			t.Errorf("%s of the previous version stays beside the new shim", n)
		}
	}
}

func TestSelfcheckFindsChecksumsBesideTheShim(t *testing.T) {
	home := isolate(t)
	d := pathsOf(home)
	rel := fakeRelease(t, filepath.Join(home, "release"), "9.9.9", true)
	target := filepath.Join(d.Data, "bin", "9.9.9-abc", "backsight.exe")
	shim := filepath.Join(d.Data, "bin", "current.exe")
	if err := installShim(target, shim); err != nil {
		t.Fatal(err)
	}
	if rel == shim || !sameBytes(t, rel, shim) {
		t.Fatal("the shim is not a copy of the release")
	}
	got := selfcheck.Check(shim, "9.9.9", "windows", "amd64", time.Now())
	if got.Status != selfcheck.Verified {
		t.Errorf("the shim must verify itself against the checksums beside it: %s %s", got.Status, got.Detail)
	}
}

func readUserPathValue(t *testing.T) (string, uint32) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, userPathKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	v, typ, err := k.GetStringValue("Path")
	if err != nil {
		t.Fatal(err)
	}
	return v, typ
}

// userPathIn points the user PATH at a key of this test alone.
func userPathIn(t *testing.T, initial string, typ uint32) {
	t.Helper()
	old := userPathKey
	userPathKey = old + `\` + strings.NewReplacer("/", "_").Replace(t.Name())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, userPathKey, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	if initial != "" {
		if typ == registry.SZ {
			err = k.SetStringValue("Path", initial)
		} else {
			err = k.SetExpandStringValue("Path", initial)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = k.Close()
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, userPathKey); userPathKey = old })
}

func TestEnsureUserPathAddsOnce(t *testing.T) {
	userPathIn(t, `C:\Tools;%USERPROFILE%\bin`, registry.EXPAND_SZ)
	dir := `C:\Users\x\AppData\Local\Programs\backsight`
	added, err := ensureUserPath(dir)
	if err != nil || !added {
		t.Fatalf("first call: %v %v", added, err)
	}
	v, typ := readUserPathValue(t)
	if v != `C:\Tools;%USERPROFILE%\bin;`+dir || typ != registry.EXPAND_SZ {
		t.Errorf("path %q type %d: the entry is appended, the existing ones and REG_EXPAND_SZ kept", v, typ)
	}
	for _, again := range []string{dir, strings.ToUpper(dir), dir + `\`, strings.ToLower(dir) + `\`} {
		if added, err := ensureUserPath(again); err != nil || added {
			t.Errorf("%q counts as present: %v %v", again, added, err)
		}
	}
	if v2, _ := readUserPathValue(t); v2 != v {
		t.Errorf("a repeated call rewrote the path: %q", v2)
	}
}

func TestEnsureUserPathSeesAVariableEntryAndKeepsPlainStrings(t *testing.T) {
	t.Setenv("BSTEST_BIN", `D:\Apps`)
	userPathIn(t, `%BSTEST_BIN%\backsight;`, registry.SZ)
	if added, err := ensureUserPath(`D:\Apps\backsight`); err != nil || added {
		t.Errorf("an entry written with a variable is the directory: %v %v", added, err)
	}
	if added, err := ensureUserPath(`E:\tools`); err != nil || !added {
		t.Fatal(added, err)
	}
	if v, typ := readUserPathValue(t); v != `%BSTEST_BIN%\backsight;E:\tools` || typ != registry.SZ {
		t.Errorf("%q type %d", v, typ)
	}
}

func TestEnsureUserPathCreatesAMissingPath(t *testing.T) {
	userPathIn(t, "", 0)
	if added, err := ensureUserPath(`C:\a`); err != nil || !added {
		t.Fatal(added, err)
	}
	if v, typ := readUserPathValue(t); v != `C:\a` || typ != registry.EXPAND_SZ {
		t.Errorf("%q type %d", v, typ)
	}
}

func TestRemoveUserPathRemovesOnlyThatEntry(t *testing.T) {
	dir := `C:\Users\x\Programs\backsight`
	userPathIn(t, `C:\Tools;`+strings.ToUpper(dir)+`\;C:\Users\x\Programs\backsight2;%X%\y`, registry.EXPAND_SZ)
	removed, err := removeUserPath(dir)
	if err != nil || !removed {
		t.Fatal(removed, err)
	}
	if v, typ := readUserPathValue(t); v != `C:\Tools;C:\Users\x\Programs\backsight2;%X%\y` || typ != registry.EXPAND_SZ {
		t.Errorf("only the entry goes, a longer name and a variable stay: %q type %d", v, typ)
	}
	if removed, err := removeUserPath(dir); err != nil || removed {
		t.Errorf("second call: %v %v", removed, err)
	}
}

func TestPathCopyIsAnExe(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	d := pathsOf(home)
	rel := fakeRelease(t, filepath.Join(home, "release"), "9.9.9", true)
	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	cmd := filepath.Join(d.Bin, "backsight.exe")
	fi, err := os.Lstat(cmd)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("%s must be a regular file, a copy: %v", cmd, err)
	}
	shim := filepath.Join(d.Data, "bin", "current.exe")
	if !sameBytes(t, cmd, shim) || !sameBytes(t, cmd, rel) {
		t.Error("the PATH copy is not a copy of the shim")
	}
	if v, _ := readUserPathValue(t); !strings.Contains(strings.ToLower(v), strings.ToLower(d.Bin)) {
		t.Errorf("%s is not on the user PATH: %q", d.Bin, v)
	}
	// an update swaps it: the PATH copy follows the shim, the old one is aside
	next := sleeper(t, filepath.Join(home, "next"), "n")
	if err := pointShim(next, shim); err != nil {
		t.Fatal(err)
	}
	link, ours := olderPathLink(d, shim)
	if !ours || link != cmd {
		t.Fatalf("an existing PATH copy is the install's to refresh: %q %v", link, ours)
	}
	if err := pointShim(shim, link); err != nil || !sameBytes(t, cmd, next) {
		t.Errorf("the PATH copy must follow the shim: %v", err)
	}
}
