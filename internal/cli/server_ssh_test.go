package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aead.dev/minisign"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/scripts"
)

// This PC checks the release matching its version for the host's
// platform, not its own, and refuses one that fails a check or that it
// cannot match.
func TestStageReleaseVerifiesTheHostsArchive(t *testing.T) {
	ctx := context.Background()
	key := trustTestKey(t)
	src := t.TempDir()
	signedRelease(t, src, "9.9.9", key, relOpts{platform: "linux_arm64"})
	work := t.TempDir()
	archive, keyID, err := stageRelease(ctx, release.Dir{Path: src}, "9.9.9", "arm64", work)
	if err != nil {
		t.Fatal(err)
	}
	if archive != "backsight_9.9.9_linux_arm64.tar.gz" || keyID != fmt.Sprintf("%016X", key.ID()) {
		t.Errorf("stageRelease = %q, %q; want the arm64 archive and key %016X", archive, keyID, key.ID())
	}
	for _, f := range []string{archive, "checksums.txt", "checksums.txt.minisig"} {
		if _, err := os.Stat(filepath.Join(work, f)); err != nil {
			t.Errorf("%s not staged: %v", f, err)
		}
	}

	_, other, _ := minisign.GenerateKey(rand.Reader)
	for name, c := range map[string]struct {
		o    relOpts
		want string
	}{
		"tampered archive": {relOpts{platform: "linux_arm64", tamperArchive: true}, "checksum"},
		"another key":      {relOpts{platform: "linux_arm64", key: &other}, "signature"},
	} {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			signedRelease(t, src, "9.9.9", key, c.o)
			_, _, err := stageRelease(ctx, release.Dir{Path: src}, "9.9.9", "arm64", t.TempDir())
			if output.CodeOf(err) != output.Blocked || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v (exit %d), want exit 20 naming %q", err, output.CodeOf(err), c.want)
			}
		})
	}
	if _, _, err := stageRelease(ctx, release.Dir{Path: src}, "9.9.8", "arm64", t.TempDir()); output.CodeOf(err) != output.Blocked || !strings.Contains(err.Error(), "9.9.8") {
		t.Errorf("absent version: %v (exit %d), want exit 20 naming 9.9.8", err, output.CodeOf(err))
	}
	if _, _, err := stageRelease(ctx, release.Dir{Path: src}, "9.9.9", "riscv64", t.TempDir()); output.CodeOf(err) != output.Blocked || !strings.Contains(err.Error(), "riscv64") {
		t.Errorf("no archive for the host: %v (exit %d), want exit 20 naming riscv64", err, output.CodeOf(err))
	}
	for _, v := range []string{"dev", "0.1.7-3-gabc1234", "0.1.7-dirty"} {
		if _, _, err := stageRelease(ctx, release.Dir{Path: src}, v, "arm64", t.TempDir()); output.CodeOf(err) != output.Blocked ||
			!strings.Contains(err.Error(), "server install --print-commands") || !strings.Contains(err.Error(), v) || strings.Contains(err.Error(), "--source") {
			t.Errorf("development build %s: %v (exit %d), want exit 20 naming --print-commands, not --source", v, err, output.CodeOf(err))
		}
	}
}

// loopbackHost stands for the host: a fake ssh on PATH that drops ssh's
// options and the target and runs the remote command line as sshd does,
// through a login shell (sh here), with the host's HOME and TMPDIR and env
// (NAME=value words). It needs a verifier install.sh accepts.
func loopbackHost(t *testing.T, env ...string) (hostHome, hostTmp string) {
	t.Helper()
	hostHome, hostTmp, _ = loopback(t, 0, env...)
	return hostHome, hostTmp
}

// loopback is loopbackHost passing the host only the first cut bytes of
// stdin when cut > 0; sshDir/reached exists once ssh ran.
func loopback(t *testing.T, cut int, env ...string) (hostHome, hostTmp, sshDir string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the loopback host is a Linux one")
	}
	if !hasVerifier() {
		t.Skip("neither minisign nor OpenSSL 3 is installed")
	}
	hostHome, hostTmp = t.TempDir(), t.TempDir()
	words := []string{"HOME=" + shellQuote(hostHome), "TMPDIR=" + shellQuote(hostTmp)}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		words = append(words, k+"="+shellQuote(v))
	}
	filter := ""
	if cut > 0 {
		filter = fmt.Sprintf("head -c %d | ", cut)
	}
	sshDir = fakeSSHOnPath(t, `: > "$(dirname "$0")/reached"
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift 2
`+filter+`env -u BACKSIGHT_HOME -u XDG_DATA_HOME -u BACKSIGHT_INSTALL_PUBKEY -u BACKSIGHT_RELEASE_DIR -u SSH_CONNECTION `+strings.Join(words, " ")+` sh -c "$1"`)
	withSSH(t, execSSH{})
	return hostHome, hostTmp, sshDir
}

