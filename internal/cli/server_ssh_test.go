package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"aead.dev/minisign"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/testtool"
	"github.com/Barben360/backsight/internal/token"
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

// The host's own environment never picks the key its install.sh trusts:
// a release signed by the key its BACKSIGHT_INSTALL_PUBKEY names installs
// nothing there.
func TestHostEnvironmentCannotChooseTheKey(t *testing.T) {
	pub, attacker, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := signing.TestKey
	signing.TestKey = "" // a release build: no key argument
	t.Cleanup(func() { signing.TestKey = old })
	src := t.TempDir()
	signedRelease(t, src, "9.9.9", attacker, relOpts{platform: "linux_" + runtime.GOARCH})
	archive := "backsight_9.9.9_linux_" + runtime.GOARCH + ".tar.gz"
	work := t.TempDir()
	for _, n := range []string{archive, "checksums.txt", "checksums.txt.minisig"} {
		b, err := os.ReadFile(filepath.Join(src, "v9.9.9", n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hostHome, _, _ := loopback(t, 0, "BACKSIGHT_INSTALL_PUBKEY="+pub.String(), "BACKSIGHT_RELEASE_URL=https://example.invalid/r")
	tail, err := installOnHost(context.Background(), "u@h", work, archive)
	if output.CodeOf(err) != output.Blocked || strings.Contains(tail, "BACKSIGHT_INSTALL_PUBKEY is set") {
		t.Errorf("err = %v (exit %d), want exit 20 from the release key\n%s", err, output.CodeOf(err), tail)
	}
	if _, err := os.Lstat(filepath.Join(hostHome, ".local/share/backsight/bin/current")); err == nil {
		t.Error("a release signed by the key the host's environment named was installed")
	}
}

// A remote script reaches sh through the user's login shell, quoted: no
// quote or backslash (fish, csh read them otherwise), no newline (csh
// refuses one inside quotes), no '!' (csh's history), and valid sh.
func TestRemoteScriptsSurviveAnyLoginShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	for name, s := range map[string]string{"probeScript": probeScript, "installScript": installScript, "handbackScript": handbackScript} {
		if strings.ContainsAny(s, "'\\\n\r!") {
			t.Errorf("%s holds a quote, a backslash, a newline or '!': %q", name, s)
		}
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("sh -n %s: %v\n%s", name, err, out)
		}
	}
}

// The host the ssh install tests reach, as its backsight would answer.
const (
	fakeShim  = "/home/u/.local/share/backsight/bin/current"
	fakeState = "/home/u/.local/state/backsight"
	llmKey    = "sk-llm-0123456789abcdefghij"
	llmKeyEnv = "BACKSIGHT_TEST_LLM_KEY"
)

// fakeHost is an sshCLI standing for the host: it answers the probe, the
// install and the token's handback by their step name, and the words after
// the shim with the envelope backsight there would print (after a line of
// profile noise). It records each call's argv, words and stdin.
type fakeHost struct {
	t        *testing.T
	addr     string // the host's end of the ssh connection
	linger   string
	version  string // backsight there; "" none
	config   bool
	role     string
	mode     string
	keySet   bool
	tok      token.Token
	setupErr *output.Error     // what setup answers, once
	drop     string            // the step whose connection drops (ssh exits 255)
	dropErr  string            // ssh's stderr then; default: a closed connection
	probe    map[string]string // the probe's answers instead of a supported Linux host's (os, machine, verifier, missing)
	progress string            // the detail of setup's progress line
	progStep string            // the step of setup's progress line; default: server
	exit     map[string]int    // a step that exits with this code instead, after its answer
	stderr   map[string]string // what a step in exit writes on stderr
	calls    []hostCall
}

type hostCall struct {
	step  string   // probe | install | handback | version | paths | secret list | secret set | setup | expose | token
	argv  []string // ssh's
	words []string // backsight's, after the shim
	stdin string
}

func (h *fakeHost) Run(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	remote := args[len(args)-1]
	c := hostCall{argv: args}
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		c.stdin = string(b)
	}
	reply := func(data any, err *output.Error) int {
		_, _ = io.WriteString(stdout, "motd: welcome to the host\n")
		_ = output.WriteJSON(stdout, "host", data, err)
		if err != nil {
			return err.Code
		}
		return 0
	}
	code := 0
	switch {
	case strings.Contains(remote, "backsight-probe"):
		c.step = "probe"
		has := map[bool]string{true: "yes", false: "no"}[h.version != ""]
		facts := map[string]string{"os": "Linux", "machine": "x86_64", "verifier": "minisign", "missing": ""}
		for k, v := range h.probe {
			facts[k] = v
		}
		fmt.Fprintf(stdout, "os=%s\nmachine=%s\nconn=100.101.1.9 50022 %s 22\nshim=%s\nhas_shim=%s\nverifier=%s\nmissing=%s\nlinger=%s\n",
			facts["os"], facts["machine"], h.addr, fakeShim, has, facts["verifier"], facts["missing"], h.linger)
	case strings.Contains(remote, "backsight-install"):
		c.step = "install"
		h.version = "9.9.9"
	case strings.Contains(remote, "backsight-handback"):
		c.step = "handback"
		if !strings.HasSuffix(remote, " "+fakeState+"/ssh-handback-token") {
			h.t.Errorf("handback reads %q, want the file server token wrote", remote)
		}
		fmt.Fprintf(stdout, "motd: welcome\nBACKSIGHT-TOKEN %s\n", token.Encode(h.tok))
	case strings.HasPrefix(remote, fakeShim+" "):
		c.words = strings.Fields(strings.TrimPrefix(remote, fakeShim+" "))
		c.step = c.words[0]
		if c.step == "secret" || c.step == "server" {
			c.step = map[string]string{"secret": "secret ", "server": ""}[c.step] + c.words[1]
		}
		if h.version == "" {
			_, _ = io.WriteString(stderr, "sh: 1: "+fakeShim+": not found\n")
			code = 127
			break
		}
		switch c.step {
		case "version":
			code = reply(map[string]string{"version": h.version}, nil)
		case "paths":
			code = reply(map[string]any{"state": fakeState, "config_exists": h.config, "server_role": h.role, "server_mode": h.mode}, nil)
		case "secret list":
			code = reply([]secretState{{Name: "api_key", Set: h.config}, {Name: "llm_api_key", Set: h.keySet, Required: true}}, nil)
		case "secret set":
			h.keySet = true
			code = reply(map[string]string{"name": "llm_api_key"}, nil)
		case "setup":
			h.config, h.role, h.mode = true, "server", "local"
			st := h.progStep
			if st == "" {
				st = "server"
			}
			line, _ := json.Marshal(map[string]any{"event": "progress", "step": st, "pct": 40, "eta_s": 30, "detail": h.progress})
			_, _ = fmt.Fprintln(stderr, string(line))
			err := h.setupErr
			h.setupErr = nil
			code = reply(map[string]any{"steps": []any{}}, err)
		case "expose":
			code = reply(map[string]any{"exposure": "private"}, nil)
		case "token":
			if i := slices.Index(c.words, "--out"); i < 0 || c.words[i+1] != fakeState+"/ssh-handback-token" {
				h.t.Errorf("server token words %q, want --out in the host's state dir", c.words)
			}
			code = reply(map[string]string{"url": h.tok.URL, "written": fakeState + "/ssh-handback-token"}, nil)
		default:
			h.t.Errorf("unexpected backsight words on the host: %q", c.words)
			code = 2
		}
	default:
		h.t.Errorf("unexpected remote command %q", remote)
		code = 1
	}
	if x, ok := h.exit[c.step]; ok {
		_, _ = io.WriteString(stderr, h.stderr[c.step])
		code = x
	}
	h.calls = append(h.calls, c)
	if c.step == h.drop {
		msg := h.dropErr
		if msg == "" {
			msg = "Connection to h closed by remote host.\n"
		}
		_, _ = io.WriteString(stderr, msg)
		return 255, nil
	}
	return code, nil
}

