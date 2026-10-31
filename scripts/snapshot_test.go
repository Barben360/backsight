package scripts

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/testtool"
)

// Security review M3: a local snapshot is never signed with a release key,
// so no install or update accepts one; only the release workflow holds the
// release key.

// needMinisign skips without minisign, except on CI, where a skip would
// silently drop the proof that snapshots are signed with a throwaway key.
func needMinisign(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("minisign"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("minisign missing on CI: the snapshot key checks cannot be proved")
		}
		t.Skip("minisign missing")
	}
}

// keyFiles is a minisign key pair made by minisign itself, as make
// snapshot makes one: its secret and public key files.
type keyFiles struct{ sec, pub string }

func minisignKey(t *testing.T) keyFiles {
	t.Helper()
	needMinisign(t)
	d := t.TempDir()
	k := keyFiles{filepath.Join(d, "k.key"), filepath.Join(d, "k.pub")}
	if b, err := exec.Command("minisign", "-G", "-W", "-p", k.pub, "-s", k.sec).CombinedOutput(); err != nil {
		t.Fatalf("minisign -G: %v\n%s", err, b)
	}
	return k
}

// pubBytes is the decoded public key line of a minisign .pub file:
// "Ed", the key id (8 bytes), the Ed25519 public key (32 bytes).
func pubBytes(t *testing.T, pub string) []byte {
	t.Helper()
	raw, err := os.ReadFile(pub)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	b, err := base64.StdEncoding.DecodeString(lines[len(lines)-1])
	if err != nil || len(b) != 42 {
		t.Fatalf("%s: not a minisign public key (%v)", pub, err)
	}
	return b
}

