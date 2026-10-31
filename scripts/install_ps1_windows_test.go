//go:build windows

package scripts

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// userPathKey gives one install a throwaway user PATH: a key under HKCU
// that a testhooks build of backsight writes instead of Environment
// (BACKSIGHT_TEST_USERPATH_KEY), deleted after the test. No test touches
// the real user PATH.
func userPathKey(t *testing.T, name string) string {
	t.Helper()
	key := fmt.Sprintf(`Software\backsight-test-scripts-%d-%s`, os.Getpid(), name)
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, key) })
	return key
}

// userPath reads the throwaway key's Path ("" when the key or the value
// is absent).
func userPath(t *testing.T, key string) string {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func readRaw(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// windowsRelease writes a release of exe as version 9.9.9 into a new
// directory: the zip goreleaser makes (backsight.exe beside the docs), the
// bare binary's and the zip's lines in checksums.txt, signed by k.
func windowsRelease(t *testing.T, exe []byte, k key) (dir, archive string) {
	t.Helper()
	dir = t.TempDir()
	archive = "backsight_9.9.9_windows_amd64.zip"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		body []byte
	}{{"LICENSE", []byte("license\n")}, {"backsight.exe", exe}} {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(f.body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, archive), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	line := func(b []byte, name string) string {
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:]) + "  " + name + "\n"
	}
	sums := line([]byte("decoy"), "backsight_9.9.9_linux_amd64.tar.gz") + line(exe, "backsight_9.9.9_windows_amd64.exe") + line(buf.Bytes(), archive)
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}
	k.sign(t, filepath.Join(dir, "checksums.txt"), "timestamp:1792845259\tfile:checksums.txt\thashed")
	return dir, archive
}

// install.ps1 installs a release from a directory as a user does: the
// binary under <data>\bin\9.9.9 with the signed checksums beside it, the
// shim current.exe, the PATH copy <bin>\backsight.exe, and <bin> on the
// user PATH, where a new process finds it. The binary is a test build
// trusting the release's key (testkey), so its own self-check accepts the
// checksums install.ps1 accepted, and writing the throwaway user PATH
// (testhooks).
func TestInstallPs1FromADirectory(t *testing.T) {
	shells := powerShells(t)
	k := newKey(t)
	exePath := buildFor(t, t.TempDir(), "windows", "amd64",
		"-X "+pkg+"/internal/buildinfo.Version=9.9.9 -X "+pkg+"/internal/signing.LinkedTestKey="+k.pub, "-tags", "testkey,testhooks")
	exe, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := windowsRelease(t, exe, k)
	for _, shell := range shells {
		t.Run(shellName(shell), func(t *testing.T) {
			bh := filepath.Join(t.TempDir(), "bs")
			key := userPathKey(t, shellName(shell))
			out, err := runInstallPs1(t, shell, []string{"BACKSIGHT_HOME=" + bh, "BACKSIGHT_RELEASE_DIR=" + rel,
				"BACKSIGHT_INSTALL_PUBKEY=" + k.pub, "BACKSIGHT_TEST_USERPATH_KEY=" + key})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			bin := filepath.Join(bh, "bin")
			command := filepath.Join(bin, "backsight.exe")
			for _, s := range []string{
				"BACKSIGHT_INSTALL_PUBKEY is set: the release is checked against " + k.pub,
				"backsight 9.9.9 installed: " + command,
				bin + " is on your user PATH",
				"next: backsight setup",
				"a coding agent installing for you: backsight guide agent-guide",
			} {
				if !strings.Contains(out, s) {
					t.Errorf("output lacks %q:\n%s", s, out)
				}
			}
			dest := filepath.Join(bh, "data", "bin", "9.9.9")
			for path, want := range map[string][]byte{
				filepath.Join(dest, "backsight.exe"):            exe,
				filepath.Join(dest, "checksums.txt"):            readRaw(t, filepath.Join(rel, "checksums.txt")),
				filepath.Join(dest, "checksums.txt.minisig"):    readRaw(t, filepath.Join(rel, "checksums.txt.minisig")),
				filepath.Join(bh, "data", "bin", "current.exe"): exe,
				command: exe,
			} {
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s: %v, %d bytes, want %d", path, err, len(got), len(want))
				}
			}
			up := userPath(t, key)
			if !strings.Contains(";"+up+";", ";"+bin+";") {
				t.Fatalf("user PATH %q lacks %s", up, bin)
			}
			// A new process with the new user PATH runs the PATH copy.
			sys := filepath.Join(os.Getenv("SystemRoot"), "System32")
			cmd := exec.Command(filepath.Join(sys, "cmd.exe"), "/d", "/c", "where backsight && backsight version")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "PATH="+up+";"+sys, "BACKSIGHT_HOME="+bh)
			got, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("backsight from the user PATH: %v\n%s", err, got)
			}
			lines := strings.Split(strings.ReplaceAll(string(got), "\r\n", "\n"), "\n")
			if !strings.EqualFold(lines[0], command) || !strings.Contains(string(got), "backsight 9.9.9 ") ||
				!strings.Contains(string(got), "test build: trusts the test signing key") {
				t.Errorf("want %s running backsight 9.9.9, the test build, got:\n%s", command, got)
			}
		})
	}
}

// A zip whose SHA-256 is not the signed one is refused before anything is
// written. The release is the real v0.1.18-m7's checksums.txt, verified
// against the key install.ps1 embeds (no override, so no warning), with a
// zip that is not the one it lists.
func TestInstallPs1RefusesABadZipHash(t *testing.T) {
	rel := t.TempDir()
	for _, f := range []string{"checksums.txt", "checksums.txt.minisig"} {
		if err := os.WriteFile(filepath.Join(rel, f), readRaw(t, fixture(t, "v0.1.18-m7/"+f)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rel, "backsight_0.1.18-m7_windows_amd64.zip"), []byte("PK not the signed zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			bh := filepath.Join(t.TempDir(), "bs")
			key := userPathKey(t, shellName(shell))
			out, err := runInstallPs1(t, shell, []string{"BACKSIGHT_HOME=" + bh, "BACKSIGHT_RELEASE_DIR=" + rel, "BACKSIGHT_TEST_USERPATH_KEY=" + key})
			if err == nil || !strings.Contains(out, "backsight install: backsight_0.1.18-m7_windows_amd64.zip: checksum mismatch") {
				t.Fatalf("want a checksum refusal, got %v\n%s", err, out)
			}
			if strings.Contains(out, "is set") {
				t.Errorf("a warning without any override:\n%s", out)
			}
			if _, err := os.Stat(bh); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s written by a refused install (%v)", bh, err)
			}
			if up := userPath(t, key); up != "" {
				t.Errorf("user PATH written by a refused install: %q", up)
			}
		})
	}
}