func (h *fakeHost) steps() []string {
	var out []string
	for _, c := range h.calls {
		out = append(out, c.step)
	}
	return out
}

func (h *fakeHost) call(step string) *hostCall {
	for i := range h.calls {
		if h.calls[i].step == step {
			return &h.calls[i]
		}
	}
	return nil
}

// sshMachine is this PC (setup's steps faked) on backsight 9.9.9 with a
// signed release of it for linux/amd64 in src, and a new host at addr
// handing out a token of a fake Hindsight; stdin is empty.
func sshMachine(t *testing.T, addr string) (home, src string, h *fakeHost) {
	t.Helper()
	home, _ = setupMachine(t)
	setBuild(t, "9.9.9", "abc1234")
	src = t.TempDir()
	signedRelease(t, src, "9.9.9", trustTestKey(t), relOpts{platform: "linux_amd64"})
	h = &fakeHost{t: t, addr: addr, linger: "yes", tok: joinableHost(t)}
	withSSH(t, h)
	withStdin(t, "")
	t.Setenv(llmKeyEnv, llmKey)
	return home, src, h
}

func sshInstallArgs(src string, extra ...string) []string {
	return append([]string{"server", "install", "--ssh", "u@h", "--source", src, "--llm-key-from-env", llmKeyEnv,
		"--answer", "consent=yes", "--non-interactive"}, extra...)
}

type sshInstallEnvelope struct {
	OK    bool             `json:"ok"`
	Data  sshInstallResult `json:"data"`
	Error *output.Error    `json:"error"`
}

func parseSSHInstall(t *testing.T, out string) sshInstallEnvelope {
	t.Helper()
	var env sshInstallEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not one envelope: %v\n%s", err, out)
	}
	return env
}

// A new host: release, key, setup, expose and token there, then this PC
// joins it; the host's progress shows here.
func TestSSHInstallNewHost(t *testing.T) {
	home, src, h := sshMachine(t, "100.101.1.2")
	h.linger = "no"
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	env := parseSSHInstall(t, out)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if want := []string{"probe", "install", "paths", "secret set", "setup", "expose", "token", "handback"}; !slices.Equal(h.steps(), want) {
		t.Errorf("calls %q, want %q", h.steps(), want)
	}
	if s := strings.Join(h.call("setup").words, " "); !strings.Contains(s, "setup --role server --non-interactive --json --answer consent=yes --answer provider=deepseek") {
		t.Errorf("setup on the host: %s", s)
	}
	if s := strings.Join(h.call("expose").words, " "); !strings.Contains(s, "--private 100.101.1.2") || strings.Contains(s, "cleartext_lan") {
		t.Errorf("expose on the host: %s", s)
	}
	d := pathsOf(home)
	if cfg, err := config.Load(d.ConfigFile(), d, home); err != nil || cfg.Server.Mode != "remote" || cfg.Server.URL != h.tok.URL {
		t.Errorf("this PC's config: %+v %v", cfg.Server, err)
	}
	if sec, err := config.LoadSecrets(d.SecretsFile()); err != nil || sec.APIKey != hostKey {
		t.Errorf("this PC's secrets: %v", err)
	}
	if st, _ := state.Load(d.StateFile()); st.RemoteServer == nil || st.RemoteServer.ID != h.tok.ServerID {
		t.Errorf("binding: %+v", st.RemoteServer)
	}
	for _, s := range env.Data.Steps {
		if s.Status != "done" {
			t.Errorf("step %s is %s", s.Name, s.Status)
		}
	}
	if len(env.Data.Steps) != 8 || env.Data.Address != "100.101.1.2" || env.Data.Connect == nil || env.Data.Connect.Situation != joinNew {
		t.Errorf("result: %+v", env.Data)
	}
	if !strings.Contains(strings.Join(env.Data.Warnings, "\n"), "sudo loginctl enable-linger u") {
		t.Errorf("warnings: %q", env.Data.Warnings)
	}
	if !strings.Contains(errOut, `"step":"host:server"`) {
		t.Errorf("no host progress relayed on stderr:\n%s", errOut)
	}
}

