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

var version = "9.9.9"

func goos() string { return runtime.GOOS }

// release builds a release directory for this machine, signed by k.
func release(t *testing.T, k key) (dir, archive string) {
	t.Helper()
	return releaseIn(t, t.TempDir(), k, false)
}

// releaseIn builds the release in dir; link makes the backsight member a
// symlink, as a hostile archive could.
func releaseIn(t *testing.T, dir string, k key, link bool) (string, string) {
	t.Helper()
	archive := "backsight_" + version + "_" + goos() + "_" + runtime.GOARCH + ".tar.gz"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho fake backsight " + version + "\n")
	if link {
		_ = tw.WriteHeader(&tar.Header{Name: "backsight", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"})
	} else {
		_ = tw.WriteHeader(&tar.Header{Name: "backsight", Mode: 0o755, Size: int64(len(body))})
		_, _ = tw.Write(body)
	}
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
	return installIn(t, t.TempDir(), relDir, pubkey, path)
}

func installIn(t *testing.T, home, relDir, pubkey, path string, env ...string) (string, string, error) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl missing")
	}
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + path, "BACKSIGHT_RELEASE_DIR=" + relDir, "BACKSIGHT_INSTALL_PUBKEY=" + pubkey}, env...)
	b, err := cmd.CombinedOutput()
	return home, string(b), err
}

func dataDir(home string) string {
	if goos() == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "backsight")
	}
	return filepath.Join(home, ".local", "share", "backsight")
}

func installedBinary(home string) string {
	return filepath.Join(dataDir(home), "bin", version, "backsight")
}

// pathEntry checks the command on PATH: link → the shim <data>/bin/current
// (decision 0067 block review: an update moves the shim, so the PATH
// command follows it, and pruning old binaries never breaks it) → binary.
func pathEntry(t *testing.T, link, data, binary string) {
	t.Helper()
	shim := filepath.Join(data, "bin", "current")
	if got, err := os.Readlink(link); err != nil || got != shim {
		t.Errorf("%s → %q (%v), want the shim %s", link, got, err, shim)
	}
	if got, err := os.Readlink(shim); err != nil || got != binary {
		t.Errorf("%s → %q (%v), want %s", shim, got, err, binary)
	}
}

func TestInstallVerifiesAndInstallsWithOpenSSL(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	home, out, err := install(t, dir, k.pub, toolsWithout(t))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	link := filepath.Join(home, ".local", "bin", "backsight")
	pathEntry(t, link, dataDir(home), installedBinary(home))
	if run, err := exec.Command(link).Output(); err != nil || string(run) != "fake backsight "+version+"\n" {
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
	arch := filepath.Join(dir, "backsight_"+version+"_"+goos()+"_"+runtime.GOARCH+".tar.gz")
	_ = os.WriteFile(arch, []byte("swapped archive"), 0o644)
	if _, out, err := install(t, dir, k.pub, os.Getenv("PATH")); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Errorf("minisign path accepted a swapped archive: %v\n%s", err, out)
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
		{"legacy signature", func(t *testing.T, dir, _ string, k key) string {
			// "Ed": the signature over the file itself, not its prehash
			f := filepath.Join(dir, "checksums.txt")
			data, _ := os.ReadFile(f)
			sig := ed25519.Sign(k.priv, data)
			comment := "timestamp:1\tfile:checksums.txt"
			global := ed25519.Sign(k.priv, append(append([]byte{}, sig...), comment...))
			_ = os.WriteFile(f+".minisig", []byte("untrusted comment: x\n"+base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), k.id...), sig...))+
				"\ntrusted comment: "+comment+"\n"+base64.StdEncoding.EncodeToString(global)+"\n"), 0o644)
			return k.pub
		}, "not a prehashed"},
		{"signature made for another file", func(t *testing.T, dir, _ string, k key) string {
			k.sign(t, filepath.Join(dir, "checksums.txt"), "timestamp:1792845259\tfile:old-checksums.txt\thashed")
			return k.pub
		}, "made for another file"},
		{"archive name escaping the directory", func(t *testing.T, dir, archive string, k key) string {
			p := filepath.Join(dir, "checksums.txt")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, []byte(strings.ReplaceAll(string(b), archive, "backsight_../"+archive)), 0o644)
			k.sign(t, p, "timestamp:1\tfile:checksums.txt\thashed")
			return k.pub
		}, "unexpected archive name"},
		{"symlinked binary", func(t *testing.T, dir, _ string, k key) string {
			_, _ = releaseIn(t, dir, k, true)
			return k.pub
		}, "not a regular file"},
		{"missing signature", func(t *testing.T, dir, _ string, k key) string {
			_ = os.Remove(filepath.Join(dir, "checksums.txt.minisig"))
			return k.pub
		}, "checksums.txt.minisig is not in"},
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

