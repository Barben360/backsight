package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Barben360/backsight/internal/fsutil"
)

// The fetch's Windows steps (decision 0080, E34 W5 and W6), on any host:
// a test renders and runs them on Linux with Native.GOOS "windows".

// extractUVZip writes the zip's uv.exe (and nothing else) to dst. The
// archive was hash-checked before: nothing is extracted from another.
func extractUVZip(archive *os.File, dst string) error {
	st, err := archive.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(archive, st.Size())
	if err != nil {
		return fmt.Errorf("the uv archive: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || path.Base(f.Name) != "uv.exe" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("the uv archive: %w", err)
		}
		defer func() { _ = rc.Close() }()
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		out, err := os.CreateTemp(filepath.Dir(dst), ".uv-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(out.Name()) }() // gone once renamed
		if _, err := io.Copy(out, io.LimitReader(rc, maxUV)); err != nil {
			_ = out.Close()
			return fmt.Errorf("the uv archive: %w", err)
		}
		if err := out.Close(); err != nil {
			return err
		}
		return os.Rename(out.Name(), dst)
	}
	return errors.New("the uv archive holds no uv.exe")
}

// uvMinorLink and uvUntrustedMount are in uv's error when, over SSH, it
// cannot create the minor-version junction after installing the Python
// itself (E34 W6: "untrusted mount point", os error 448).
const (
	uvMinorLink      = "minor version link"
	uvUntrustedMount = "os error 448"
)

// uvPythonInstallWindows installs the pinned Python with --no-bin
// --no-registry (E34 ruling 6). uv's exit 2 whose error is the minor
// version link's, refused as an untrusted mount point, is accepted when
// the pinned python.exe then runs: the
// Python is complete, only uv's junction is missing, which nothing uses.
func (n Native) uvPythonInstallWindows(ctx context.Context) error {
	py := n.pins().Python()
	_, err := n.Tools.Run(ctx, n.uvEnv(), nil, n.UV(), "python", "install", "--no-bin", "--no-registry", py)
	if err == nil {
		return nil
	}
	var code interface{ ExitCode() int }
	msg := strings.ToLower(err.Error())
	if !errors.As(err, &code) || code.ExitCode() != 2 || !strings.Contains(msg, uvMinorLink) || !strings.Contains(msg, uvUntrustedMount) {
		return fmt.Errorf("uv python install %s: %w", py, err)
	}
	if _, runErr := n.Tools.Run(ctx, n.toolEnvFor(filepath.Dir(n.PythonBin())), nil, n.PythonBin(), "-c", "import sys"); runErr != nil {
		return fmt.Errorf("uv python install %s: %w; and %s does not run: %w", py, err, n.PythonBin(), runErr)
	}
	return nil
}

// venvPython is what uv venv gets as --python: the pinned interpreter's
// path, but on Windows the pinned patch version. Given a path, uv bases
// the venv on the minor-version junction (python\cpython-3.11-…), which
// a process of an SSH session cannot traverse (os error 448, "untrusted
// mount point": seen on the VM); asked for the exact patch it bases it on
// the patch directory. UV_PYTHON_PREFERENCE=only-managed and
// UV_PYTHON_INSTALL_DIR keep it to the Python Fetch installed.
func (n Native) venvPython() string {
	if n.goos() == "windows" {
		return n.pins().Python()
	}
	return n.PythonBin()
}

// runtimeDLLs are the msvc-runtime wheel's VC++ runtime, which the venv
// holds in Scripts\ (E34 W5; its root has seven of them, not vcruntime140
// nor vcruntime140_1: seen on the VM): Postgres' programs, pg0 and
// onnxruntime need them, and Windows has none of its own without the
// redistributable, which needs admin.
var runtimeDLLs = []string{"concrt140.dll", "msvcp140.dll", "msvcp140_1.dll", "msvcp140_2.dll", "msvcp140_atomic_wait.dll",
	"msvcp140_codecvt_ids.dll", "vcruntime140.dll", "vcruntime140_1.dll", "vcruntime140_threads.dll"}

// dllManifest, under Root(), maps each runtime DLL to its sha256: what
// doctor checks every copy against.
const dllManifest = "dlls.json"

// pg0BinDir is version v's pg0.exe directory.
func (n Native) pg0BinDir(v string) string { return n.join(n.sitePackages(v), "pg0", "bin") }

// pg0Exe is version v's pg0.
func (n Native) pg0Exe(v string) string { return n.join(n.pg0BinDir(v), n.exe("pg0")) }

// onnxCapi is where onnxruntime loads its DLLs from: beside python.exe
// is not enough (E34 W5).
func (n Native) onnxCapi(v string) string { return n.join(n.sitePackages(v), "onnxruntime", "capi") }

// venvDLLDirs are the places fetchVenv copies version v's runtime DLLs
// into: pg0's bin\, onnxruntime\capi\, and the copied installation's
// bin\ when there is one (shared by every version: a new msvc-runtime
// replaces its DLLs, for the manifest to hold for all three).
func (n Native) venvDLLDirs(v string) []string {
	dirs := []string{n.pg0BinDir(v), n.onnxCapi(v)}
	if bin := n.join(n.installation(), "bin"); isDir(bin) {
		dirs = append(dirs, bin)
	}
	return dirs
}

// copyRuntimeDLLs copies the runtime DLLs from version v's Scripts\ into
// each of dirs (E34 ruling 3: the copied installation's bin\, pg0's bin\,
// onnxruntime\capi\) and writes the manifest. A DLL missing from the venv
// is an error: the lock's msvc-runtime was not installed. A copy that is
// already the same is left alone (a running postgres has it loaded).
func (n Native) copyRuntimeDLLs(v string, dirs ...string) error {
	sums := map[string]string{}
	for _, name := range runtimeDLLs {
		src := n.join(n.VenvDir(v), "Scripts", name)
		raw, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("the VC++ runtime DLL %s (the lock's msvc-runtime): %w", name, err)
		}
		sum := sha256.Sum256(raw)
		sums[name] = hex.EncodeToString(sum[:])
		for _, dir := range dirs {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := placeDLL(n.join(dir, name), raw); err != nil {
				return fmt.Errorf("copying %s into %s: %w", name, dir, err)
			}
		}
	}
	raw, err := json.MarshalIndent(sums, "", "  ") // keys sorted
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(n.join(n.Root(), dllManifest), append(raw, '\n'), 0o644)
}

// isDir: p is a directory.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// dllAside is the suffix's start of a replaced DLL moved aside (placeDLL).
const dllAside = ".old-"

// placeDLL writes raw at dst unless dst already holds it. A DLL a running
// program has loaded cannot be replaced, only renamed: it is moved aside
// first, and removed once nothing holds it (at a later placeDLL).
func placeDLL(dst string, raw []byte) error {
	if old, _ := filepath.Glob(dst + dllAside + "*"); len(old) > 0 {
		for _, f := range old {
			_ = os.Remove(f) // still loaded: left for the next time
		}
	}
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, raw) {
		return nil
	}
	err := fsutil.WriteAtomic(dst, raw, 0o755)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	aside := dst + dllAside + randHex()
	if renameErr := os.Rename(dst, aside); renameErr != nil {
		return errors.Join(err, renameErr)
	}
	if err := fsutil.WriteAtomic(dst, raw, 0o755); err != nil {
		_ = os.Rename(aside, dst)
		return err
	}
	_ = os.Remove(aside)
	return nil
}