// writePub writes a .pub file for this id and Ed25519 public key.
func writePub(t *testing.T, id, pk []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.pub")
	line := base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), id...), pk...))
	if err := os.WriteFile(p, []byte("untrusted comment: minisign public key\n"+line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const releaseRef = "Barben360/backsight/.github/workflows/release.yml@refs/tags/v0.1.1"

// guard runs scripts/release-key-guard.sh with only env (and PATH).
func guard(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	if _, err := os.Stat("release-key-guard.sh"); err != nil { // an input of the test cache key
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"release-key-guard.sh"}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func TestReleaseKeyGuardRefusesAReleaseKeyOutsideTheReleaseWorkflow(t *testing.T) {
	rel := minisignKey(t)   // stands in for minisign.pub's key
	other := minisignKey(t) // a throwaway key
	secret, err := os.ReadFile(rel.sec)
	if err != nil {
		t.Fatal(err)
	}
	pb := pubBytes(t, rel.pub)
	samePKOtherID := writePub(t, []byte("12345678"), pb[10:])
	encrypted := filepath.Join(t.TempDir(), "enc.key")
	{ // the same key, marked scrypt-encrypted: its key id cannot be read
		lines := strings.Split(string(secret), "\n")
		b, _ := base64.StdEncoding.DecodeString(lines[1])
		copy(b[2:4], "Sc")
		lines[1] = base64.StdEncoding.EncodeToString(b)
		_ = os.WriteFile(encrypted, []byte(strings.Join(lines, "\n")), 0o600)
	}
	garbage := filepath.Join(t.TempDir(), "garbage.key")
	_ = os.WriteFile(garbage, []byte("untrusted comment: x\nbm90IGEga2V5\n"), 0o600)

	inRelease := []string{"GITHUB_ACTIONS=true", "GITHUB_WORKFLOW_REF=" + releaseRef}
	inCI := []string{"GITHUB_ACTIONS=true", "GITHUB_WORKFLOW_REF=Barben360/backsight/.github/workflows/ci.yml@refs/heads/main"}
	cases := []struct {
		name string
		env  []string
		args []string
		ok   bool
		want string // in the output
	}{
		{"no key at all", nil, []string{"--snapshot", rel.pub}, true, ""},
		{"a throwaway key file", []string{"MINISIGN_KEY_FILE=" + other.sec}, []string{"--snapshot", rel.pub}, true, ""},
		{"a release key file, locally", []string{"MINISIGN_KEY_FILE=" + rel.sec}, []string{rel.pub}, false, "release key"},
		{"a release key file, for a snapshot", []string{"MINISIGN_KEY_FILE=" + rel.sec}, []string{"--snapshot", rel.pub}, false, "release key"},
		{"a release key in MINISIGN_SECRET_KEY", []string{"MINISIGN_SECRET_KEY=" + string(secret)}, []string{"--snapshot", rel.pub}, false, "MINISIGN_SECRET_KEY"},
		{"the release key's public half under another id", []string{"MINISIGN_KEY_FILE=" + rel.sec}, []string{samePKOtherID}, false, "release key"},
		{"a release key in another workflow", append([]string{"MINISIGN_KEY_FILE=" + rel.sec}, inCI...), []string{rel.pub}, false, "release key"},
		{"a release key in the release workflow", append([]string{"MINISIGN_KEY_FILE=" + rel.sec}, inRelease...), []string{rel.pub}, true, ""},
		{"a snapshot in the release workflow", append([]string{"MINISIGN_KEY_FILE=" + rel.sec}, inRelease...), []string{"--snapshot", rel.pub}, false, "release key"},
		{"a release workflow claimed without Actions", []string{"MINISIGN_KEY_FILE=" + rel.sec, "GITHUB_WORKFLOW_REF=" + releaseRef}, []string{rel.pub}, false, "release key"},
		{"an encrypted key, locally", []string{"MINISIGN_KEY_FILE=" + encrypted}, []string{"--snapshot", rel.pub}, false, "encrypted"},
		{"not a minisign key", []string{"MINISIGN_KEY_FILE=" + garbage}, []string{"--snapshot", rel.pub}, false, "not a minisign secret key"},
		{"a missing key file", []string{"MINISIGN_KEY_FILE=" + garbage + ".gone"}, []string{"--snapshot", rel.pub}, false, "cannot read"},
		// The defaults are the repository's minisign.pub and minisign.next.pub.
		{"a throwaway key against the published keys", []string{"MINISIGN_KEY_FILE=" + other.sec}, []string{"--snapshot"}, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := guard(t, c.env, c.args...)
			if c.ok != (err == nil) || !strings.Contains(out, c.want) {
				t.Errorf("exit %v (want ok %v), output %q (want %q)", err, c.ok, out, c.want)
			}
		})
	}
}

// Without public key arguments the guard compares with the published
// keys: --list prints the ids it read from minisign.pub and
// minisign.next.pub.
func TestReleaseKeyGuardKnowsThePublishedKeys(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	out, err := guard(t, nil, "--list")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, f := range []string{"../minisign.pub", "../minisign.next.pub"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		id := regexp.MustCompile(`minisign public key ([0-9A-F]{16})`).FindSubmatch(raw)
		if id == nil || !strings.Contains(out, string(id[1])) {
			t.Errorf("the guard does not list %s's key %q:\n%s", f, id, out)
		}
	}
}

// goreleaser runs the guard first, whoever calls it: a snapshot with a
// release key stops before anything is built or signed.
func TestGoreleaserRunsTheKeyGuardFirst(t *testing.T) {
	gr, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^before:\n  hooks:\n(?:    #.*\n)*    - scripts/release-key-guard\.sh \{\{ if \.IsSnapshot \}\}--snapshot\{\{ end \}\}\n`).Match(gr) {
		t.Error(".goreleaser.yaml does not run scripts/release-key-guard.sh (with --snapshot on a snapshot) as its first hook")
	}
}

// snapshotRun runs make snapshot with a fake goreleaser and a fake pass on
// PATH. The fake goreleaser signs a checksums.txt in DIST with the key it
// is given (so make's verify-dist checks it against the public key make
// says it made) and keeps a copy of that key; dist holds a real release
// build for verify-keys.
type snapshotResult struct {
	out     string
	err     error
	keyCopy string // a copy of the key goreleaser was given
	keyPath string // the path it was given
	passLog string // pass's calls, if any
}

func snapshotRun(t *testing.T, env ...string) snapshotResult {
	t.Helper()
	needTools(t)
	needMinisign(t)
	for _, f := range []string{"../Makefile", "release-key-guard.sh", "verify-keys.sh"} { // inputs of the test cache key
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
	bin, rec, dist := t.TempDir(), t.TempDir(), t.TempDir()
	buildInto(t, dist, "-s -w")
	keyCopy, keyPath, passLog := filepath.Join(rec, "key"), filepath.Join(rec, "key-path"), filepath.Join(rec, "pass.log")
	fake := map[string]string{
		"goreleaser": `#!/bin/sh
set -e
cp "$MINISIGN_KEY_FILE" "` + keyCopy + `"
printf '%s' "$MINISIGN_KEY_FILE" > "` + keyPath + `"
echo "fake goreleaser $*"
echo "0000  backsight_0.0.1_linux_amd64.tar.gz" > "$DIST/checksums.txt"
minisign -S -s "$MINISIGN_KEY_FILE" -m "$DIST/checksums.txt" -x "$DIST/checksums.txt.minisig" >/dev/null
`,
		"pass": `#!/bin/sh
echo "pass $*" >> "` + passLog + `"
echo "the release key, which make snapshot must never ask for"
`,
	}
	for name, body := range fake {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("make", "-s", "-C", "..", "snapshot")
	cmd.Env = append(append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "DIST="+dist,
		"MINISIGN_KEY_FILE=", "MINISIGN_SECRET_KEY="), env...)
	b, err := cmd.CombinedOutput()
	return snapshotResult{string(b), err, keyCopy, keyPath, passLog}
}

func TestSnapshotSignsWithAThrowawayKey(t *testing.T) {
	r := snapshotRun(t)
	if r.err != nil {
		t.Fatalf("make snapshot: %v\n%s", r.err, r.out)
	}
	if !strings.Contains(r.out, "fake goreleaser release --snapshot --clean") || !strings.Contains(r.out, "1 signatures verified") {
		t.Errorf("make snapshot did not build a snapshot and verify its signature:\n%s", r.out)
	}
	if b, err := os.ReadFile(r.passLog); err == nil {
		t.Errorf("make snapshot asked pass for a key: %s", b)
	}
	// The key it signed with: a minisign secret key whose public half is
	// neither published key.
	sec, err := os.ReadFile(r.keyCopy)
	if err != nil {
		t.Fatalf("goreleaser was not given a key: %v", err)
	}
	lines := strings.Split(string(sec), "\n")
	b, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(b) != 158 {
		t.Fatalf("not a minisign secret key: %q", sec)
	}
	id, pk := b[54:62], b[94:126]
	for _, f := range []string{"../minisign.pub", "../minisign.next.pub"} {
		p := pubBytes(t, f)
		if string(p[2:10]) == string(id) || string(p[10:]) == string(pk) {
			t.Errorf("make snapshot signed with %s's key", f)
		}
	}
	// A key of its own each time, gone once the build is done.
	if path, _ := os.ReadFile(r.keyPath); len(path) == 0 {
		t.Error("no key path recorded")
	} else if _, err := os.Stat(string(path)); !os.IsNotExist(err) {
		t.Errorf("the throwaway key %s is left on disk (%v)", path, err)
	}
	if second, _ := os.ReadFile(snapshotRun(t).keyCopy); string(second) == string(sec) {
		t.Error("two snapshots signed with the same key")
	}
}

// A release key handed to make snapshot is refused, not silently set
// aside: whoever exported it is told, and nothing is built.
func TestSnapshotRefusesAReleaseKey(t *testing.T) {
	rel := minisignKey(t)
	secret, err := os.ReadFile(rel.sec)
	if err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]string{
		"MINISIGN_KEY_FILE":   "MINISIGN_KEY_FILE=" + rel.sec,
		"MINISIGN_SECRET_KEY": "MINISIGN_SECRET_KEY=" + string(secret),
	} {
		t.Run(name, func(t *testing.T) {
			r := snapshotRun(t, env, "RELEASE_PUBKEYS="+rel.pub)
			if r.err == nil || !strings.Contains(r.out, "release key") {
				t.Errorf("make snapshot with a release key in %s: %v\n%s", name, r.err, r.out)
			}
			if _, err := os.Stat(r.keyCopy); !os.IsNotExist(err) {
				t.Errorf("goreleaser ran: %v\n%s", err, r.out)
			}
		})
	}
}