// RF 2: the LLM key travels only on the stdin of secret set on the host,
// and neither it, the host's API key nor the token reaches a command line
// or this PC's output, also when connect refuses the token.
func TestSSHInstallNeverPutsASecretOnACommandLineOrOutput(t *testing.T) {
	check := func(t *testing.T, h *fakeHost, out string) {
		t.Helper()
		enc := token.Encode(h.tok)
		for _, c := range h.calls {
			argv := strings.Join(c.argv, " ")
			if strings.Contains(argv, llmKey) || strings.Contains(argv, h.tok.APIKey) || strings.Contains(argv, enc) {
				t.Errorf("%s: a secret on the command line: %s", c.step, argv)
			}
			switch c.step {
			case "secret set":
				if c.stdin != llmKey+"\n" {
					t.Errorf("secret set's stdin %q", c.stdin)
				}
			case "install":
				if c.stdin == "" {
					t.Error("install got no tar")
				}
			default:
				if c.stdin != "" {
					t.Errorf("%s got stdin %q", c.step, c.stdin)
				}
			}
		}
		if leaked(out, h.tok) || strings.Contains(out, llmKey) || strings.Contains(out, enc) {
			t.Errorf("a secret in the output:\n%s", out)
		}
	}
	for _, mode := range [][]string{{"--json"}, nil} {
		name := map[bool]string{true: "json", false: "text"}[len(mode) > 0]
		t.Run(name, func(t *testing.T) {
			_, src, h := sshMachine(t, "100.101.1.2")
			code, out, errOut := run(t, sshInstallArgs(src, mode...)...)
			if code != output.OK {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			check(t, h, out+errOut)
		})
	}
	t.Run("refused token", func(t *testing.T) {
		_, src, h := sshMachine(t, "100.101.1.2")
		h.tok.ServerID = "not-the-servers-id"
		code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
		env := parseSSHInstall(t, out)
		if code != output.Blocked || env.Error == nil {
			t.Fatalf("exit %d, want 20\n%s%s", code, out, errOut)
		}
		check(t, h, out+errOut)
		if r := env.Error.Resume; !strings.HasPrefix(r, "backsight server install --ssh u@h ") || !strings.Contains(r, "--llm-key-from-env "+llmKeyEnv) || strings.Contains(r, llmKey) {
			t.Errorf("resume %q", r)
		}
	})
}

// RF 1: a host key this PC does not know is refused, never accepted, and
// nothing changes here.
func TestSSHUnknownHostKeyIsRefusedNeverAccepted(t *testing.T) {
	home, src, _ := sshMachine(t, "100.101.1.2")
	f := &fakeSSH{code: 255, stderr: "Host key verification failed.\n"}
	withSSH(t, f)
	before := treeOf(t, home)
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	env := parseSSHInstall(t, out)
	if code != output.NeedsUser || env.Error == nil || len(env.Error.Actions) == 0 || env.Error.Actions[0].Cmd != "ssh u@h" {
		t.Fatalf("exit %d, want 10 with ssh u@h to run\n%s%s", code, out, errOut)
	}
	if len(f.calls) != 1 {
		t.Fatalf("ssh calls: %d", len(f.calls))
	}
	if argv := strings.Join(f.calls[0], " "); !strings.Contains(argv, "StrictHostKeyChecking=yes") || !strings.Contains(argv, "UpdateHostKeys=no") {
		t.Errorf("argv %s", argv)
	}
	sameTree(t, before, treeOf(t, home))
}

// RF 3: the server opens on the address this PC's ssh reached: a tailnet
// one as is, a LAN one with the cleartext answer, none other PCs cannot
// use; --private picks another.
func TestSSHInstallPicksTheAddressItConnectedTo(t *testing.T) {
	for _, c := range []struct {
		addr   string
		extra  []string
		code   int
		expose string // in expose's words, when it ran
		want   string // in the error
	}{
		{addr: "100.101.1.2", expose: "--private 100.101.1.2"},
		{addr: "192.168.1.20", code: output.NeedsUser, want: "cleartext_lan"},
		{addr: "192.168.1.20", extra: []string{"--answer", "cleartext_lan=yes"}, expose: "--private 192.168.1.20 --json --answer cleartext_lan=yes"},
		{addr: "127.0.0.1", code: output.NeedsUser, want: "--private"},
		{addr: "203.0.113.7", code: output.NeedsUser, want: "--private"},
		{addr: "192.168.1.20", extra: []string{"--private", "100.64.0.9"}, expose: "--private 100.64.0.9"},
	} {
		t.Run(c.addr+strings.Join(c.extra, ""), func(t *testing.T) {
			_, src, h := sshMachine(t, c.addr)
			code, out, errOut := run(t, sshInstallArgs(src, append(c.extra, "--json")...)...)
			env := parseSSHInstall(t, out)
			if code != c.code {
				t.Fatalf("exit %d, want %d\n%s%s", code, c.code, out, errOut)
			}
			if c.code != output.OK {
				msg, _ := json.Marshal(env.Error)
				if !strings.Contains(string(msg), c.want) || h.call("install") != nil || len(h.calls) != 1 {
					t.Errorf("error %s, calls %q: want %q named and only the probe", msg, h.steps(), c.want)
				}
				return
			}
			if e := h.call("expose"); e == nil || !strings.Contains(strings.Join(e.words, " "), c.expose) {
				t.Errorf("expose: %+v, want %q", e, c.expose)
			}
		})
	}
	t.Run("unusable address resume", func(t *testing.T) {
		_, src, _ := sshMachine(t, "127.0.0.1")
		_, out, _ := run(t, sshInstallArgs(src, "--json")...)
		if env := parseSSHInstall(t, out); env.Error == nil || !strings.HasSuffix(env.Error.Resume, " --private <ip>") || !strings.Contains(env.Error.Message, "u@h at 127.0.0.1") {
			t.Errorf("error %+v", env.Error)
		}
	})
}

// RF 4: an install over ssh never swaps the backsight of another version
// nor turns a PC with Claude Code or a client into a server-only host.
func TestSSHInstallRefusesAHostItWouldBreak(t *testing.T) {
	for _, c := range []struct {
		name                string
		version, role, mode string
		config              bool
		want                []string
	}{
		{"older backsight", "9.9.8", "", "", false, []string{"9.9.8", "update apply"}},
		{"a PC with Claude Code", "9.9.9", "both", "local", true, []string{"server expose --private", "connect --token-file"}},
		{"a client", "9.9.9", "both", "remote", true, []string{"client of another memory server"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, src, h := sshMachine(t, "100.101.1.2")
			h.version, h.role, h.mode, h.config = c.version, c.role, c.mode, c.config
			code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
			env := parseSSHInstall(t, out)
			if code != output.Blocked || env.Error == nil {
				t.Fatalf("exit %d, want 20\n%s%s", code, out, errOut)
			}
			for _, w := range c.want {
				if !strings.Contains(env.Error.Message, w) {
					t.Errorf("message %q lacks %q", env.Error.Message, w)
				}
			}
			for _, s := range h.steps() {
				if !slices.Contains([]string{"probe", "version", "paths", "secret list"}, s) {
					t.Errorf("a %s call on a host it refuses", s)
				}
			}
		})
	}
}

// RF 5: a run that stopped on the host resumes: the release and the key
// already there are not sent again, setup goes on.
func TestSSHInstallRerunAfterAFailedSetupResumes(t *testing.T) {
	testtool.SkipWindows(t, "Task 3b: resume commands quote a Windows path POSIX-style")
	_, src, h := sshMachine(t, "100.101.1.2")
	h.setupErr = &output.Error{Code: output.Blocked, Message: "Docker is not running"}
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	env := parseSSHInstall(t, out)
	want := "backsight server install --ssh u@h --source " + src + " --llm-key-from-env " + llmKeyEnv + " --answer consent=yes --non-interactive"
	if code != output.Blocked || env.Error == nil || !strings.HasPrefix(env.Error.Message, "on u@h: ") || env.Error.Resume != want {
		t.Fatalf("exit %d, want 20 from the host resuming with %q\n%s%s", code, want, out, errOut)
	}
	h.calls = nil
	args := slices.DeleteFunc(sshInstallArgs(src, "--json"), func(a string) bool { return a == "--llm-key-from-env" || a == llmKeyEnv })
	code, out, errOut = run(t, args...)
	if code != output.OK {
		t.Fatalf("rerun: exit %d\n%s%s", code, out, errOut)
	}
	if want := []string{"probe", "version", "paths", "secret list", "setup", "expose", "token", "handback"}; !slices.Equal(h.steps(), want) {
		t.Errorf("rerun calls %q, want %q", h.steps(), want)
	}
	env = parseSSHInstall(t, out)
	for _, s := range env.Data.Steps {
		if s.Name == "release" && s.Status != "skipped" || s.Name == "llm-key" && (s.Status != "skipped" || !strings.Contains(s.Detail, "already set")) {
			t.Errorf("rerun step %+v", s)
		}
	}
}

// --plan reads the host and changes nothing, here or there.
func TestSSHInstallPlanChangesNothing(t *testing.T) {
	home, src, h := sshMachine(t, "100.101.1.2")
	before := treeOf(t, home)
	code, out, errOut := run(t, "server", "install", "--ssh", "u@h", "--source", src, "--plan", "--json")
	env := parseSSHInstall(t, out)
	if code != output.OK || !env.Data.Plan {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !slices.Equal(h.steps(), []string{"probe"}) {
		t.Errorf("calls %q", h.steps())
	}
	for name, detail := range map[string]string{"release": "9.9.9 for linux/amd64", "llm-key": "", "setup": "", "expose": "100.101.1.2", "token": "never shown", "connect": "join it"} {
		if !hasConnectStep(env.Data.Steps, name, "planned", detail) {
			t.Errorf("no planned %s step with %q: %+v", name, detail, env.Data.Steps)
		}
	}
	sameTree(t, before, treeOf(t, home))

	// This PC with a server of its own: the plan says connect backs it up
	// and how to go back.
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	if err := os.WriteFile(d.ConfigFile(), []byte("schema_version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ = run(t, "server", "install", "--ssh", "u@h", "--source", src, "--plan", "--json")
	if env := parseSSHInstall(t, out); !hasConnectStep(env.Data.Steps, "connect", "planned", "back up") || !hasConnectStep(env.Data.Steps, "connect", "planned", "connect --undo") {
		t.Errorf("own server plan: %+v", env.Data.Steps)
	}
	if code, _, _ := run(t, "server", "install", "--ssh", "u@h", "--plan", "--llm-key-from-env", llmKeyEnv); code != output.Usage {
		t.Errorf("--plan with a key: exit %d, want 2", code)
	}
}

// The consent and the LLM key come before anything changes on the host.
func TestSSHInstallNeedsConsentAndKeyBeforeTouchingTheHost(t *testing.T) {
	testtool.SkipWindows(t, "Task 3b: resume commands quote a Windows path POSIX-style")
	_, src, h := sshMachine(t, "100.101.1.2")
	code, out, _ := run(t, "server", "install", "--ssh", "u@h", "--source", src, "--llm-key-from-env", llmKeyEnv, "--non-interactive", "--json")
	env := parseSSHInstall(t, out)
	if code != output.NeedsUser || env.Error == nil || !strings.HasSuffix(env.Error.Resume, " --answer consent=yes") || !slices.Equal(h.steps(), []string{"probe"}) {
		t.Fatalf("no consent: exit %d, calls %q\n%s", code, h.steps(), out)
	}
	// The plan it names runs as a plan.
	_, after, _ := strings.Cut(env.Error.Message, "read the plan (")
	planCmd, _, _ := strings.Cut(after, ")")
	words := strings.Fields(planCmd)
	if len(words) < 2 || words[0] != "backsight" || words[len(words)-1] != "--plan" {
		t.Fatalf("plan command %q", planCmd)
	}
	h.calls = nil
	if code, out, errOut := run(t, append(words[1:], "--json")...); code != output.OK || !parseSSHInstall(t, out).Data.Plan || !slices.Equal(h.steps(), []string{"probe"}) {
		t.Errorf("%s: exit %d, calls %q\n%s%s", planCmd, code, h.steps(), out, errOut)
	}

	h.calls = nil
	code, out, _ = run(t, "server", "install", "--ssh", "u@h", "--source", src, "--answer", "consent=yes", "--non-interactive", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "--llm-key-from-file") || h.call("install") != nil {
		t.Errorf("no key: exit %d, calls %q\n%s", code, h.steps(), out)
	}

	h.calls = nil
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	code, out, _ = run(t, "server", "install", "--ssh", "u@h", "--source", src, "--answer", "consent=yes", "--non-interactive", "--json")
	stdinIsTerminal = old
	if code != output.NeedsUser || !strings.Contains(out, "--llm-key-from-file") || h.call("install") != nil {
		t.Errorf("no key, --non-interactive at a terminal: exit %d, calls %q\n%s", code, h.steps(), out)
	}

	h.calls = nil
	code, out, errOut := run(t, "server", "install", "--ssh", "u@h", "--source", src, "--provider", "ollama", "--answer", "consent=yes", "--non-interactive", "--json")
	if code != output.OK || h.call("secret set") != nil || !strings.Contains(strings.Join(h.call("setup").words, " "), "--answer provider=ollama") {
		t.Errorf("keyless provider: exit %d, calls %q\n%s%s", code, h.steps(), out, errOut)
	}
}

// A server-only host holds no memory of its own to join from.
func TestSSHInstallFromAServerOnlyPCRefused(t *testing.T) {
	home, src, h := sshMachine(t, "100.101.1.2")
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	if err := os.WriteFile(d.ConfigFile(), []byte("schema_version = 1\n[server]\nrole = \"server\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := run(t, sshInstallArgs(src)...)
	if code != output.Usage || !strings.Contains(errOut, "server-only host") || len(h.calls) != 0 {
		t.Errorf("exit %d, calls %q: %s", code, h.steps(), errOut)
	}
}

// A client of another server installs a host too (plain server install
// still refuses there) and moves to it.
func TestSSHInstallFromAClientInstallsAnotherHost(t *testing.T) {
	home, src, _ := sshMachine(t, "100.101.1.2")
	asClientOf(t, home, "http://100.101.1.7:8888")
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	if env := parseSSHInstall(t, out); code != output.OK || env.Data.Connect == nil || env.Data.Connect.Situation != joinOther {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

// The flags of an install over ssh mean nothing without --ssh.
func TestNewSSHFlagsNeedSSH(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"--private", "100.64.0.9"}, {"--source", "/r"}, {"--llm-key-from-env", "K"}, {"--llm-key-from-file", "/k"},
		{"--answer", "consent=yes"}, {"--non-interactive"}, {"--config-dir", "/c"}} {
		if code, _, errOut := run(t, append([]string{"server", "install"}, args...)...); code != output.Usage || !strings.Contains(errOut, "--ssh") {
			t.Errorf("%v: exit %d: %s", args, code, errOut)
		}
	}
}

// The local checks refuse before any ssh call: a token or a secret as an
// answer, an unknown answer, flags and answers that disagree.
func TestSSHInstallRefusesBadInvocations(t *testing.T) {
	_, src, h := sshMachine(t, "100.101.1.2")
	for _, args := range [][]string{
		{"--answer", "llm_api_key=x"},
		{"--answer", "colour=blue"},
		{"--answer", "x=" + token.Prefix + "abc"},
		{"--provider", token.Prefix + "abc"},
		{"--model", token.Prefix + "abc"},
		{"--base-url", token.Prefix + "abc"},
		{"--provider", "anthropic", "--answer", "provider=deepseek"},
		{"--llm-key-from-file", "/k"},
		{"--answer", "cleartext_lan=maybe"},
	} {
		code, out, errOut := run(t, sshInstallArgs(src, args...)...)
		if code != output.Usage || strings.Contains(out+errOut, token.Prefix+"abc") {
			t.Errorf("%v: exit %d, want 2\n%s%s", args, code, out, errOut)
		}
	}
	if code, _, _ := run(t, "server", "install", "--ssh", "-oProxyCommand=x"); code != output.Usage {
		t.Errorf("an option as target: exit %d", code)
	}
	if len(h.calls) != 0 {
		t.Errorf("ssh ran: %q", h.steps())
	}
}

// The host's error comes back with its class, named for the host, its
// actions kept but a relogin (each ssh run is a new login).
func TestRunOnHostKeepsTheHostsClass(t *testing.T) {
	f := &fakeSSH{code: 10}
	withSSH(t, f)
	var buf strings.Builder
	_ = output.WriteJSON(&buf, "setup", nil, &output.Error{Code: output.NeedsUser, Message: "the docker group", Resume: "backsight setup --resume",
		Actions: []output.Action{{Kind: "run_as_user", Cmd: "sudo usermod -aG docker u", Why: "use Docker"}, {Kind: "relogin", Cmd: "", Why: "the group applies at login"}}})
	f.stdout = buf.String()
	_, err := runOnHost(context.Background(), &env{stdout: io.Discard, stderr: io.Discard}, "u@h", fakeShim, nil, "setup", "--json")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != output.NeedsUser || oe.Message != "on u@h: the docker group" || oe.Resume != "" ||
		len(oe.Actions) != 1 || oe.Actions[0].Why != "on u@h: use Docker" {
		t.Errorf("err %#v", err)
	}
	f.stdout, f.code, f.stderr = "", 255, "Connection closed by remote host\n"
	if _, err := runOnHost(context.Background(), &env{stdout: io.Discard, stderr: io.Discard}, "u@h", fakeShim, nil, "setup"); output.CodeOf(err) != output.Blocked ||
		!strings.Contains(err.Error(), "dropped") {
		t.Errorf("a dropped connection: %v", err)
	}
	f.stdout, f.code, f.stderr = "garbage\n", 1, "boom\n"
	if _, err := runOnHost(context.Background(), &env{stdout: io.Discard, stderr: io.Discard}, "u@h", fakeShim, nil, "setup"); output.CodeOf(err) != output.Blocked ||
		!strings.Contains(err.Error(), "boom") || !strings.HasPrefix(err.Error(), "on u@h: ") {
		t.Errorf("no envelope: %v", err)
	}
}

// The handback script prints the token file as one line and removes it,
// and prints nothing from a missing one.
func TestHandbackScriptReadsThenRemovesTheFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	file := filepath.Join(t.TempDir(), "ssh-handback-token")
	if err := os.WriteFile(file, []byte(token.Prefix+"abc_-=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", handbackScript, "backsight-handback", file).Output()
	if err != nil || string(out) != "BACKSIGHT-TOKEN "+token.Prefix+"abc_-=\n" {
		t.Errorf("out %q, %v", out, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the token file is still there: %v", err)
	}
	if out, err := exec.Command("sh", "-c", handbackScript, "backsight-handback", file).Output(); err == nil || len(out) != 0 {
		t.Errorf("missing file: out %q, err %v", out, err)
	}
}

// At a terminal the plan shows, then the question; each step shows as it
// is done; connect asks its own. A no changes nothing on the host.
func TestSSHInstallAsksAtATerminal(t *testing.T) {
	terminal := func(t *testing.T, in string) {
		withStdin(t, in)
		oldIn, oldOut := stdinIsTerminal, stdoutIsTerminal
		stdinIsTerminal, stdoutIsTerminal = func() bool { return true }, func() bool { return true }
		t.Cleanup(func() { stdinIsTerminal, stdoutIsTerminal = oldIn, oldOut })
	}
	_, src, h := sshMachine(t, "100.101.1.2")
	h.linger = "no"
	terminal(t, "n\n")
	args := []string{"server", "install", "--ssh", "u@h", "--source", src, "--llm-key-from-env", llmKeyEnv}
	code, out, errOut := run(t, args...)
	if code != output.OK || !strings.Contains(out, "planned  setup:") || !strings.Contains(out, "Install the memory server on u@h and join it from this PC? [y/N]") ||
		!strings.Contains(out, "nothing changed") || !slices.Equal(h.steps(), []string{"probe"}) {
		t.Fatalf("declined: exit %d, calls %q\n%s%s", code, h.steps(), out, errOut)
	}

	h.calls = nil
	terminal(t, "y\ny\n"+strings.Repeat("\n", 10)) // setup takes its defaults
	code, out, errOut = run(t, args...)
	if code != output.OK || strings.Count(out, "done     setup: backsight setup --role server") != 1 || !strings.Contains(out, "Join this server? [y/N]") ||
		!strings.Contains(out, "done     connect:") || h.call("handback") == nil || strings.Count(out, "enable-linger") != 1 {
		t.Errorf("accepted: exit %d, calls %q\n%s%s", code, h.steps(), out, errOut)
	}
}

// A connection that drops once the host started changing is a partial
// install (exit 40) that the same command resumes; this PC is untouched.
func TestSSHInstallDroppedConnectionIsPartial(t *testing.T) {
	for step, failed := range map[string]string{"install": "release", "expose": "expose"} {
		t.Run(step, func(t *testing.T) {
			home, src, h := sshMachine(t, "100.101.1.2")
			h.drop = step
			before := treeOf(t, home)
			code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
			env := parseSSHInstall(t, out)
			if code != output.Partial || env.Error == nil || !strings.Contains(env.Error.Message, "stopped part way") ||
				!strings.HasPrefix(env.Error.Resume, "backsight server install --ssh u@h ") || !hasConnectStep(env.Data.Steps, failed, "failed", "") {
				t.Fatalf("exit %d, want 40\n%s%s", code, out, errOut)
			}
			if h.call("handback") != nil {
				t.Errorf("went on after the drop: %q", h.steps())
			}
			sameTree(t, before, treeOf(t, home))
		})
	}
}

// Brief step 2: a host the release cannot go to is refused right after
// the probe, with nothing else run there.
func TestSSHInstallRefusesAHostItCannotInstall(t *testing.T) {
	for _, c := range []struct {
		name   string
		probe  map[string]string
		code   int
		want   []string
		action string
	}{
		{"not Linux", map[string]string{"os": "Darwin"}, output.Blocked, []string{"runs Darwin", "Linux host only", "backsight server install --print-commands"}, ""},
		{"no release for it", map[string]string{"machine": "riscv64"}, output.Blocked, []string{"riscv64", "no release", "backsight server install --print-commands"}, ""},
		{"no verifier", map[string]string{"verifier": ""}, output.NeedsUser, []string{"neither minisign nor OpenSSL 3"}, "on u@h: install minisign (or OpenSSL 3), which checks the release signature"},
		{"missing tools", map[string]string{"missing": " tar mktemp"}, output.Blocked, []string{"lacks tar, mktemp"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, src, h := sshMachine(t, "100.101.1.2")
			h.probe = c.probe
			before := treeOf(t, home)
			code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
			env := parseSSHInstall(t, out)
			if code != c.code || env.Error == nil {
				t.Fatalf("exit %d, want %d\n%s%s", code, c.code, out, errOut)
			}
			for _, w := range c.want {
				if !strings.Contains(env.Error.Message, w) {
					t.Errorf("message %q lacks %q", env.Error.Message, w)
				}
			}
			if c.action != "" && (len(env.Error.Actions) != 1 || env.Error.Actions[0].Kind != "install" || env.Error.Actions[0].Why != c.action) {
				t.Errorf("actions %+v, want the install of a verifier", env.Error.Actions)
			}
			if !slices.Equal(h.steps(), []string{"probe"}) {
				t.Errorf("calls %q, want the probe only", h.steps())
			}
			sameTree(t, before, treeOf(t, home))
		})
	}
}

// A rerun once all went through: the plan says this PC already joined,
// and connect finds the same server.
func TestSSHInstallRerunAfterASuccess(t *testing.T) {
	_, src, h := sshMachine(t, "100.101.1.2")
	if code, out, errOut := run(t, sshInstallArgs(src, "--json")...); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	h.calls = nil
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	env := parseSSHInstall(t, out)
	if code != output.OK || env.Data.Connect == nil || env.Data.Connect.Situation != joinSame {
		t.Fatalf("rerun: exit %d\n%s%s", code, out, errOut)
	}
	if want := []string{"probe", "version", "paths", "secret list", "secret set", "setup", "expose", "token", "handback"}; !slices.Equal(h.steps(), want) {
		t.Errorf("rerun calls %q, want %q", h.steps(), want)
	}
}

// The plan names a client of the host's own address as already joined.
func TestSSHInstallPlanSaysAlreadyJoined(t *testing.T) {
	home, src, _ := sshMachine(t, "100.101.1.2")
	asClientOf(t, home, "http://100.101.1.2:8888")
	_, out, _ := run(t, "server", "install", "--ssh", "u@h", "--source", src, "--plan", "--json")
	if env := parseSSHInstall(t, out); !hasConnectStep(env.Data.Steps, "connect", "planned", "already joined") {
		t.Errorf("steps %+v", env.Data.Steps)
	}
	asClientOf(t, home, "http://100.101.1.7:8888")
	_, out, _ = run(t, "server", "install", "--ssh", "u@h", "--source", src, "--plan", "--json")
	if env := parseSSHInstall(t, out); !hasConnectStep(env.Data.Steps, "connect", "planned", "leave the server at http://100.101.1.7:8888") {
		t.Errorf("another server: steps %+v", env.Data.Steps)
	}
}

// The host's progress lines are masked with the key this run sent, also
// where JSON escapes it (a key holding & or "), and in text mode, which
// prints the host's step names.
func TestSSHInstallRelaysNoSecret(t *testing.T) {
	const key = `sk-a&b"c<d>0123456789`
	esc, _ := json.Marshal(key)
	for _, mode := range [][]string{{"--json"}, nil} {
		_, src, h := sshMachine(t, "100.101.1.2")
		t.Setenv(llmKeyEnv, key)
		h.progress, h.progStep = "using "+key, "pull "+key
		code, out, errOut := run(t, sshInstallArgs(src, mode...)...)
		if code != output.OK || strings.Contains(out+errOut, key) || strings.Contains(out+errOut, strings.Trim(string(esc), `"`)) {
			t.Errorf("%v: exit %d\n%s%s", mode, code, out, errOut)
		}
		if h.call("secret set").stdin != key+"\n" {
			t.Errorf("%v: the key sent is %q", mode, h.call("secret set").stdin)
		}
		relayed := `"step":"host:pull [redacted]"`
		if len(mode) == 0 {
			relayed = "u@h: pull [redacted]\n"
		}
		if !strings.Contains(errOut, relayed) {
			t.Errorf("%v: no masked progress relayed (want %s):\n%s", mode, relayed, errOut)
		}
	}
}

// Every secret this run holds is masked in what comes back from the host:
// its error's message, actions and questions, and a failing step's last
// stderr line; so is the host token's key when connect fails.
func TestSSHInstallMasksTheHostsAnswers(t *testing.T) {
	const localKey = "this-pcs-api-key-0123456789"
	for name, c := range map[string]struct {
		fail func(h *fakeHost)
		want string // in the output: the failure was relayed
	}{
		"setup refuses": {func(h *fakeHost) {
			h.setupErr = &output.Error{Code: output.NeedsUser, Message: "Docker refused " + llmKey + " and " + localKey,
				Actions:   []output.Action{{Kind: "run_as_user", Cmd: "docker login -p " + llmKey, Why: "the registry wants " + localKey}},
				Questions: []setup.Question{{ID: "x", Prompt: "is " + llmKey + " right?", Options: []setup.Option{{Value: "yes", Label: "keep " + localKey}}}}}
		}, "Docker refused [redacted] and [redacted]"},
		"install refuses": {func(h *fakeHost) {
			h.exit, h.stderr = map[string]int{"install": 1}, map[string]string{"install": "install.sh: bad key " + llmKey + "\n"}
		}, "bad key [redacted]"},
	} {
		for _, mode := range [][]string{{"--json"}, nil} {
			t.Run(name+fmt.Sprint(mode), func(t *testing.T) {
				home, src, h := sshMachine(t, "100.101.1.2")
				d := pathsOf(home)
				if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("api_key", localKey) }); err != nil {
					t.Fatal(err)
				}
				c.fail(h)
				code, out, errOut := run(t, sshInstallArgs(src, mode...)...)
				if code == output.OK || !strings.Contains(out+errOut, c.want) {
					t.Fatalf("exit %d, want a failure saying %q\n%s%s", code, c.want, out, errOut)
				}
				for _, s := range []string{llmKey, localKey} {
					if strings.Contains(out+errOut, s) {
						t.Errorf("%s in the output:\n%s%s", s, out, errOut)
					}
				}
				if len(mode) > 0 && name == "setup refuses" {
					env := parseSSHInstall(t, out)
					if b, _ := json.Marshal(env.Error); env.Error == nil || len(env.Error.Actions) != 1 || !strings.Contains(string(b), "keep [redacted]") {
						t.Errorf("the host's actions and questions are not relayed masked: %s", b)
					}
				}
			})
		}
	}
	t.Run("connect fails", func(t *testing.T) {
		_, src, h := sshMachine(t, "100.101.1.2")
		old := connectFault
		connectFault = func(step string) error {
			if step == "secrets" {
				return &output.Error{Code: output.Blocked, Message: "injected " + hostKey,
					Actions: []output.Action{{Kind: "run_as_user", Cmd: "echo " + hostKey, Why: hostKey}}}
			}
			return nil
		}
		t.Cleanup(func() { connectFault = old })
		code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
		if code == output.OK || !strings.Contains(out, "injected [redacted]") || leaked(out+errOut, h.tok) {
			t.Errorf("exit %d, want connect's failure masked\n%s%s", code, out, errOut)
		}
	})
}

// The token never lands in a file on this PC, nor in its temp directory:
// only the API key it holds, in secrets.toml.
func TestSSHInstallLeavesNoTokenOnThisPC(t *testing.T) {
	home, src, h := sshMachine(t, "100.101.1.2")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if code, out, errOut := run(t, sshInstallArgs(src, "--json")...); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	enc := token.Encode(h.tok)
	secrets := pathsOf(home).SecretsFile()
	for _, root := range []string{home, tmp} {
		for p, v := range treeOf(t, root) {
			if v == "dir" || strings.HasPrefix(v, "-> ") {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte(enc)) || bytes.Contains(b, []byte(strings.TrimPrefix(enc, token.Prefix))) {
				t.Errorf("%s holds the token", p)
			}
			if bytes.Contains(b, []byte(h.tok.APIKey)) && p != secrets {
				t.Errorf("%s holds the token's API key", p)
			}
		}
	}
	if b, _ := os.ReadFile(secrets); !bytes.Contains(b, []byte(h.tok.APIKey)) {
		t.Error("secrets.toml lacks the API key: the scan looked at nothing")
	}
}

// An answer id with spaces around it still reaches its side.
func TestSSHInstallTrimsAnswerIDs(t *testing.T) {
	_, src, _ := sshMachine(t, "100.101.1.2")
	code, out, errOut := run(t, "server", "install", "--ssh", "u@h", "--source", src, "--llm-key-from-env", llmKeyEnv, "--answer", " consent =yes", "--non-interactive", "--json")
	if code != output.OK {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

// ssh's own class mid-run keeps its action on the partial install.
func TestSSHInstallPartialKeepsSSHAction(t *testing.T) {
	_, src, h := sshMachine(t, "100.101.1.2")
	h.drop, h.dropErr = "expose", "Host key verification failed.\n"
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	env := parseSSHInstall(t, out)
	if code != output.Partial || env.Error == nil || len(env.Error.Actions) == 0 || env.Error.Actions[0].Cmd != "ssh u@h" || !strings.HasPrefix(env.Error.Resume, "backsight server install --ssh u@h ") {
		t.Errorf("exit %d, want 40 with ssh u@h to run\n%s%s", code, out, errOut)
	}
}

// With --json, each change this PC makes streams a progress line as it
// starts and as it ends, the release's upload to the host included.
func TestSSHInstallStreamsItsSteps(t *testing.T) {
	_, src, _ := sshMachine(t, "100.101.1.2")
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var got []string
	for _, l := range strings.Split(errOut, "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(l), &ev) != nil || ev["event"] != "progress" || ev["command"] != "server install" {
			continue
		}
		got = append(got, fmt.Sprint(ev["step"], ":", ev["status"]))
	}
	want := []string{"release:running", "upload:running", "release:done", "llm-key:running", "llm-key:done", "setup:running", "setup:done",
		"expose:running", "expose:done", "token:running", "token:done", "connect:running", "connect:done"}
	if !slices.Equal(got, want) {
		t.Errorf("progress %q\nwant     %q", got, want)
	}
	if !strings.Contains(errOut, `"step":"host:server"`) {
		t.Errorf("the host's own progress is no longer relayed:\n%s", errOut)
	}
}

// A host extracting with a Claude subscription: its terms are accepted
// before anything changes there, in the plan, at a terminal, or as an
// answer the resume carries.
func TestSSHInstallAsksTheSubscriptionAckFirst(t *testing.T) {
	_, src, h := sshMachine(t, "100.101.1.2")
	args := sshInstallArgs(src, "--provider", "claude-code", "--json")
	code, out, errOut := run(t, args...)
	env := parseSSHInstall(t, out)
	if code != output.NeedsUser || env.Error == nil || !strings.HasSuffix(env.Error.Resume, " --answer consent=yes --non-interactive --answer ack_subscription=yes") ||
		!strings.Contains(fmt.Sprint(env.Error.Questions), "ack_subscription") || !slices.Equal(h.steps(), []string{"probe"}) {
		t.Fatalf("no ack: exit %d, calls %q\n%s%s", code, h.steps(), out, errOut)
	}

	h.calls = nil
	_, out, _ = run(t, "server", "install", "--ssh", "u@h", "--source", src, "--provider", "claude-code", "--plan", "--json")
	if env := parseSSHInstall(t, out); !hasConnectStep(env.Data.Steps, "ack_subscription", "planned", "Claude subscription") {
		t.Errorf("plan: %+v", env.Data.Steps)
	}

	h.calls = nil
	if code, out, errOut := run(t, append(args, "--answer", "ack_subscription=yes")...); code != output.OK ||
		!strings.Contains(strings.Join(h.call("setup").words, " "), "--answer ack_subscription=yes") {
		t.Errorf("acked: exit %d, calls %q\n%s%s", code, h.steps(), out, errOut)
	}

	terminal := func(t *testing.T, in string) {
		withStdin(t, in)
		oldIn, oldOut := stdinIsTerminal, stdoutIsTerminal
		stdinIsTerminal, stdoutIsTerminal = func() bool { return true }, func() bool { return true }
		t.Cleanup(func() { stdinIsTerminal, stdoutIsTerminal = oldIn, oldOut })
	}
	text := []string{"server", "install", "--ssh", "u@h", "--source", src, "--llm-key-from-env", llmKeyEnv, "--provider", "claude-code"}
	h.calls = nil
	terminal(t, "n\n")
	if code, out, errOut := run(t, text...); code != output.OK || !strings.Contains(out, "Claude subscription") || !strings.Contains(out, "nothing changed") ||
		slices.ContainsFunc(h.steps(), func(s string) bool { return !slices.Contains([]string{"probe", "version", "paths", "secret list"}, s) }) {
		t.Errorf("declined at a terminal: exit %d, calls %q, want reads only\n%s%s", code, h.steps(), out, errOut)
	}
	h.calls = nil
	terminal(t, "y\ny\n"+strings.Repeat("\n", 10))
	if code, out, errOut := run(t, text...); code != output.OK || !strings.Contains(strings.Join(h.call("setup").words, " "), "--answer ack_subscription=yes") {
		t.Errorf("accepted at a terminal: exit %d, calls %q\n%s%s", code, h.steps(), out, errOut)
	}
}

// An older backsight on the host is named before anything else is read
// there: its paths --json may not even answer.
func TestSSHInstallNamesAnOlderHostFirst(t *testing.T) {
	_, src, h := sshMachine(t, "100.101.1.2")
	h.version = "9.9.8"
	h.exit, h.stderr = map[string]int{"paths": 2}, map[string]string{"paths": "unknown flag: --json\n"}
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	env := parseSSHInstall(t, out)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "runs backsight 9.9.8 and this PC 9.9.9") {
		t.Fatalf("exit %d, want 20 naming both versions\n%s%s", code, out, errOut)
	}
	if !slices.Equal(h.steps(), []string{"probe", "version"}) {
		t.Errorf("calls %q, want the probe and version only", h.steps())
	}
}

// A value the host's login shell could read otherwise than sh is refused
// before any ssh call: never once the host started changing.
func TestSSHInstallRefusesAShellUnsafeValueFirst(t *testing.T) {
	_, src, h := sshMachine(t, "100.101.1.2")
	for _, args := range [][]string{
		{"--answer", "model=a!b"},
		{"--model", "a!b"},
		{"--answer", `model=a\b`},
		{"--base-url", "http://h/\x01"},
		{"--answer", "retention=a!b"},
		{"--private", "100.64.0.9!"},
	} {
		h.calls = nil
		code, out, errOut := run(t, sshInstallArgs(src, append(args, "--json")...)...)
		if code != output.Usage || len(h.calls) != 0 || !strings.Contains(out, "login shell") {
			t.Errorf("%q: exit %d, calls %q, want exit 2 before any ssh\n%s%s", args, code, h.steps(), out, errOut)
		}
	}
}

// A host question quoting a key JSON escapes (& ") comes back masked.
func TestSSHInstallMasksAnEscapedKeyInAHostQuestion(t *testing.T) {
	const key = `sk-q&r"s0123456789`
	esc, _ := json.Marshal(key)
	_, src, h := sshMachine(t, "100.101.1.2")
	t.Setenv(llmKeyEnv, key)
	h.setupErr = &output.Error{Code: output.NeedsUser, Message: "setup asks",
		Questions: []setup.Question{{ID: "x", Prompt: "is " + key + " right?"}}}
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	if code != output.NeedsUser || !strings.Contains(out, "is [redacted] right?") ||
		strings.Contains(out+errOut, key) || strings.Contains(out+errOut, strings.Trim(string(esc), `"`)) {
		t.Errorf("exit %d, want the question masked\n%s%s", code, out, errOut)
	}
}
