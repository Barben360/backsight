//go:build windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
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

// An entry written with a variable that expands to dir is dir: it goes
// too (pathHas finds it), and one that expands elsewhere stays.
func TestRemoveUserPathRemovesAnEntryWrittenWithAVariable(t *testing.T) {
	t.Setenv("BSTEST_PROGRAMS", `C:\Users\x\Programs`)
	t.Setenv("BSTEST_OTHER", `C:\Users\y`)
	dir := `C:\Users\x\Programs\backsight`
	userPathIn(t, `C:\Tools;%BSTEST_PROGRAMS%\backsight\;%BSTEST_OTHER%\backsight`, registry.EXPAND_SZ)
	removed, err := removeUserPath(dir)
	if err != nil || !removed {
		t.Fatal(removed, err)
	}
	if v, _ := readUserPathValue(t); v != `C:\Tools;%BSTEST_OTHER%\backsight` {
		t.Errorf("user PATH %q, want the variable entry of dir gone, the other kept", v)
	}
}

// A file reached through its directory's 8.3 short name is the file
// itself (GitHub's runner has TEMP at C:\Users\RUNNER~1\...): copying it
// onto its long name changes nothing and does not fail, where replacing a
// file open for reading is refused.
func TestCopyFileOntoItselfThroughAShortName(t *testing.T) {
	long := filepath.Join(t.TempDir(), "a directory with a long name")
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	short := shortPath(t, long)
	if strings.EqualFold(short, long) {
		t.Skip("no 8.3 short names on this volume")
	}
	p := filepath.Join(long, "checksums.txt")
	if err := os.WriteFile(p, []byte("sums"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(filepath.Join(short, "checksums.txt"), p, 0o644); err != nil {
		t.Fatalf("copying a file onto itself: %v", err)
	}
	if raw, _ := os.ReadFile(p); string(raw) != "sums" {
		t.Errorf("content %q", raw)
	}
	if left, _ := filepath.Glob(filepath.Join(long, ".checksums.txt.tmp-*")); len(left) > 0 {
		t.Errorf("temp files left: %q", left)
	}
}

func shortPath(t *testing.T, p string) string {
	t.Helper()
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(in, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		t.Fatalf("GetShortPathName(%s): %v", p, err)
	}
	return windows.UTF16ToString(buf[:n])
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

func TestAFailedSwapKeepsTheOldShimsChecksums(t *testing.T) {
	dir := t.TempDir()
	v1, v2 := sleeper(t, filepath.Join(dir, "v1"), "1"), sleeper(t, filepath.Join(dir, "v2"), "2")
	bin := filepath.Join(dir, "bin")
	want := map[string]string{selfcheck.ChecksumsFile: "v1 sums", selfcheck.SignatureFile: "v1 sig"}
	for n, c := range want {
		_ = os.WriteFile(filepath.Join(dir, "v1", n), []byte(c), 0o644)
	}
	shim := filepath.Join(bin, "current.exe")
	if err := pointShim(v1, shim); err != nil {
		t.Fatal(err)
	}
	// v2 has different checksums; its exe is missing, then its dir
	for n := range want {
		_ = os.WriteFile(filepath.Join(dir, "v2", n), []byte("v2 "+n), 0o644)
	}
	_ = os.Remove(v2)
	for _, target := range []string{v2, filepath.Join(dir, "nowhere", "backsight.exe")} {
		if err := pointShim(target, shim); err == nil {
			t.Fatalf("%s: a missing target must fail", target)
		}
		for n, c := range want {
			if b, _ := os.ReadFile(filepath.Join(bin, n)); string(b) != c {
				t.Errorf("%s after a failed swap to %s: %q, want %q", n, target, b, c)
			}
		}
		if !sameBytes(t, shim, v1) {
			t.Error("the shim changed")
		}
	}
}

// A test binary re-executed as backsight (update apply's runChild, the
// hooks) must write the throwaway PATH key of its parent, never the real
// Environment.
func TestAReexecutedChildWritesTheThrowawayPathKey(t *testing.T) {
	if os.Getenv("BACKSIGHT_TEST_PATH_CHILD") == "1" {
		return // the child: TestMain already isolated it
	}
	if userPathKey == "Environment" {
		t.Fatal("the parent is not isolated")
	}
	dir := `C:\bs-reexec-` + strings.ReplaceAll(t.Name(), "/", "_")
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestReexecChildHelper$")
	cmd.Env = append(os.Environ(), "BACKSIGHT_TEST_PATH_CHILD=1", "BACKSIGHT_TEST_PATH_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, userPathKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	v, _, _ := k.GetStringValue("Path")
	_ = k.Close()
	if !strings.Contains(v, dir) {
		t.Errorf("the child did not write the parent's throwaway key: %q", v)
	}
	real, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = real.Close() }()
	if rv, _, _ := real.GetStringValue("Path"); strings.Contains(rv, dir) {
		t.Fatalf("the child wrote the REAL user PATH: %q", rv)
	}
}

func TestReexecChildHelper(t *testing.T) {
	if os.Getenv("BACKSIGHT_TEST_PATH_CHILD") != "1" {
		t.Skip("only as the child of TestAReexecutedChildWritesTheThrowawayPathKey")
	}
	if _, err := ensureUserPath(os.Getenv("BACKSIGHT_TEST_PATH_DIR")); err != nil {
		t.Fatal(err)
	}
}

// A repeat client install with nothing new leaves the shim and the PATH
// copy in place: the supervisor (or a session) running current.exe keeps
// running it rather than a current.exe.old-<n> that doctor's shim-copies
// would count, and both steps say unchanged.
func TestClientInstallAgainLeavesTheShimInPlace(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	d := pathsOf(home)
	exe := sleeper(t, filepath.Join(home, "release"), "r")
	old := selfExe
	selfExe = func() (string, error) { return exe, nil }
	t.Cleanup(func() { selfExe = old })
	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatalf("first install: exit %d\n%s", code, out)
	}
	shim := filepath.Join(d.Data, "bin", "current.exe")
	startShim(t, shim) // the supervisor setup's server step started
	code, out, _ := run(t, "client", "install", "--json")
	if code != output.OK {
		t.Fatalf("second install: exit %d\n%s", code, out)
	}
	var env struct {
		Data clientInstallResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shim", "path"} {
		found := false
		for _, s := range env.Data.Steps {
			if s.Name == name {
				found = true
				if s.Status != "unchanged" {
					t.Errorf("%s step %q, want unchanged: %s", name, s.Status, s.Detail)
				}
			}
		}
		if !found {
			t.Errorf("no %s step: %+v", name, env.Data.Steps)
		}
	}
	if got := olds(t, shim); len(got) != 0 {
		t.Errorf("the running shim was renamed aside: %v", got)
	}
	if got := olds(t, filepath.Join(d.Bin, "backsight.exe")); len(got) != 0 {
		t.Errorf("the PATH copy was swapped: %v", got)
	}
	if !sameBytes(t, shim, exe) || !sameBytes(t, filepath.Join(d.Bin, "backsight.exe"), exe) {
		t.Error("the shim or the PATH copy is not this binary")
	}
}
