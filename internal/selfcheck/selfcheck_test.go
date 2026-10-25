package selfcheck

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/internal/state"
)

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

const version = "9.9.9"

type release struct {
	pub  minisign.PublicKey
	priv minisign.PrivateKey
}

// trustNewKey makes a keypair the binary trusts as its test key.
func trustNewKey(t *testing.T) release {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := signing.TestKey
	signing.TestKey = pub.String()
	t.Cleanup(func() { signing.TestKey = old })
	return release{pub, priv}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// install writes a binary and, unless sums is empty, its checksums.txt and
// a signature by r, as install.sh leaves them in <data>/bin/<v>/.
func (r release) install(t *testing.T, dir string, body []byte, sums string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "backsight")
	if err := os.WriteFile(exe, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if sums == "" {
		return exe
	}
	if err := os.WriteFile(filepath.Join(dir, ChecksumsFile), []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}
	rd := minisign.NewReader(strings.NewReader(sums))
	_, _ = io.Copy(io.Discard, rd)
	sig := rd.SignWithComments(r.priv, "timestamp:1\tfile:checksums.txt\thashed", "signature from minisign secret key")
	if err := os.WriteFile(filepath.Join(dir, SignatureFile), sig, 0o644); err != nil {
		t.Fatal(err)
	}
	return exe
}

func binaryLine(body []byte) string {
	return sum(body) + "  " + BinaryName(version, runtime.GOOS, runtime.GOARCH) + "\n"
}

func TestBinaryName(t *testing.T) {
	for _, c := range [][4]string{
		{"0.4.0", "linux", "amd64", "backsight_0.4.0_linux_amd64"},
		{"0.4.0-rc1", "darwin", "arm64", "backsight_0.4.0-rc1_darwin_arm64"},
		{"0.4.0", "windows", "amd64", "backsight_0.4.0_windows_amd64.exe"},
	} {
		if got := BinaryName(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("BinaryName(%s, %s, %s) = %q, want %q", c[0], c[1], c[2], got, c[3])
		}
	}
}

func TestCheck(t *testing.T) {
	r := trustNewKey(t)
	body := []byte("the released binary")
	archive := strings.Repeat("a", 64) + "  backsight_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz\n"

	t.Run("verified", func(t *testing.T) {
		exe := r.install(t, t.TempDir(), body, archive+binaryLine(body))
		got := Check(exe, version, runtime.GOOS, runtime.GOARCH, now)
		fi, _ := os.Stat(exe)
		if got.Status != Verified || got.KeyID != signing.Keys()[2].ID || got.Version != version || got.Exe != exe ||
			got.Size != fi.Size() || !got.ModTime.Equal(fi.ModTime()) || !got.CheckedAt.Equal(now) {
			t.Errorf("%+v", got)
		}
	})
	cases := []struct {
		name   string
		body   []byte // installed binary (the checksums are always of body)
		sums   string
		status string
		detail string
	}{
		{"no checksums: a dev build", body, "", Unsigned, "no checksums.txt"},
		{"tampered binary", []byte("the released binarY"), archive + binaryLine(body), Failed, "checksum mismatch"},
		{"no entry for this binary", body, archive, Failed, "no entry"},
		{"another version's entry", body, sum(body) + "  backsight_9.9.8_" + runtime.GOOS + "_" + runtime.GOARCH + "\n", Failed, "no entry"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exe := r.install(t, t.TempDir(), c.body, c.sums)
			got := Check(exe, version, runtime.GOOS, runtime.GOARCH, now)
			if got.Status != c.status || !strings.Contains(got.Detail, c.detail) {
				t.Errorf("status %q detail %q, want %q containing %q", got.Status, got.Detail, c.status, c.detail)
			}
		})
	}
	t.Run("checksums without a signature", func(t *testing.T) {
		dir := t.TempDir()
		exe := r.install(t, dir, body, archive+binaryLine(body))
		_ = os.Remove(filepath.Join(dir, SignatureFile))
		if got := Check(exe, version, runtime.GOOS, runtime.GOARCH, now); got.Status != Failed || !strings.Contains(got.Detail, "not signed") {
			t.Errorf("%+v", got)
		}
	})
	t.Run("signed by an untrusted key", func(t *testing.T) {
		dir := t.TempDir()
		exe := r.install(t, dir, body, archive+binaryLine(body))
		signing.TestKey = ""
		defer func() { signing.TestKey = r.pub.String() }()
		if got := Check(exe, version, runtime.GOOS, runtime.GOARCH, now); got.Status != Failed || !strings.Contains(got.Detail, "does not trust") {
			t.Errorf("%+v", got)
		}
	})
	t.Run("checksums edited after signing", func(t *testing.T) {
		dir := t.TempDir()
		other := []byte("another binary")
		exe := r.install(t, dir, other, archive+binaryLine(body))
		_ = os.WriteFile(filepath.Join(dir, ChecksumsFile), []byte(archive+binaryLine(other)), 0o644)
		if got := Check(exe, version, runtime.GOOS, runtime.GOARCH, now); got.Status != Failed || !strings.Contains(got.Detail, "does not match its signature") {
			t.Errorf("%+v", got)
		}
	})
	// The shim is a symlink elsewhere: the checksums are the target's.
	t.Run("through a symlink", func(t *testing.T) {
		exe := r.install(t, t.TempDir(), body, archive+binaryLine(body))
		link := filepath.Join(t.TempDir(), "current")
		if err := os.Symlink(exe, link); err != nil {
			t.Skip(err)
		}
		real, _ := filepath.EvalSymlinks(exe)
		if got := Check(link, version, runtime.GOOS, runtime.GOARCH, now); got.Status != Verified || got.Exe != real {
			t.Errorf("%+v", got)
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		if got := Check(filepath.Join(t.TempDir(), "gone"), version, runtime.GOOS, runtime.GOARCH, now); got.Status != Failed {
			t.Errorf("%+v", got)
		}
	})
}

func dirs(t *testing.T) paths.Dirs {
	t.Helper()
	h := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(h, "data"), Config: filepath.Join(h, "config"), State: filepath.Join(h, "state"), Bin: filepath.Join(h, "bin")}
	if err := os.MkdirAll(d.State, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func setVersion(t *testing.T, v string) {
	old := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = old })
}