func hasVerifier() bool {
	if _, err := exec.LookPath("minisign"); err == nil {
		return true
	}
	out, err := exec.Command("openssl", "version").Output()
	return err == nil && len(out) > 8 && strings.HasPrefix(string(out), "OpenSSL ") && out[8] >= '3' && out[8] <= '9'
}

// The probe reads what the host is, where install.sh would put the shim
// and the address this ssh connection reached it on.
func TestProbeHostReadsTheFacts(t *testing.T) {
	hostHome, _ := loopbackHost(t, "SSH_CONNECTION=100.101.1.9 50022 100.101.1.2 22")
	f, err := probeHost(context.Background(), "u@h")
	if err != nil {
		t.Fatal(err)
	}
	if f.Addr != "100.101.1.2" || f.OS != "Linux" || f.GOARCH != runtime.GOARCH || f.Machine == "" {
		t.Errorf("facts = %+v; want Addr 100.101.1.2, OS Linux, GOARCH %s", f, runtime.GOARCH)
	}
	if want := filepath.Join(hostHome, ".local/share/backsight/bin/current"); f.Shim != want || f.HasShim {
		t.Errorf("Shim %q (HasShim %v), want %q not there", f.Shim, f.HasShim, want)
	}
	if f.Verifier != "minisign" && f.Verifier != "openssl" {
		t.Errorf("Verifier %q, want minisign or openssl", f.Verifier)
	}
	if len(f.Missing) != 0 {
		t.Errorf("Missing %q", f.Missing)
	}
	if f.Linger != "" && f.Linger != "yes" && f.Linger != "no" {
		t.Errorf("Linger %q", f.Linger)
	}

	bh := filepath.Join(t.TempDir(), "bh")
	loopbackHost(t, "BACKSIGHT_HOME="+bh)
	shim := filepath.Join(bh, "data/bin/current")
	if err := os.MkdirAll(filepath.Dir(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(bh, "data/bin/9.9.9/backsight"), shim); err != nil {
		t.Fatal(err)
	}
	f, err = probeHost(context.Background(), "u@h")
	if err != nil {
		t.Fatal(err)
	}
	if f.Shim != shim || !f.HasShim || f.Addr != "" {
		t.Errorf("with BACKSIGHT_HOME: Shim %q (HasShim %v, Addr %q), want %q there, no address", f.Shim, f.HasShim, f.Addr, shim)
	}
}

// The facts the probe prints map to what server install checks: a
// machine it has no release for, a verifier that cannot check Ed25519
// and missing tools say so.
func TestProbeHostMapsTheAnswer(t *testing.T) {
	for _, c := range []struct {
		out  string
		want hostFacts
	}{
		{"os=Linux\nmachine=aarch64\nconn=fd7a:115c:a1e0::9 50022 fd7a:115c:a1e0::2 22\nshim=/h/.local/share/backsight/bin/current\nhas_shim=yes\nverifier=openssl\nmissing=\nlinger=yes\n",
			hostFacts{OS: "Linux", Machine: "aarch64", GOARCH: "arm64", Addr: "fd7a:115c:a1e0::2", Shim: "/h/.local/share/backsight/bin/current", HasShim: true, Verifier: "openssl", Linger: "yes"}},
		{"motd from a profile\nos=Linux\nmachine=riscv64\nconn=\nshim=/a=b/bin/current\nhas_shim=no\nverifier=\nmissing= tar mktemp\nlinger=\n",
			hostFacts{OS: "Linux", Machine: "riscv64", Shim: "/a=b/bin/current", Missing: []string{"tar", "mktemp"}}},
		{"os=Linux\nmachine=x86_64\nconn=1.2.3.4 5 10.0.0.2 22\nshim=/s\nhas_shim=no\nverifier=libressl\nmissing=\nlinger=no\n",
			hostFacts{OS: "Linux", Machine: "x86_64", GOARCH: "amd64", Addr: "10.0.0.2", Shim: "/s", Linger: "no"}},
	} {
		withSSH(t, &fakeSSH{stdout: c.out})
		got, err := probeHost(context.Background(), "u@h")
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", c.want) {
			t.Errorf("probe of\n%s= %#v\nwant %#v", c.out, got, c.want)
		}
	}
	withSSH(t, &fakeSSH{stdout: "Welcome\n"})
	if _, err := probeHost(context.Background(), "box"); output.CodeOf(err) != output.Blocked || !strings.Contains(err.Error(), "box") {
		t.Errorf("no answer: %v (exit %d), want exit 20 naming the host", err, output.CodeOf(err))
	}
	withSSH(t, &fakeSSH{code: 3, stderr: "sh: boom\n"})
	if _, err := probeHost(context.Background(), "u@h"); output.CodeOf(err) != output.Blocked || !strings.Contains(err.Error(), "sh: boom") {
		t.Errorf("failing probe: %v (exit %d), want exit 20 quoting the host", err, output.CodeOf(err))
	}
}

