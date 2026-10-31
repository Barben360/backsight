//go:build windows

package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/server/nativelock"
)

// winNative is newNative laid out for Windows: the real Windows layout
// on this Windows host.
func winNative(t *testing.T, tools ToolRunner) Native {
	t.Helper()
	n := newNative(t, tools)
	n.GOOS = "windows"
	return n
}

// uvZip is uv's Windows release archive as astral ships it: uv.exe, uvx.exe
// and uvw.exe at its root.
func uvZip(t *testing.T, uv []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range []struct {
		name string
		body []byte
	}{{"uvx.exe", []byte("uvx")}, {"uv.exe", uv}, {"uvw.exe", []byte("uvw")}} {
		if f.body == nil {
			continue
		}
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(f.body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Requirement 4 on Windows: uv comes from the pinned zip, hash-checked
// before anything is extracted; uv.exe alone is extracted.
func TestFetchUVFromTheZipChecksTheHash(t *testing.T) {
	uvExe := []byte("MZ uv 0.13.1")
	archive := uvZip(t, uvExe)

	t.Run("a sha256 other than the pin installs nothing", func(t *testing.T) {
		got := serveUV(t, archive) // uvSHA256 stays the real uv's
		tools := &fakeTools{}
		n := winNative(t, tools)
		err := n.fetchUV(context.Background(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "the uv download does not match the sha256 this backsight carries: nothing was installed") {
			t.Fatalf("fetchUV = %v, want the sha256 refusal", err)
		}
		if want := "/astral-sh/uv/releases/download/0.13.1/uv-x86_64-pc-windows-msvc.zip"; len(*got) != 1 || (*got)[0] != want {
			t.Errorf("requests %v, want one GET of %s", *got, want)
		}
		if exists(filepath.Join(n.Root(), "uv")) {
			t.Error("a uv\\ directory exists after a refused download")
		}
		if left, _ := os.ReadDir(n.Root()); len(left) != 0 {
			t.Errorf("the download left %v in %s", left, n.Root())
		}
	})

	t.Run("the pinned bytes give uv.exe alone", func(t *testing.T) {
		serveUV(t, archive)
		sum := sha256.Sum256(archive)
		uvSHA256 = func(p nativelock.Platform) string {
			if p.GOOS != "windows" {
				t.Errorf("checked against %s's pin", p.GOOS)
			}
			return hex.EncodeToString(sum[:])
		}
		n := winNative(t, &fakeTools{})
		if err := n.fetchUV(context.Background(), io.Discard); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(n.UV()); err != nil || !bytes.Equal(b, uvExe) {
			t.Errorf("%s holds %q (%v), want the archive's uv.exe", n.UV(), b, err)
		}
		if entries, _ := os.ReadDir(filepath.Dir(n.UV())); len(entries) != 1 {
			t.Errorf("uv\\0.13.1 holds %v, want uv.exe alone", entries)
		}
		if !n.uvInstalled() {
			t.Error("uv.exe extracted but not installed")
		}
	})

	t.Run("a pinned archive without uv.exe installs nothing", func(t *testing.T) {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		w, _ := zw.Create("uvx.exe")
		_, _ = w.Write([]byte("uvx"))
		_ = zw.Close()
		serveUV(t, b.Bytes())
		sum := sha256.Sum256(b.Bytes())
		uvSHA256 = func(nativelock.Platform) string { return hex.EncodeToString(sum[:]) }
		n := winNative(t, &fakeTools{})
		if err := n.fetchUV(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "holds no uv.exe") {
			t.Errorf("fetchUV = %v, want the missing uv.exe", err)
		}
		if exists(n.UV()) {
			t.Error("uv.exe exists")
		}
	})
}

// winEffects does to the disk what uv, pip and hf do on Windows: Python's
// python.exe, the venv's Scripts\ with the msvc-runtime DLLs, pg0.exe and
// onnxruntime's capi\ in site-packages.
func winEffects(t *testing.T, n Native) func(toolCall) {
	return func(c toolCall) {
		switch {
		case len(c.Args) > 1 && c.Args[0] == "python" && c.Args[1] == "install":
			touch(t, n.PythonBin(), 0o755)
		case len(c.Args) > 0 && c.Args[0] == "venv":
			v := c.Args[len(c.Args)-1]
			touch(t, filepath.Join(v, "Scripts", "python.exe"), 0o755)
			touch(t, filepath.Join(v, "Scripts", "hf.exe"), 0o755)
		case len(c.Args) > 1 && c.Args[0] == "pip":
			venv := filepath.Dir(filepath.Dir(c.Args[slices.Index(c.Args, "--python")+1]))
			sp := filepath.Join(venv, "Lib", "site-packages")
			touch(t, filepath.Join(sp, "pg0", "bin", "pg0.exe"), 0o755)
			touch(t, filepath.Join(sp, "onnxruntime", "capi", "onnxruntime.dll"), 0o644)
			for _, d := range runtimeDLLs {
				if err := os.WriteFile(filepath.Join(venv, "Scripts", d), []byte("dll "+d), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// The venv on Windows: uv venv with the pinned patch version, pip install of
// the Windows lock with <venv>\Scripts\python.exe, every uv call in
// Windows' tool environment (nothing of the user's Python settings, PATH
// uv's directory then System32), and the runtime DLLs in pg0's bin\ and
// onnxruntime's capi\ before the venv is marked complete.
func TestWindowsVenvLayout(t *testing.T) {
	t.Setenv("PYTHONPATH", `C:\evil\pythonpath`)
	t.Setenv("VIRTUAL_ENV", `C:\evil\venv`)
	t.Setenv("UV_INDEX_URL", "https://evil.invalid/simple")
	tools := &fakeTools{}
	n := winNative(t, tools)
	v := n.Version()
	tools.Effect = winEffects(t, n)
	var completeBeforeDLLs bool
	tools.Fail = func(c toolCall) error {
		completeBeforeDLLs = completeBeforeDLLs || exists(filepath.Join(n.VenvDir(v), venvComplete))
		return nil
	}
	touch(t, n.UV(), 0o644)
	touch(t, n.PythonBin(), 0o755)
	if err := n.fetchVenv(context.Background(), io.Discard, v); err != nil {
		t.Fatal(err)
	}
	venv := n.VenvDir(v)
	if len(tools.Calls) != 2 {
		t.Fatalf("%d calls, want uv venv and uv pip install: %+v", len(tools.Calls), tools.Calls)
	}
	c := tools.Calls
	// The patch version, not python.exe's path: given a path uv bases the
	// venv on the minor-version junction, which an SSH session cannot
	// traverse (os error 448).
	if want := []string{"venv", "--no-project", "--python", "3.11.17", venv}; c[0].Path != n.UV() || !slices.Equal(c[0].Args, want) {
		t.Errorf("call 1: %s %q, want %s %q", c[0].Path, c[0].Args, n.UV(), want)
	}
	if !strings.HasSuffix(n.PythonBin(), `\python\cpython-3.11.17-windows-x86_64-none\python.exe`) {
		t.Errorf("PythonBin() = %s, want uv's Windows layout", n.PythonBin())
	}
	py := filepath.Join(venv, "Scripts", "python.exe")
	if i := slices.Index(c[1].Args, "--python"); c[1].Path != n.UV() || i < 0 || c[1].Args[i+1] != py {
		t.Errorf("call 2: %s %q, want uv pip install --python %s", c[1].Path, c[1].Args, py)
	}
	if !bytes.Equal(c[1].Req, nativelock.WindowsRequirements) {
		t.Errorf("pip install read %d bytes, not the Windows lock (%d bytes)", len(c[1].Req), len(nativelock.WindowsRequirements))
	}
	for i, call := range c {
		for _, e := range call.Env {
			k, _, _ := strings.Cut(e, "=")
			if slices.Contains([]string{"PYTHONPATH", "VIRTUAL_ENV", "UV_INDEX_URL"}, strings.ToUpper(k)) {
				t.Errorf("call %d inherits %s", i+1, e)
			}
		}
		for _, want := range []string{"PATH=" + filepath.Dir(n.UV()) + `;` + filepath.Join(os.Getenv("SYSTEMROOT"), "System32"),
			"UV_NO_CONFIG=1", "UV_PYTHON_PREFERENCE=only-managed", "UV_PYTHON_INSTALL_DIR=" + n.Python(), "TEMP=" + filepath.Join(n.Root(), "tmp")} {
			if !slices.Contains(call.Env, want) {
				t.Errorf("call %d env %q lacks %s", i+1, call.Env, want)
			}
		}
	}
	for _, dir := range []string{n.pg0BinDir(v), n.onnxCapi(v)} {
		for _, d := range runtimeDLLs {
			if b, err := os.ReadFile(filepath.Join(dir, d)); err != nil || string(b) != "dll "+d {
				t.Errorf("%s: %q %v, want the venv's %s", filepath.Join(dir, d), b, err, d)
			}
		}
	}
	if completeBeforeDLLs || !exists(filepath.Join(venv, venvComplete)) {
		t.Errorf("the venv marked complete before its DLLs (%v), or never", completeBeforeDLLs)
	}

	t.Run("a venv without the runtime DLLs is not complete", func(t *testing.T) {
		tools := &fakeTools{}
		n := winNative(t, tools)
		effects := winEffects(t, n)
		tools.Effect = func(c toolCall) {
			effects(c)
			if c.Args[0] == "pip" {
				_ = os.Remove(filepath.Join(n.VenvDir(v), "Scripts", "msvcp140.dll"))
			}
		}
		touch(t, n.UV(), 0o644)
		touch(t, n.PythonBin(), 0o755)
		err := n.fetchVenv(context.Background(), io.Discard, v)
		if err == nil || !strings.Contains(err.Error(), "msvcp140.dll") {
			t.Fatalf("fetchVenv = %v, want the missing msvcp140.dll", err)
		}
		if exists(n.VenvDir(v)) {
			t.Error("the venv without its DLLs is left")
		}
	})

	t.Run("no installation yet: none created", func(t *testing.T) {
		if exists(filepath.Join(n.installation(), "bin")) {
			t.Errorf("fetchVenv created %s before the unpack", filepath.Join(n.installation(), "bin"))
		}
	})
}

// Fix round 1 (Task 10): the copied installation's bin\ is shared by every
// version, so a venv whose msvc-runtime changed replaces its DLLs there
// too, and dlls.json holds for all three places; an unchanged DLL is left
// alone (a running postgres has it loaded).
func TestVenvRefreshesTheInstallationsDLLs(t *testing.T) {
	tools := &fakeTools{}
	n := winNative(t, tools)
	v := n.Version()
	tools.Effect = winEffects(t, n)
	touch(t, n.UV(), 0o644)
	touch(t, n.PythonBin(), 0o755)
	bin := filepath.Join(n.installation(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range runtimeDLLs { // an older msvc-runtime's
		if err := os.WriteFile(filepath.Join(bin, d), []byte("old "+d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	same := filepath.Join(bin, "concrt140.dll")
	if err := os.WriteFile(same, []byte("dll concrt140.dll"), 0o755); err != nil { // unchanged in the new one
		t.Fatal(err)
	}
	before, err := os.Stat(same)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := n.fetchVenv(context.Background(), io.Discard, v); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(n.Root(), dllManifest))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{bin, n.pg0BinDir(v), n.onnxCapi(v)} {
		for _, d := range runtimeDLLs {
			b, err := os.ReadFile(filepath.Join(dir, d))
			sum := sha256.Sum256(b)
			if err != nil || string(b) != "dll "+d || manifest[d] != hex.EncodeToString(sum[:]) {
				t.Errorf("%s: %q %v, manifest %q: want the new venv's, as the manifest says", filepath.Join(dir, d), b, err, manifest[d])
			}
		}
	}
	if after, err := os.Stat(same); err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("an unchanged DLL was rewritten (%v → %v, %v)", before.ModTime(), after.ModTime(), err)
	}
}

// A DLL a running program has loaded cannot be overwritten, only renamed:
// placeDLL moves it aside, writes the new one, and removes the old one
// once nothing holds it.
func TestPlaceDLLReplacesALoadedDLL(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "concrt140.dll")
	sys, err := os.ReadFile(filepath.Join(os.Getenv("SYSTEMROOT"), "System32", "version.dll"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, sys, 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := windows.LoadLibraryEx(dst, 0, windows.LOAD_LIBRARY_AS_IMAGE_RESOURCE)
	if err != nil {
		t.Fatal(err)
	}
	freed := false
	free := func() {
		if !freed {
			freed = true
			_ = windows.FreeLibrary(h)
		}
	}
	t.Cleanup(free)
	if err := fsutil.WriteAtomic(dst, []byte("new"), 0o755); err == nil {
		t.Fatal("a loaded DLL was overwritten in place: this test proves nothing here")
	}
	if err := placeDLL(dst, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("%s = %d bytes, want the new DLL", dst, len(b))
	}
	aside, _ := filepath.Glob(dst + dllAside + "*")
	if len(aside) != 1 {
		t.Fatalf("aside %v, want the loaded one kept aside", aside)
	}
	free()
	if err := placeDLL(dst, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if exists(aside[0]) {
		t.Error("the old DLL stays aside once nothing holds it")
	}
}

// exitCodeErr is a tool's failure with its exit code, as exec's ExitError
// gives it.
type exitCodeErr struct {
	code int
	msg  string
}

func (e exitCodeErr) Error() string { return e.msg }
func (e exitCodeErr) ExitCode() int { return e.code }

// E34 ruling 6: uv python install gets --no-bin --no-registry; its exit 2
// on the minor-version link (over SSH) is accepted once the pinned
// python.exe runs; any other failure is not.
func TestUVMinorLinkErrorAccepted(t *testing.T) {
	minorLink := "exit status 2: error: Failed to create Python minor version link directory: untrusted mount point (os error 448)"
	for _, tc := range []struct {
		name      string
		uvErr     error
		pythonErr error
		ok        bool
	}{
		{"the minor link alone", exitCodeErr{2, minorLink}, nil, true},
		{"the minor link, Python broken", exitCodeErr{2, minorLink}, errors.New("exit status 3221225781"), false},
		{"another exit code", exitCodeErr{1, minorLink}, nil, false},
		{"the minor link refused otherwise", exitCodeErr{2, "exit status 2: error: Failed to create Python minor version link directory: Access is denied. (os error 5)"}, nil, false},
		{"another error", exitCodeErr{2, "exit status 2: error: download failed"}, nil, false},
		{"no exit code", errors.New(minorLink), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := &fakeTools{}
			n := winNative(t, tools)
			touch(t, n.UV(), 0o644)
			tools.Fail = func(c toolCall) error {
				switch c.Path {
				case n.UV():
					touch(t, n.PythonBin(), 0o755) // installed, the junction aside
					return tc.uvErr
				case n.PythonBin():
					return tc.pythonErr
				}
				return nil
			}
			err := n.fetchPython(context.Background(), io.Discard)
			if (err == nil) != tc.ok {
				t.Fatalf("fetchPython = %v, want ok %v", err, tc.ok)
			}
			if want := []string{"python", "install", "--no-bin", "--no-registry", "3.11.17"}; !slices.Equal(tools.Calls[0].Args, want) {
				t.Errorf("uv %q, want %q", tools.Calls[0].Args, want)
			}
			ranPython := len(tools.Calls) == 2 && tools.Calls[1].Path == n.PythonBin() && slices.Equal(tools.Calls[1].Args, []string{"-c", "import sys"})
			if wantPython := tc.name == "the minor link alone" || tc.name == "the minor link, Python broken"; ranPython != wantPython {
				t.Errorf("python.exe -c \"import sys\" ran %v, want %v (calls %+v)", ranPython, wantPython, tools.Calls)
			}
			if !tc.ok && !strings.Contains(err.Error(), "uv python install 3.11.17") {
				t.Errorf("error %q does not name the install", err)
			}
		})
	}
}