func TestInstallUnderBackSightHomeAndUpgrade(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	path := toolsWithout(t)
	home := t.TempDir()
	bh := filepath.Join(home, "bs")
	if _, out, err := installIn(t, home, dir, k.pub, path, "BACKSIGHT_HOME="+bh); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	link := filepath.Join(bh, "bin", "backsight")
	pathEntry(t, link, filepath.Join(bh, "data"), filepath.Join(bh, "data", "bin", version, "backsight"))
	// A newer release replaces the link, the older binary stays in place.
	old := version
	version = "9.9.10"
	t.Cleanup(func() { version = old })
	dir2, _ := release(t, k)
	if _, out, err := installIn(t, home, dir2, k.pub, path, "BACKSIGHT_HOME="+bh); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if run, err := exec.Command(link).Output(); err != nil || string(run) != "fake backsight 9.9.10\n" {
		t.Errorf("after the upgrade the link runs %q (%v)", run, err)
	}
	pathEntry(t, link, filepath.Join(bh, "data"), filepath.Join(bh, "data", "bin", "9.9.10", "backsight"))
	if _, err := os.Stat(filepath.Join(bh, "data", "bin", old, "backsight")); err != nil {
		t.Error("the previous binary was removed")
	}
}

// A newer release that does not verify leaves the working install alone.
func TestARefusedUpgradeKeepsTheWorkingInstall(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	path := toolsWithout(t)
	home, out, err := install(t, dir, k.pub, path)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	good := installedBinary(home)
	old := version
	version = "9.9.10"
	t.Cleanup(func() { version = old })
	dir2, archive := release(t, k)
	_ = os.WriteFile(filepath.Join(dir2, archive), []byte("not the signed archive"), 0o644)
	if _, out, err := installIn(t, home, dir2, k.pub, path); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("the tampered 9.9.10 was not refused: %v\n%s", err, out)
	}
	link := filepath.Join(home, ".local", "bin", "backsight")
	if run, err := exec.Command(link).Output(); err != nil || string(run) != "fake backsight "+old+"\n" {
		t.Errorf("after the refused upgrade the link runs %q (%v), want %s", run, err, old)
	}
	if _, err := os.Stat(installedBinary(home)); err == nil {
		t.Error("the refused 9.9.10 was installed")
	}
	if _, err := os.Stat(good); err != nil {
		t.Errorf("the working %s binary is gone: %v", old, err)
	}
}

// The verified checksums.txt and its signature stay next to the binary:
// the binary checks itself against them on its first run (SPEC §11.1).
func TestInstallKeepsTheSignedChecksums(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	home, out, err := install(t, dir, k.pub, toolsWithout(t))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, f := range []string{"checksums.txt", "checksums.txt.minisig"} {
		want, _ := os.ReadFile(filepath.Join(dir, f))
		got, err := os.ReadFile(filepath.Join(filepath.Dir(installedBinary(home)), f))
		if err != nil || len(want) == 0 || string(got) != string(want) {
			t.Errorf("%s next to the binary: %v", f, err)
		}
	}
}

// An install.sh from before decision 0067's block review linked the PATH
// command to a versioned binary: a new install points it at the shim.
func TestInstallPointsAnOlderPathLinkAtTheShim(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	home := t.TempDir()
	old := filepath.Join(dataDir(home), "bin", "9.9.8", "backsight")
	link := filepath.Join(home, ".local", "bin", "backsight")
	for _, d := range []string{filepath.Dir(old), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho fake backsight 9.9.8\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, link); err != nil {
		t.Fatal(err)
	}
	if _, out, err := installIn(t, home, dir, k.pub, toolsWithout(t)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pathEntry(t, link, dataDir(home), installedBinary(home))
	if run, err := exec.Command(link).Output(); err != nil || string(run) != "fake backsight "+version+"\n" {
		t.Errorf("the PATH command runs %q (%v)", run, err)
	}
}