// stageHere signs release 9.9.9 for this machine and stages it into a
// fresh work directory.
func stageHere(t *testing.T) (work, archive string) {
	t.Helper()
	src := t.TempDir()
	signedRelease(t, src, "9.9.9", trustTestKey(t), relOpts{platform: "linux_" + runtime.GOARCH})
	work = t.TempDir()
	archive, _, err := stageRelease(context.Background(), release.Dir{Path: src}, "9.9.9", runtime.GOARCH, work)
	if err != nil {
		t.Fatal(err)
	}
	return work, archive
}

// The host runs this version's install.sh on the release this PC sent:
// installed beside its signed checksums, the shim switched, nothing left
// in its temp directory.
func TestInstallOnHostRunsInstallShThere(t *testing.T) {
	hostHome, hostTmp := loopbackHost(t)
	work, archive := stageHere(t)
	tail, err := installOnHost(context.Background(), "u@h", work, archive)
	if err != nil {
		t.Fatal(err)
	}
	// The test key reaches install.sh, which names it.
	if !strings.Contains(tail, "BACKSIGHT_INSTALL_PUBKEY is set: the release is checked against "+signing.TestKey) {
		t.Errorf("install.sh's stderr does not name the test key it trusts:\n%s", tail)
	}
	dest := filepath.Join(hostHome, ".local/share/backsight/bin/9.9.9")
	if b, err := os.ReadFile(filepath.Join(dest, "backsight")); err != nil || !bytes.Equal(b, fakeBody("9.9.9")) {
		t.Errorf("installed binary: %q, %v", b, err)
	}
	if got, err := os.Readlink(filepath.Join(hostHome, ".local/share/backsight/bin/current")); err != nil || got != filepath.Join(dest, "backsight") {
		t.Errorf("shim -> %q (%v), want %s", got, err, filepath.Join(dest, "backsight"))
	}
	for _, f := range []string{"checksums.txt", "checksums.txt.minisig"} {
		want, _ := os.ReadFile(filepath.Join(work, f))
		if got, err := os.ReadFile(filepath.Join(dest, f)); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s beside the binary: %v", f, err)
		}
	}
	if ents, _ := os.ReadDir(hostTmp); len(ents) != 0 {
		t.Errorf("the host's TMPDIR keeps %d entries", len(ents))
	}
}

