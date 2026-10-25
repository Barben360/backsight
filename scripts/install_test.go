// Package scripts holds the tests of the shell scripts shipped with
// backsight: install.sh runs against a release built and signed here.
package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

type key struct {
	id   []byte
	priv ed25519.PrivateKey
	pub  string // minisign public key line
}

func newKey(t *testing.T) key {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	return key{id, priv, base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), id...), pub...))}
}

// sign writes file.minisig as minisign does by default: a prehashed
// (BLAKE2b-512) signature, then a signature over it and the trusted comment.
func (k key) sign(t *testing.T, file, comment string) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	h := blake2b.Sum512(data)
	sig := ed25519.Sign(k.priv, h[:])
	global := ed25519.Sign(k.priv, append(append([]byte{}, sig...), comment...))
	lines := "untrusted comment: signature from minisign secret key\n" +
		base64.StdEncoding.EncodeToString(append(append([]byte("ED"), k.id...), sig...)) + "\n" +
		"trusted comment: " + comment + "\n" + base64.StdEncoding.EncodeToString(global) + "\n"
	if err := os.WriteFile(file+".minisig", []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
}

const version = "9.9.9"

func goos() string { return runtime.GOOS }

// release builds a release directory for this machine, signed by k.
func release(t *testing.T, k key) (dir, archive string) {
	t.Helper()
	dir = t.TempDir()
	archive = "backsight_" + version + "_" + goos() + "_" + runtime.GOARCH + ".tar.gz"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho fake backsight\n")
	_ = tw.WriteHeader(&tar.Header{Name: "backsight", Mode: 0o755, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(filepath.Join(dir, archive), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	other := "backsight_" + version + "_plan9_mips.tar.gz"
	checksums := hex.EncodeToString(sum[:]) + "  " + archive + "\n" + strings.Repeat("0", 64) + "  " + other + "\n"
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(checksums), 0o644); err != nil {
		t.Fatal(err)
	}
	k.sign(t, filepath.Join(dir, "checksums.txt"), "timestamp:1792845259\tfile:checksums.txt\thashed")
	return dir, archive
}

// toolsWithout is a PATH holding the tools install.sh uses, minisign left
// out so that the openssl path runs.
func toolsWithout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"sh", "openssl", "tar", "gzip", "awk", "sed", "od", "base64", "head", "tail", "cut",
		"sha256sum", "shasum", "mktemp", "mkdir", "rm", "cp", "install", "mv", "ln", "uname", "cat", "basename", "tr"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(dir, tool))
		}
	}
	return dir
}

func install(t *testing.T, relDir, pubkey, path string) (home, out string, err error) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl missing")
	}
	home = t.TempDir()
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = []string{"HOME=" + home, "PATH=" + path, "BACKSIGHT_RELEASE_DIR=" + relDir, "BACKSIGHT_INSTALL_PUBKEY=" + pubkey}
	b, err := cmd.CombinedOutput()
	return home, string(b), err
}

func installedBinary(home string) string {
	if goos() == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "backsight", "bin", version, "backsight")
	}
	return filepath.Join(home, ".local", "share", "backsight", "bin", version, "backsight")
}

func TestInstallVerifiesAndInstallsWithOpenSSL(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	home, out, err := install(t, dir, k.pub, toolsWithout(t))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	link := filepath.Join(home, ".local", "bin", "backsight")
	target, lerr := filepath.EvalSymlinks(link)
	want, _ := filepath.EvalSymlinks(installedBinary(home))
	if lerr != nil || target != want {
		t.Errorf("%s → %s (%v), want %s", link, target, lerr, want)
	}
	if run, err := exec.Command(link).Output(); err != nil || string(run) != "fake backsight\n" {
		t.Errorf("installed binary: %q %v", run, err)
	}
	for _, s := range []string{"backsight " + version + " installed", "export PATH=", "next: backsight setup"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestInstallWithMinisign(t *testing.T) {
	if _, err := exec.LookPath("minisign"); err != nil {
		t.Skip("minisign missing")
	}
	k := newKey(t)
	dir, _ := release(t, k)
	if _, out, err := install(t, dir, k.pub, os.Getenv("PATH")); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	_ = os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte("tampered\n"), 0o644)
	if _, out, err := install(t, dir, k.pub, os.Getenv("PATH")); err == nil || !strings.Contains(out, "bad signature") {
		t.Errorf("minisign accepted a tampered file: %v\n%s", err, out)
	}
}

func TestInstallRefusesWhatDoesNotVerify(t *testing.T) {
	path := toolsWithout(t)
	cases := []struct {
		name   string
		tamper func(t *testing.T, dir, archive string, k key) string // returns the key to trust
		want   string
	}{
		{"archive changed", func(t *testing.T, dir, archive string, k key) string {
			_ = os.WriteFile(filepath.Join(dir, archive), []byte("not the signed archive"), 0o644)
			return k.pub
		}, "checksum mismatch"},
		{"checksums changed", func(t *testing.T, dir, _ string, k key) string {
			f, _ := os.OpenFile(filepath.Join(dir, "checksums.txt"), os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.WriteString("\n")
			_ = f.Close()
			return k.pub
		}, "bad signature"},
		{"another key", func(t *testing.T, _, _ string, _ key) string { return newKey(t).pub }, "signed by another key"},
		{"trusted comment changed", func(t *testing.T, dir, _ string, k key) string {
			p := filepath.Join(dir, "checksums.txt.minisig")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, []byte(strings.Replace(string(b), "timestamp:1792845259", "timestamp:1792845260", 1)), 0o644)
			return k.pub
		}, "bad trusted comment signature"},
		{"no archive for this machine", func(t *testing.T, dir, archive string, k key) string {
			p := filepath.Join(dir, "checksums.txt")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, []byte(strings.ReplaceAll(string(b), archive, "backsight_9.9.9_plan9_arm.tar.gz")), 0o644)
			k.sign(t, p, "timestamp:1\tfile:checksums.txt\thashed")
			return k.pub
		}, "no archive for"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newKey(t)
			dir, archive := release(t, k)
			trust := c.tamper(t, dir, archive, k)
			home, out, err := install(t, dir, trust, path)
			if err == nil || !strings.Contains(out, c.want) {
				t.Errorf("want a refusal with %q, got %v\n%s", c.want, err, out)
			}
			if _, err := os.Stat(installedBinary(home)); err == nil {
				t.Error("a refused release was installed")
			}
		})
	}
}