// Ensure checks a binary once: the record answers later calls, until the
// binary or the version changes.
func TestEnsureChecksOncePerBinary(t *testing.T) {
	r := trustNewKey(t)
	setVersion(t, version)
	d := dirs(t)
	body := []byte("the released binary")
	dir := filepath.Join(d.Data, "bin", version)
	exe := r.install(t, dir, body, binaryLine(body))

	got, err := Ensure(d, exe)
	if err != nil || got.Status != Verified {
		t.Fatalf("first run: %+v %v", got, err)
	}
	st, _ := state.Load(d.StateFile())
	if st.SelfCheck == nil || st.SelfCheck.Status != Verified || st.SelfCheck.Exe == "" {
		t.Fatalf("not recorded: %+v", st.SelfCheck)
	}

	// The record answers: the checksums are not read again.
	_ = os.Remove(filepath.Join(dir, ChecksumsFile))
	if got, _ := Ensure(d, exe); got.Status != Verified {
		t.Errorf("second run re-checked: %+v", got)
	}

	// A new version of the binary at the same path is checked again.
	setVersion(t, "9.9.10")
	if got, _ := Ensure(d, exe); got.Status != Unsigned || got.Version != "9.9.10" {
		t.Errorf("after a version change: %+v", got)
	}
	setVersion(t, version)
	if got, _ := Ensure(d, exe); got.Status != Unsigned || got.Version != version {
		t.Errorf("back to the first version, not checked again: %+v", got)
	}
}

// A binary replaced in place is checked again: the record names its size
// and modification time.
func TestEnsureChecksAReplacedBinaryAgain(t *testing.T) {
	r := trustNewKey(t)
	setVersion(t, version)
	body := []byte("the released binary")
	for name, replace := range map[string]func(exe string, mod time.Time){
		"same size, later time": func(exe string, mod time.Time) {
			_ = os.WriteFile(exe, []byte("the released binarX"), 0o755)
			_ = os.Chtimes(exe, mod.Add(time.Second), mod.Add(time.Second))
		},
		"other size, same time": func(exe string, mod time.Time) {
			_ = os.WriteFile(exe, []byte("the released binary, longer"), 0o755)
			_ = os.Chtimes(exe, mod, mod)
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := dirs(t)
			exe := r.install(t, filepath.Join(d.Data, "bin", version), body, binaryLine(body))
			if got, err := Ensure(d, exe); err != nil || got.Status != Verified {
				t.Fatalf("%+v %v", got, err)
			}
			fi, _ := os.Stat(exe)
			replace(exe, fi.ModTime())
			if got, _ := Ensure(d, exe); got.Status != Failed || !strings.Contains(got.Detail, "checksum mismatch") {
				t.Errorf("the replaced binary was not checked again: %+v", got)
			}
		})
	}
}

// Current never writes state; it uses a record that matches.
func TestCurrentIsReadOnly(t *testing.T) {
	r := trustNewKey(t)
	setVersion(t, version)
	d := dirs(t)
	body := []byte("the released binary")
	dir := filepath.Join(d.Data, "bin", version)
	exe := r.install(t, dir, body, binaryLine(body))

	if got := Current(d, exe); got.Status != Verified {
		t.Fatalf("%+v", got)
	}
	if _, err := os.Stat(d.StateFile()); !os.IsNotExist(err) {
		t.Errorf("Current wrote state.json: %v", err)
	}
	if _, err := Ensure(d, exe); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, ChecksumsFile), []byte(binaryLine([]byte("x"))), 0o644)
	before, _ := os.ReadFile(d.StateFile())
	if got := Current(d, exe); got.Status != Verified {
		t.Errorf("the matching record was not used: %+v", got)
	}
	if after, _ := os.ReadFile(d.StateFile()); !bytes.Equal(before, after) {
		t.Error("Current changed state.json")
	}
}