// What reaches the host: this version's install.sh and the three release
// files on stdin; the test key as the only argument, in a build that
// trusts one.
func TestInstallOnHostSendsThisVersionsInstallSh(t *testing.T) {
	work, archive := stageHere(t)
	old := signing.TestKey
	t.Cleanup(func() { signing.TestKey = old })
	for _, key := range []string{signing.TestKey, ""} {
		signing.TestKey = key
		f := &fakeSSH{}
		withSSH(t, f)
		if _, err := installOnHost(context.Background(), "u@h", work, archive); err != nil {
			t.Fatal(err)
		}
		remote := f.calls[0][len(f.calls[0])-1]
		if want := "sh -c " + shellQuote(installScript) + " backsight-install"; key != "" && remote != want+" "+key || key == "" && remote != want {
			t.Errorf("key %q: remote %q", key, remote)
		}
		got := map[string][]byte{}
		tr := tar.NewReader(strings.NewReader(f.stdins[0]))
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(tr)
			got[h.Name] = b
		}
		want := map[string][]byte{"install.sh": scripts.Install}
		for _, n := range []string{archive, "checksums.txt", "checksums.txt.minisig"} {
			want["release/"+n], _ = os.ReadFile(filepath.Join(work, n))
		}
		for n, b := range want {
			if !bytes.Equal(got[n], b) || len(b) == 0 {
				t.Errorf("key %q: %s on stdin differs (%d bytes, want %d)", key, n, len(got[n]), len(b))
			}
		}
	}
}

// The host checks the release again with the key its install.sh carries:
// a release tampered past this PC's check, or signed by a key it does not
// trust, installs nothing there.
func TestHostRefusesATamperedRelease(t *testing.T) {
	_, other, _ := minisign.GenerateKey(rand.Reader)
	refusal := "bad signature" // minisign's verdict; install.sh's openssl path names the key
	if _, err := exec.LookPath("minisign"); err != nil {
		refusal = "signed by another key"
	}
	archive := "backsight_9.9.9_linux_" + runtime.GOARCH + ".tar.gz"
	for name, c := range map[string]struct {
		o        relOpts
		drop     string // left out of work
		truncate bool   // the host gets the tar cut inside the archive
		want     string
		local    bool // refused here, before ssh
	}{
		"tampered archive":  {o: relOpts{tamperArchive: true}, want: "checksum mismatch"},
		"another key":       {o: relOpts{key: &other}, want: refusal},
		"missing signature": {drop: "checksums.txt.minisig", want: "checksums.txt.minisig", local: true},
		"truncated stream":  {truncate: true, want: "(tar: "},
	} {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			c.o.platform = "linux_" + runtime.GOARCH
			signedRelease(t, src, "9.9.9", trustTestKey(t), c.o)
			work := t.TempDir()
			for _, n := range []string{archive, "checksums.txt", "checksums.txt.minisig"} {
				b, err := os.ReadFile(filepath.Join(src, "v9.9.9", n))
				if err != nil {
					t.Fatal(err)
				}
				if n == c.drop {
					continue
				}
				if err := os.WriteFile(filepath.Join(work, n), b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cut := 0
			if c.truncate { // into the archive's last block, before tar's end blocks
				probe := filepath.Join(t.TempDir(), "u.tar")
				if err := writeUpload(probe, work, archive); err != nil {
					t.Fatal(err)
				}
				fi, _ := os.Stat(probe)
				cut = int(fi.Size()) - 1024 - 256
			}
			hostHome, hostTmp, sshDir := loopback(t, cut)
			_, err := installOnHost(context.Background(), "u@h", work, archive)
			if output.CodeOf(err) != output.Blocked || !strings.Contains(err.Error(), c.want) ||
				!c.local && !strings.Contains(err.Error(), "rerunning this is safe") {
				t.Errorf("err = %v (exit %d), want exit 20 quoting %q", err, output.CodeOf(err), c.want)
			}
			if _, err := os.Stat(filepath.Join(sshDir, "reached")); c.local == (err == nil) {
				t.Errorf("ssh ran: %v, want %v", err == nil, !c.local)
			}
			if _, err := os.Lstat(filepath.Join(hostHome, ".local/share/backsight/bin/current")); err == nil {
				t.Error("a shim was switched on the host")
			}
			if ents, _ := os.ReadDir(hostTmp); len(ents) != 0 {
				t.Errorf("the host's TMPDIR keeps %d entries", len(ents))
			}
		})
	}
}

// A remote script reaches sh through the user's login shell, quoted: no
// quote or backslash (fish, csh read them otherwise), no newline (csh
// refuses one inside quotes), no '!' (csh's history), and valid sh.
func TestRemoteScriptsSurviveAnyLoginShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	for name, s := range map[string]string{"probeScript": probeScript, "installScript": installScript} {
		if strings.ContainsAny(s, "'\\\n\r!") {
			t.Errorf("%s holds a quote, a backslash, a newline or '!': %q", name, s)
		}
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("sh -n %s: %v\n%s", name, err, out)
		}
	}
}
