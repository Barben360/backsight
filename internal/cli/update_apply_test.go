package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/migrate"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/selfcheck"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/supervise"
	"github.com/Barben360/backsight/internal/testtool"
)

// fakeBody is a fake backsight binary of version v; fakeBinaries runs it
// in process as that version.
func fakeBody(v string) []byte { return []byte("#!/bin/sh\n# fake backsight " + v + "\n") }

var fakeVersion = regexp.MustCompile(`# fake backsight (\S+)`)

// fakeBinaries stands in for exec'ing a release binary: it runs the CLI in
// this process with buildinfo.Version set to the binary's version, selfExe
// set to it and that version's migrations.
type fakeBinaries struct {
	t          *testing.T
	calls      []string
	migrations map[string][]migrate.Migration
	fail       map[string]int // "<version> <args>" prefix → exit code, without running
	// lockHeld: whether the maintenance lock was held by update apply at
	// each call.
	lockHeld []bool
	dirs     paths.Dirs
	// claimVersion makes a binary report another version than its own.
	claimVersion map[string]string
	// images: the Hindsight image each version pins (default: this one's).
	images map[string]string
	// onFail runs when a fail prefix matches, before the failure returns.
	onFail func(line string)
	// failStderr is what a failed call prints on stderr (default
	// "progress\n").
	failStderr string
	// indexes: the metrics index migrations each version embeds (default:
	// this test binary's).
	indexes map[string]fs.FS
	// before and after run around each call that is not failed (ctx is
	// the call's).
	before func(ctx context.Context, line string)
	after  func(line string, code int)
}

func withBinaries(t *testing.T, d paths.Dirs) *fakeBinaries {
	t.Helper()
	f := &fakeBinaries{t: t, migrations: map[string][]migrate.Migration{}, fail: map[string]int{}, indexes: map[string]fs.FS{}, dirs: d, claimVersion: map[string]string{}, images: map[string]string{}}
	old := binaryExec
	binaryExec = f
	t.Cleanup(func() { binaryExec = old })
	return f
}

func (f *fakeBinaries) Run(ctx context.Context, exe string, args ...string) ([]byte, []byte, int, error) {
	if d, ok := ctx.Deadline(); ok && time.Until(d) <= time.Millisecond {
		// A real exec takes longer: the deadline is reached before the
		// child does anything (Windows' clock may not even have moved yet).
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil { // exec does not start a command on a done context
		return nil, nil, 0, err
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		return nil, nil, 0, err
	}
	m := fakeVersion.FindSubmatch(raw)
	if m == nil {
		return nil, nil, 0, errors.New("exec format error: " + exe)
	}
	v := string(m[1])
	line := v + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	h := state.Inspect(f.dirs.MaintenanceLock())
	f.lockHeld = append(f.lockHeld, h != nil && !h.Stale && h.Owner.Command == "update apply")
	for prefix, code := range f.fail {
		if strings.HasPrefix(line, prefix) {
			if f.onFail != nil {
				f.onFail(line)
			}
			var out bytes.Buffer
			e := &output.Error{Code: code, Message: "failing on purpose: " + prefix}
			if code == output.NeedsUser {
				e.Actions = []output.Action{{Kind: "install", Cmd: "npm install -g @anthropic-ai/claude-code", Why: "Claude Code is too old"}}
			}
			_ = output.WriteJSON(&out, "test", nil, e)
			stderr := "progress\n"
			if f.failStderr != "" {
				stderr = f.failStderr
			}
			return out.Bytes(), []byte(stderr), code, nil
		}
	}
	oldV, oldExe, oldMig, oldImg, oldReg := buildinfo.Version, selfExe, migrations, server.HindsightImage, indexRegistry
	if img, ok := f.images[v]; ok {
		server.HindsightImage = img
	}
	if reg, ok := f.indexes[v]; ok {
		indexRegistry = func() fs.FS { return reg }
	}
	buildinfo.Version = v
	if c, ok := f.claimVersion[v]; ok {
		buildinfo.Version = c
	}
	selfExe = func() (string, error) { return exe, nil }
	migrations = func() []migrate.Migration { return f.migrations[v] }
	defer func() {
		buildinfo.Version, selfExe, migrations, server.HindsightImage, indexRegistry = oldV, oldExe, oldMig, oldImg, oldReg
	}()
	if f.before != nil {
		f.before(ctx, line)
	}
	var out, errb bytes.Buffer
	code := Execute(args, &out, &errb)
	if f.after != nil {
		f.after(line, code)
	}
	return out.Bytes(), errb.Bytes(), code, nil
}

func (f *fakeBinaries) ran(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// trustTestKey makes the running binary trust a fresh key, as a test
// build does, and returns its secret half.
func trustTestKey(t *testing.T) minisign.PrivateKey {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := signing.TestKey
	signing.TestKey = pub.String()
	t.Cleanup(func() { signing.TestKey = old })
	return priv
}

type relOpts struct {
	unsigned      bool                 // no checksums.txt.minisig
	sig           []byte               // this checksums.txt.minisig instead
	comment       string               // sign with this trusted comment
	key           *minisign.PrivateKey // sign with this key instead
	tamperArchive bool                 // the archive differs from its checksum
	tamperBinary  bool                 // the archive's binary differs from the bare one's checksum
	legacy        bool                 // a legacy (not prehashed) signature
	noBareBinary  bool                 // checksums.txt lists no bare binary
	noNewline     bool                 // checksums.txt's last line has no newline
	platform      string               // the archive's <os>_<arch> (default: this machine's)
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// signedRelease writes the goreleaser release of version v under
// root/v<v>: this platform's archive (or o.platform's), the bare binary,
// checksums.txt and its signature.
func signedRelease(t *testing.T, root, v string, key minisign.PrivateKey, o relOpts) {
	t.Helper()
	dir := filepath.Join(root, "v"+v)
	_ = os.MkdirAll(dir, 0o755)
	body := fakeBody(v)
	inArchive := body
	if o.tamperBinary {
		inArchive = append(slices.Clone(body), []byte("# tampered\n")...)
	}
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if o.platform != "" {
		goos, goarch, _ = strings.Cut(o.platform, "_")
	}
	// goreleaser's shape: a .tar.gz holding backsight, on Windows a .zip
	// holding backsight.exe (release.Archive, unpackBinary).
	var tgz bytes.Buffer
	archive := "backsight_" + v + "_" + goos + "_" + goarch + ".tar.gz"
	if goos == "windows" {
		archive = strings.TrimSuffix(archive, ".tar.gz") + ".zip"
		zw := zip.NewWriter(&tgz)
		for _, f := range []struct {
			name string
			b    []byte
		}{{"LICENSE", []byte("MIT")}, {"backsight.exe", inArchive}} {
			w, _ := zw.Create(f.name)
			_, _ = w.Write(f.b)
		}
		_ = zw.Close()
	} else {
		gz := gzip.NewWriter(&tgz)
		tw := tar.NewWriter(gz)
		for name, b := range map[string][]byte{"LICENSE": []byte("MIT"), "backsight": inArchive} {
			_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg})
			_, _ = tw.Write(b)
		}
		_ = tw.Close()
		_ = gz.Close()
	}
	bare := selfcheck.BinaryName(v, goos, goarch)
	sums := sha(tgz.Bytes()) + "  " + archive + "\n"
	if !o.noBareBinary {
		sums += sha(body) + "  " + bare + "\n"
	}
	if o.noNewline {
		sums = strings.TrimSuffix(sums, "\n")
	}
	stored := tgz.Bytes()
	if o.tamperArchive {
		stored = append(slices.Clone(stored), 0)
	}
	files := map[string][]byte{archive: stored, bare: body, "checksums.txt": []byte(sums),
		"CHANGELOG.md": []byte("## Changelog\n* abc1234 ✨ Release " + v + "\n")}
	if !o.unsigned {
		k := key
		if o.key != nil {
			k = *o.key
		}
		comment := "timestamp:1\tfile:checksums.txt\thashed"
		if o.comment != "" {
			comment = o.comment
		}
		r := minisign.NewReader(strings.NewReader(sums))
		_, _ = io.Copy(io.Discard, r)
		files["checksums.txt.minisig"] = r.SignWithComments(k, comment, "x")
		if o.legacy {
			files["checksums.txt.minisig"] = minisign.SignWithComments(k, []byte(sums), comment, "x")
		}
		if o.sig != nil {
			files["checksums.txt.minisig"] = o.sig
		}
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// installedAt sets up a dev install whose shim runs a fake backsight v
// wired into a fake Claude Code, as client install leaves it.
func installedAt(t *testing.T, v string) (string, *fakeClaude, *fakeBinaries) {
	t.Helper()
	home := devInstall(t, true)
	configure(t, home, "[backup]\nenabled = false")
	f, b := installedOn(t, home, v)
	return home, f, b
}

// installedOn wires an existing dev install as installedAt does.
func installedOn(t *testing.T, home, v string) (*fakeClaude, *fakeBinaries) {
	t.Helper()
	f := withClaude(t)
	d := pathsOf(home)
	dl := filepath.Join(t.TempDir(), "backsight")
	if err := os.WriteFile(dl, fakeBody(v), 0o755); err != nil {
		t.Fatal(err)
	}
	setBuild(t, v, "abc1234")
	old := selfExe
	selfExe = func() (string, error) { return dl, nil }
	t.Cleanup(func() { selfExe = old })
	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatalf("client install: exit %d\n%s", code, out)
	}
	f.calls = nil
	f.marketplaces = `[{"name":"backsight","path":"` + testtool.JSONPath(filepath.Join(d.Data, "marketplace")) + `"}]`
	f.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	return f, withBinaries(t, d)
}

func shimOf(t *testing.T, d paths.Dirs) string {
	t.Helper()
	got, err := shimTargetOf(d)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// shimTargetOf is the installed binary the shim runs: its link's target, on
// Windows the <data>/bin/<version>/backsight.exe it is a copy of (the
// shim itself when it copies none).
func shimTargetOf(d paths.Dirs) (string, error) {
	shim := filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS))
	if runtime.GOOS != "windows" {
		return filepath.EvalSymlinks(shim)
	}
	raw, err := os.ReadFile(shim)
	if err != nil {
		return "", err
	}
	ents, _ := os.ReadDir(filepath.Join(d.Data, "bin"))
	for _, e := range ents {
		p := filepath.Join(d.Data, "bin", e.Name(), "backsight.exe")
		if b, err := os.ReadFile(p); err == nil && e.IsDir() && bytes.Equal(b, raw) {
			return p, nil
		}
	}
	return shim, nil
}

type applyEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Command       string        `json:"command"`
	OK            bool          `json:"ok"`
	Error         *output.Error `json:"error"`
	Data          struct {
		Installed  string   `json:"installed"`
		Target     string   `json:"target"`
		Plan       bool     `json:"plan"`
		UpToDate   bool     `json:"up_to_date"`
		KeyID      string   `json:"key_id"`
		Binary     string   `json:"binary"`
		Previous   string   `json:"previous"`
		Migrations []string `json:"migrations"`
		Plugin     string   `json:"plugin"`
		Server     *struct {
			Current  string `json:"current"`
			Target   string `json:"target"`
			Upgrade  bool   `json:"upgrade"`
			Upgraded bool   `json:"upgraded"`
			Safety   string `json:"safety_archive"`
		} `json:"server"`
		RolledBack bool     `json:"rolled_back"`
		Resumed    bool     `json:"resumed"`
		Steps      []step   `json:"steps"`
		Warnings   []string `json:"warnings"`
	} `json:"data"`
}

func decodeApply(t *testing.T, out string) applyEnvelope {
	t.Helper()
	var env applyEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not one JSON envelope: %v\n%s", err, out)
	}
	return env
}

// snapshot is what an update may change: the shim, the installed
// binaries, state.json, config.toml and the generated plugin.
func snapshot(t *testing.T, d paths.Dirs) string {
	t.Helper()
	var b strings.Builder
	shim, _ := shimTargetOf(d)
	b.WriteString("shim " + shim + "\n")
	ents, _ := os.ReadDir(filepath.Join(d.Data, "bin"))
	for _, e := range ents {
		b.WriteString("bin " + e.Name() + "\n")
	}
	for _, p := range []string{d.StateFile(), d.ConfigFile()} {
		raw, _ := os.ReadFile(p)
		b.WriteString(p + "\n" + string(raw) + "\n")
	}
	_ = filepath.WalkDir(filepath.Join(d.Data, "marketplace"), func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			raw, _ := os.ReadFile(p)
			b.WriteString(p + " " + sha(raw) + "\n")
		}
		return nil
	})
	if ents, _ := os.ReadDir(d.State); len(ents) > 0 {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "update") {
				b.WriteString("state " + e.Name() + "\n")
			}
		}
	}
	if _, err := os.Stat(d.MaintenanceLock()); err == nil {
		b.WriteString("maintenance lock\n")
	}
	return b.String()
}

func TestUpdateApplyInstallsTheNewRelease(t *testing.T) {
	home, claude, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	key := trustTestKey(t)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", key, relOpts{})
	old := shimOf(t, d)
	before, _ := state.Load(d.StateFile())
	ran := 0
	bins.migrations["0.2.0"] = []migrate.Migration{{ID: "0001-test", Run: func(context.Context, migrate.Env) error {
		ran++
		if !state.MaintenanceHeld(d.MaintenanceLock()) {
			t.Error("a migration ran without the maintenance lock")
		}
		return nil
	}}}

	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || !env.OK || env.SchemaVersion != 1 || env.Command != "update apply" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	now := shimOf(t, d)
	if !strings.HasPrefix(now, filepath.Join(d.Data, "bin", "0.2.0-")) || now == old {
		t.Fatalf("shim runs %s", now)
	}
	if raw, _ := os.ReadFile(now); !bytes.Equal(raw, fakeBody("0.2.0")) {
		t.Errorf("installed binary %q", raw)
	}
	for _, f := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(now), f)); err != nil {
			t.Errorf("%s not kept next to the binary: %v", f, err)
		}
	}
	// The installed binary checks itself against them (SPEC §11.1).
	if rec := selfcheck.Check(now, "0.2.0", runtime.GOOS, runtime.GOARCH, time.Now()); rec.Status != selfcheck.Verified {
		t.Errorf("self-check of the installed binary: %+v", rec)
	}
	st, _ := state.Load(d.StateFile())
	if st.ShimTarget != now || st.Client == nil || st.Client.ShimPrevious != old {
		t.Errorf("state shim %q previous %+v, want %q and %q", st.ShimTarget, st.Client, now, old)
	}
	if !slices.Equal(st.AppliedMigrations, []string{"0001-test"}) || ran != 1 {
		t.Errorf("migrations %v ran %d", st.AppliedMigrations, ran)
	}
	if st.PluginVersion == before.PluginVersion || !strings.HasPrefix(st.PluginVersion, "0.2.0") {
		t.Errorf("plugin version %q (was %q)", st.PluginVersion, before.PluginVersion)
	}
	if !slices.Contains(mutating(claude), "plugin update "+plugin.PluginID()) {
		t.Errorf("claude plugin update not run: %q", mutating(claude))
	}
	if bins.ran("0.2.0 update migrate") != 1 || bins.ran("0.2.0 client install") != 1 {
		t.Errorf("binary calls %q", bins.calls)
	}
	// The download and its checks (running the staged binary once) come
	// before the lock; everything after the install runs under it.
	for i, held := range bins.lockHeld {
		if staged := strings.HasPrefix(bins.calls[i], "0.2.0 version"); held == staged {
			t.Errorf("%s ran with the lock %v", bins.calls[i], held)
		}
	}
	if _, err := os.Stat(d.MaintenanceLock()); err == nil {
		t.Error("the maintenance lock was not released")
	}
	if _, err := os.Stat(d.UpdateMarker()); err == nil {
		t.Error("the update marker was left")
	}
	if e := env.Data; e.Installed != "0.1.0" || e.Target != "0.2.0" || e.Binary != now || e.Previous != old || !slices.Equal(e.Migrations, []string{"0001-test"}) || e.KeyID == "" || e.RolledBack {
		t.Errorf("result %+v", e)
	}

	// The new binary is up to date, and its migration never runs twice.
	setBuild(t, "0.2.0", "abc1234")
	code, out, _ = run(t, "update", "apply", "--source", src, "--json")
	if env := decodeApply(t, out); code != output.OK || !env.Data.UpToDate {
		t.Errorf("rerun: exit %d\n%s", code, out)
	}
	if _, _, code, _ := bins.Run(context.Background(), now, "update", "migrate", "--json"); code != output.OK || ran != 1 {
		t.Errorf("update migrate again: exit %d, migration ran %d times", code, ran)
	}
	if st, _ := state.Load(d.StateFile()); len(st.AppliedMigrations) != 1 {
		t.Errorf("recorded %v", st.AppliedMigrations)
	}
}

func TestUpdateApplyRefusesABadSignatureBeforeAnythingChanges(t *testing.T) {
	_, otherPriv, _ := minisign.GenerateKey(rand.Reader)
	// want: what the refusal names (the check that caught it).
	for name, c := range map[string]struct {
		o    relOpts
		want string
	}{
		"unsigned":                {relOpts{unsigned: true}, "has no checksums.txt.minisig"},
		"empty signature":         {relOpts{sig: []byte{}}, "fails the signature check (" + signing.ErrUnsigned.Error()},
		"untrusted key":           {relOpts{key: &otherPriv}, "fails the signature check (" + signing.ErrUntrustedKey.Error()},
		"legacy signature":        {relOpts{legacy: true}, "fails the signature check (" + signing.ErrLegacySignature.Error()},
		"signed for another file": {relOpts{comment: "timestamp:1\tfile:SHA256SUMS\thashed"}, "fails the signature check (" + signing.ErrWrongFile.Error()},
		"tampered archive":        {relOpts{tamperArchive: true}, "fails the checksum check"},
		"tampered binary":         {relOpts{tamperBinary: true}, "is not the release's"},
	} {
		t.Run(name, func(t *testing.T) {
			home, claude, bins := installedAt(t, "0.1.0")
			d := pathsOf(home)
			src := t.TempDir()
			signedRelease(t, src, "0.2.0", trustTestKey(t), c.o)
			before := snapshot(t, d)
			code, out, _ := run(t, "update", "apply", "--source", src, "--json")
			env := decodeApply(t, out)
			if code != output.Blocked || env.OK || env.Error == nil || !strings.Contains(env.Error.Message, "nothing was changed") || !strings.Contains(env.Error.Message, c.want) {
				t.Fatalf("exit %d, want %q\n%s", code, c.want, out)
			}
			if after := snapshot(t, d); after != before {
				t.Errorf("changed:\n%s\nwas:\n%s", after, before)
			}
			if len(bins.calls) != 0 || len(mutating(claude)) != 0 {
				t.Errorf("ran %q, claude %q", bins.calls, mutating(claude))
			}
		})
	}
}

// A local snapshot (make snapshot: goreleaser's incpatch version, newer
// than the installed pre-release, signed with a throwaway key) is refused
// by a binary that trusts the release keys only, before anything changes
// (security review M3).
func TestUpdateApplyRefusesASnapshotSignedByAThrowawayKey(t *testing.T) {
	old := signing.TestKey
	signing.TestKey = "" // a release binary: minisign.pub and minisign.next.pub only
	t.Cleanup(func() { signing.TestKey = old })
	home, claude, bins := installedAt(t, "0.1.0-m5")
	d := pathsOf(home)
	_, throwaway, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	signedRelease(t, src, "0.1.1-snapshot+1300233", throwaway, relOpts{})
	before := snapshot(t, d)
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.Blocked || env.OK || env.Error == nil || !strings.Contains(env.Error.Message, "0.1.1-snapshot+1300233") ||
		!strings.Contains(env.Error.Message, "fails the signature check ("+signing.ErrUntrustedKey.Error()) || !strings.Contains(env.Error.Message, "nothing was changed") {
		t.Fatalf("exit %d, want the snapshot refused for its key\n%s", code, out)
	}
	if after := snapshot(t, d); after != before {
		t.Errorf("changed:\n%s\nwas:\n%s", after, before)
	}
	if len(bins.calls) != 0 || len(mutating(claude)) != 0 {
		t.Errorf("ran %q, claude %q", bins.calls, mutating(claude))
	}
}

// checksums.txt without the bare binary (an older release layout): the
// update installs with a warning, and the new binary's self-check fails
// until a release lists it. A last line without its newline still counts.
func TestUpdateApplyChecksTheBareBinaryWhenListed(t *testing.T) {
	t.Run("not listed", func(t *testing.T) {
		home, _, _ := installedAt(t, "0.1.0")
		src := t.TempDir()
		signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{noBareBinary: true})
		code, out, _ := run(t, "update", "apply", "--source", src, "--json")
		env := decodeApply(t, out)
		bare := selfcheck.BinaryName("0.2.0", runtime.GOOS, runtime.GOARCH)
		if code != output.OK || !slices.ContainsFunc(env.Data.Warnings, func(w string) bool { return strings.Contains(w, "lists no "+bare) }) {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if rec := selfcheck.Check(shimOf(t, pathsOf(home)), "0.2.0", runtime.GOOS, runtime.GOARCH, time.Now()); rec.Status != selfcheck.Failed {
			t.Errorf("self-check of a binary its release does not list: %+v", rec)
		}
	})
	t.Run("last line without a newline", func(t *testing.T) {
		home, _, _ := installedAt(t, "0.1.0")
		d := pathsOf(home)
		src := t.TempDir()
		signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{noNewline: true, tamperBinary: true})
		before := snapshot(t, d)
		code, out, _ := run(t, "update", "apply", "--source", src, "--json")
		if code != output.Blocked || !strings.Contains(out, "is not the release's") {
			t.Fatalf("the listed binary was not checked: exit %d\n%s", code, out)
		}
		if snapshot(t, d) != before {
			t.Error("changed")
		}
	})
}

func TestUpdateApplyRefusesAnOlderOrEqualVersion(t *testing.T) {
	home, _, bins := installedAt(t, "0.2.0")
	d := pathsOf(home)
	key := trustTestKey(t)
	src := t.TempDir()
	for _, v := range []string{"0.1.0", "0.2.0"} {
		signedRelease(t, src, v, key, relOpts{})
	}
	before := snapshot(t, d)
	for _, v := range []string{"0.1.0", "0.2.0", "v0.2.0"} {
		code, out, _ := run(t, "update", "apply", "--source", src, "--version", v, "--json")
		if env := decodeApply(t, out); code != output.Usage || env.Error == nil || !strings.Contains(env.Error.Message, "not newer") {
			t.Errorf("--version %s: exit %d\n%s", v, code, out)
		}
	}
	// Nothing newer: up to date, nothing to do.
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	if env := decodeApply(t, out); code != output.OK || !env.Data.UpToDate || env.Data.Target != "" {
		t.Errorf("no newer release: exit %d\n%s", code, out)
	}
	// A version the source does not have.
	if code, out, _ := run(t, "update", "apply", "--source", src, "--version", "0.9.0", "--json"); code != output.Blocked {
		t.Errorf("absent version: exit %d\n%s", code, out)
	}
	if after := snapshot(t, d); after != before || len(bins.calls) != 0 {
		t.Errorf("changed (calls %q):\n%s\nwas:\n%s", bins.calls, after, before)
	}
}

func TestUpdateApplyPlanChangesNothing(t *testing.T) {
	home, claude, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	ran := false
	bins.migrations["0.2.0"] = []migrate.Migration{{ID: "0001-test", Run: func(context.Context, migrate.Env) error { ran = true; return nil }}}
	before := snapshot(t, d)
	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || !env.Data.Plan || env.Data.Target != "0.2.0" || env.Data.KeyID == "" || !slices.Equal(env.Data.Migrations, []string{"0001-test"}) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	planned := map[string]bool{}
	for _, s := range env.Data.Steps {
		if s.Status != "planned" && s.Status != "done" && s.Status != "skipped" {
			t.Errorf("plan step %+v", s)
		}
		planned[s.Name] = true
	}
	for _, s := range []string{"download", "verify", "install", "shim", "migrations", "plugin"} {
		if !planned[s] {
			t.Errorf("plan lacks %s: %+v", s, env.Data.Steps)
		}
	}
	if after := snapshot(t, d); after != before || ran || len(mutating(claude)) != 0 || bins.ran("0.2.0 client install") != 0 {
		t.Errorf("the plan changed something (migration ran %v, claude %q, calls %q):\n%s\nwas:\n%s", ran, mutating(claude), bins.calls, after, before)
	}
}

// A failing migration rolls everything back: the shim, state.json and
// config.toml as they were, the new binary removed, exit 30.
func TestUpdateApplyFailingMigrationRollsBack(t *testing.T) {
	home, claude, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	bins.migrations["0.2.0"] = []migrate.Migration{
		{ID: "0001-edits", Run: func(_ context.Context, e migrate.Env) error {
			raw, _ := os.ReadFile(e.Dirs.ConfigFile())
			return os.WriteFile(e.Dirs.ConfigFile(), append(raw, []byte("\n# migrated\n")...), 0o600)
		}},
		{ID: "0002-fails", Run: func(context.Context, migrate.Env) error { return errors.New("cannot migrate") }},
	}
	before := snapshot(t, d)
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.RolledBack || env.OK || !env.Data.RolledBack || env.Error == nil || !strings.Contains(env.Error.Message, "cannot migrate") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if after := snapshot(t, d); after != before {
		t.Errorf("not rolled back:\n%s\nwas:\n%s", after, before)
	}
	if bins.ran("0.2.0 client install") != 0 || len(mutating(claude)) != 0 {
		t.Errorf("the plugin step ran after a failed migration: %q %q", bins.calls, mutating(claude))
	}
}

// A Ctrl-C during the update rolls it back too (the rollback itself is
// not cancelled).
func TestUpdateApplyInterruptedRollsBack(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bins.migrations["0.2.0"] = []migrate.Migration{{ID: "0001-ctrl-c", Run: func(context.Context, migrate.Env) error { cancel(); return nil }}}
	before := snapshot(t, d)
	var out, errb bytes.Buffer
	code := ExecuteContext(ctx, []string{"update", "apply", "--source", src, "--json"}, &out, &errb)
	if code != output.RolledBack {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if after := snapshot(t, d); after != before {
		t.Errorf("not rolled back:\n%s\nwas:\n%s", after, before)
	}
	if bins.ran("0.2.0 client install") != 0 {
		t.Errorf("went on after the cancel: %q", bins.calls)
	}
}

// A Ctrl-C while the new binary installs its plugin: the rollback is not
// cancelled with it, and puts the previous plugin back (exit 30).
func TestUpdateApplyInterruptedDuringThePluginRollsBack(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bins.before = func(_ context.Context, line string) {
		if strings.HasPrefix(line, "0.2.0 client install") {
			cancel()
		}
	}
	before := snapshot(t, d)
	beforeState, _ := state.Load(d.StateFile())
	var out, errb bytes.Buffer
	code := ExecuteContext(ctx, []string{"update", "apply", "--source", src, "--json"}, &out, &errb)
	if code != output.RolledBack || !decodeApply(t, out.String()).Data.RolledBack {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if bins.ran("0.1.0 client install") != 1 || bins.ran("0.2.0 server") != 0 {
		t.Errorf("calls %q", bins.calls)
	}
	if st, _ := state.Load(d.StateFile()); st.PluginVersion != beforeState.PluginVersion {
		t.Errorf("plugin %q, want %q", st.PluginVersion, beforeState.PluginVersion)
	}
	if after := snapshot(t, d); after != before {
		t.Errorf("not rolled back:\n%s\nwas:\n%s", after, before)
	}
}

// A failure in the plugin step, after the new plugin was generated, puts
// the previous binary's plugin back through its own client install.
func TestUpdateApplyFailingPluginStepRollsBack(t *testing.T) {
	home, claude, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	old := shimOf(t, d)
	beforeState, _ := state.Load(d.StateFile())
	claude.failOn, claude.failOnly = "plugin update", 1 // the new binary's call fails, the rollback's passes
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	if code != output.RolledBack {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if shimOf(t, d) != old {
		t.Errorf("shim %s, want %s", shimOf(t, d), old)
	}
	if bins.ran("0.1.0 client install") != 1 {
		t.Errorf("the previous binary did not put its plugin back: %q", bins.calls)
	}
	st, _ := state.Load(d.StateFile())
	if st.PluginVersion != beforeState.PluginVersion || st.ShimTarget != old || len(st.AppliedMigrations) != 0 {
		t.Errorf("state %+v, want plugin %q", st, beforeState.PluginVersion)
	}
	manifest, _ := os.ReadFile(filepath.Join(d.Data, "marketplace", "plugins", "backsight", ".claude-plugin", "plugin.json"))
	if !strings.Contains(string(manifest), beforeState.PluginVersion) {
		t.Errorf("generated plugin is not the previous one:\n%s", manifest)
	}
	if _, err := os.Stat(d.UpdateMarker()); err == nil {
		t.Error("marker left after a finished rollback")
	}
}

// A rollback that fails exits 20, keeps the marker, and doctor names the
// way back.
func TestUpdateApplyFailedRollbackKeepsTheMarker(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	old := shimOf(t, d)
	// Wired into a Claude Code profile: the way back names it.
	_ = state.Update(d, "test", func(s *state.State) error { s.Client.Profile = "/p"; return nil })
	way := old + " client install --config-dir /p"
	bins.fail["0.2.0 client install"] = output.Partial
	bins.fail["0.1.0 client install"] = output.Blocked
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "run "+way+" to return") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if bins.ran("0.2.0 client install --config-dir /p") != 1 || bins.ran("0.1.0 client install --config-dir /p") != 1 {
		t.Errorf("calls %q", bins.calls)
	}
	m, err := state.ReadUpdateMarker(d)
	if err != nil || m == nil || m.Previous != old || m.To != "0.2.0" || m.WayBack != way {
		t.Fatalf("marker %+v %v", m, err)
	}
	rep := doctorReport(t)
	c := findCheck(rep, "update-incomplete")
	if c == nil || c.Status != doctor.Error || !strings.Contains(c.Fix, way+" (back to 0.1.0)") {
		t.Errorf("doctor %+v", c)
	}
}

func TestUpdateApplyRefusesWhileMaintenanceRuns(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	// A live owner, someone else.
	raw, _ := json.Marshal(state.Owner{PID: liveProcess(t), Command: "backup create", Since: time.Now().UTC().Truncate(time.Second)})
	_ = os.MkdirAll(d.State, 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), raw, 0o600)
	before := snapshot(t, d)
	for _, args := range [][]string{{"update", "apply"}, {"update", "apply", "--plan"}} {
		code, out, _ := run(t, append(args, "--source", src, "--json")...)
		if code != output.Blocked || !strings.Contains(out, "maintenance in progress") {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
	if after := snapshot(t, d); after != before || len(bins.calls) != 0 {
		t.Errorf("changed:\n%s", after)
	}
	// update migrate does not run under someone else's lock either.
	ran := false
	old := migrations
	migrations = func() []migrate.Migration {
		return []migrate.Migration{{ID: "0001-x", Run: func(context.Context, migrate.Env) error { ran = true; return nil }}}
	}
	t.Cleanup(func() { migrations = old })
	if code, out, _ := run(t, "update", "migrate", "--json"); code != output.Blocked || ran {
		t.Errorf("update migrate: exit %d, ran %v\n%s", code, ran, out)
	}
}

func TestUpdateApplyRefusesWhileARestoreIsIncomplete(t *testing.T) {
	home, _, _ := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	_ = state.WriteRestoreMarker(d, state.RestoreMarker{Command: "restore", Safety: "/x"})
	if code, out, _ := run(t, "update", "apply", "--source", src, "--json"); code != output.Blocked || !strings.Contains(out, "restore did not finish") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A binary that says it is another version than its release is refused.
func TestUpdateApplyRefusesABinaryOfAnotherVersion(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	bins.claimVersion["0.2.0"] = "0.3.0"
	before := snapshot(t, d)
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	if code != output.Blocked || !strings.Contains(out, "reports version 0.3.0") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if after := snapshot(t, d); after != before {
		t.Errorf("changed:\n%s", after)
	}
}

// jobs maintain only checks unless update.auto_apply is set (default off).
func TestMaintainAppliesOnlyWhenAutoApplyIsOn(t *testing.T) {
	home, _, _ := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	configure(t, home, "[update]\nsource = "+testtool.TOMLStr(src)+"")
	old := shimOf(t, d)
	code, out, _ := run(t, "jobs", "maintain", "--json")
	if code != output.OK || !strings.Contains(out, `"update": "0.2.0 available"`) || shimOf(t, d) != old {
		t.Fatalf("auto_apply off: exit %d, shim %s\n%s", code, shimOf(t, d), out)
	}
	raw, _ := os.ReadFile(d.ConfigFile())
	_ = os.WriteFile(d.ConfigFile(), []byte(strings.Replace(string(raw), "[update]\n", "[update]\nauto_apply = true\n", 1)), 0o600)
	code, out, _ = run(t, "jobs", "maintain", "--json")
	if code != output.OK || !strings.Contains(out, `"update": "applied 0.2.0"`) || !strings.HasPrefix(shimOf(t, d), filepath.Join(d.Data, "bin", "0.2.0-")) {
		t.Errorf("auto_apply on: exit %d, shim %s\n%s", code, shimOf(t, d), out)
	}
}

// A failing auto-apply never fails jobs maintain, and is not retried
// every day (each try costs a safety backup and a rollback): the failed
// release is recorded and skipped until a newer one appears or the user
// runs update apply.
func TestMaintainSkipsAReleaseWhoseAutoApplyFailed(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	key := trustTestKey(t)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", key, relOpts{})
	configure(t, home, "[update]\nsource = "+testtool.TOMLStr(src)+"\nauto_apply = true")
	bins.migrations["0.2.0"] = []migrate.Migration{{ID: "0001-fails", Run: func(context.Context, migrate.Env) error { return errors.New("cannot migrate") }}}
	old := shimOf(t, d)
	maintain := func() string {
		t.Helper()
		code, out, _ := run(t, "jobs", "maintain", "--json")
		var env struct {
			Data maintainResult `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
			t.Fatalf("jobs maintain failed: exit %d %v\n%s", code, err, out)
		}
		return env.Data.Update
	}
	if u := maintain(); !strings.Contains(u, "auto-apply failed (exit 30)") || shimOf(t, d) != old {
		t.Fatalf("update %q, shim %s", u, shimOf(t, d))
	}
	st, _ := state.Load(d.StateFile())
	if st.AutoApply == nil || st.AutoApply.Failed != "0.2.0" || st.AutoApply.Code != output.RolledBack {
		t.Fatalf("failure not recorded: %+v", st.AutoApply)
	}
	// The next day: skipped, nothing downloaded or run.
	bins.calls = nil
	if u := maintain(); !strings.Contains(u, "0.2.0 available, auto-apply skipped") || len(bins.calls) != 0 {
		t.Errorf("retried: update %q, calls %q", u, bins.calls)
	}
	// update apply by hand clears the record (here it fails again), so
	// the next maintain tries again.
	if code, out, _ := run(t, "update", "apply", "--source", src, "--json"); code != output.RolledBack {
		t.Fatalf("by hand: exit %d\n%s", code, out)
	}
	if st, _ := state.Load(d.StateFile()); st.AutoApply != nil {
		t.Errorf("record kept after update apply by hand: %+v", st.AutoApply)
	}
	bins.calls = nil
	if u := maintain(); !strings.Contains(u, "auto-apply failed") || bins.ran("0.2.0 update migrate") != 1 {
		t.Errorf("not retried after update apply by hand: %q %q", u, bins.calls)
	}
	// A newer release is tried, and its success clears the record.
	signedRelease(t, src, "0.3.0", key, relOpts{})
	if u := maintain(); u != "applied 0.3.0" || !strings.HasPrefix(shimOf(t, d), filepath.Join(d.Data, "bin", "0.3.0-")) {
		t.Errorf("newer release: %q, shim %s", u, shimOf(t, d))
	}
	if st, _ := state.Load(d.StateFile()); st.AutoApply != nil {
		t.Errorf("record kept after a success: %+v", st.AutoApply)
	}
}

func TestDoctorPointsAtUpdateApply(t *testing.T) {
	home := devInstall(t, true)
	setBuild(t, "0.1.0", "abc1234")
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.UpdateCheck = &state.UpdateCheck{Latest: "0.2.0", Source: "dir:/x"}
		return nil
	})
	if c := findCheck(doctorReport(t), "update"); c == nil || c.Fix != "backsight update apply" {
		t.Errorf("%+v", c)
	}
}

// The real runner returns the child's stdout and exit code.
func TestExecBinaryRunsAProgram(t *testing.T) {
	exe := testtool.Write(t, t.TempDir(), "child", testtool.Fake{Script: "argline", Stderr: "progress\n", Code: 30})
	stdout, stderr, code, err := execBinary{}.Run(context.Background(), exe, "update", "migrate")
	if err != nil || code != 30 || string(stdout) != "update migrate\n" || string(stderr) != "progress\n" {
		t.Errorf("%q %q %d %v", stdout, stderr, code, err)
	}
	if _, _, _, err := (execBinary{}).Run(context.Background(), filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing binary is no error")
	}
	// With Progress, the child's stderr also streams there.
	var progress bytes.Buffer
	if _, stderr, _, _ := (execBinary{Progress: &progress}).Run(context.Background(), exe); string(stderr) != "progress\n" || progress.String() != "progress\n" {
		t.Errorf("stderr %q, progress %q", stderr, progress.String())
	}
}

// A cancelled update asks its child's process group to stop (SIGTERM) and
// waits for it: the child's own rollback runs and its exit code comes
// back. The child is told which maintenance lock its parent holds.
func TestExecBinaryStopsTheChildGently(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: the fake child is a POSIX shell script with a TERM trap; Windows asks a child to stop with CTRL_BREAK, which needs a console")
	dir := t.TempDir()
	exe := filepath.Join(dir, "child")
	_ = os.WriteFile(exe, []byte("#!/bin/sh\necho \"$"+updateLockEnv+"\" > "+dir+"/token\ntrap 'echo rolled back > "+dir+"/trap; exit 30' TERM\nsleep 30 &\nwait\n"), 0o755)
	owner := state.Owner{PID: os.Getpid(), Command: "update apply", Since: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	updateLock = &owner
	t.Cleanup(func() { updateLock = nil })
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 250; i++ {
			if _, err := os.Stat(filepath.Join(dir, "token")); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	_, _, code, err := execBinary{}.Run(ctx, exe)
	if err != nil || code != 30 {
		t.Fatalf("exit %d %v: the child was not left to roll back", code, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "trap")); string(raw) != "rolled back\n" || time.Since(start) > 20*time.Second {
		t.Errorf("trap %q after %s", raw, time.Since(start))
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "token")); strings.TrimSpace(string(raw)) != lockToken(owner) {
		t.Errorf("the child's lock token %q, want %q", raw, lockToken(owner))
	}
}

// heldByUpdate accepts the parent's lock only when the parent named that
// very lock (PID and start time) in the child's environment.
func TestHeldByUpdateNeedsTheParentsToken(t *testing.T) {
	since := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	parent := state.Owner{PID: os.Getppid(), Command: "update apply", Since: since}
	h := &state.HeldError{Owner: parent}
	t.Setenv(updateLockEnv, "")
	if heldByUpdate(h) {
		t.Error("the parent's PID alone passed")
	}
	t.Setenv(updateLockEnv, lockToken(parent))
	if !heldByUpdate(h) {
		t.Error("the parent's own lock refused")
	}
	if heldByUpdate(&state.HeldError{Owner: state.Owner{PID: parent.PID, Command: "update apply", Since: since.Add(-time.Hour)}}) {
		t.Error("an older lock of the same PID (reused) passed")
	}
	if heldByUpdate(&state.HeldError{Owner: state.Owner{PID: parent.PID, Command: "backup create", Since: since}}) {
		t.Error("another command's lock passed")
	}
	other := state.Owner{PID: liveProcess(t), Command: "update apply", Since: since}
	t.Setenv(updateLockEnv, lockToken(other))
	if heldByUpdate(&state.HeldError{Owner: other}) {
		t.Error("a lock neither this process's nor its parent's passed")
	}
}

func doctorReport(t *testing.T) doctor.Report {
	t.Helper()
	_, out, _ := run(t, "doctor", "--json")
	var env struct {
		Data doctor.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return env.Data
}

func findCheck(r doctor.Report, id string) *doctor.Check {
	for i := range r.Checks {
		if r.Checks[i].ID == id {
			return &r.Checks[i]
		}
	}
	return nil
}

// serverInstalledAt is a fake backsight v wired into Claude Code with a
// fake server deployed on the current pin (restore's fakes); v2 pins
// newImage.
func serverInstalledAt(t *testing.T, v, v2 string) (home, archive string, dk *restoreDocker, claude *fakeClaude, bins *fakeBinaries, oldImage string) {
	t.Helper()
	home, archive, dk, _ = restoreSetup(t)
	oldImage = server.HindsightImage
	if code, out, _ := run(t, "server", "render"); code != output.OK {
		t.Fatalf("render: exit %d\n%s", code, out)
	}
	claude, bins = installedOn(t, home, v)
	bins.images[v2] = newImage
	dk.calls, dk.unlocked, dk.images = nil, nil, nil
	return
}

func TestUpdateApplyUpgradesTheServerWithTheNewBinarysPin(t *testing.T) {
	home, _, dk, _, bins, oldImage := serverInstalledAt(t, "0.1.0", "0.2.0")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})

	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || env.Data.Server == nil || !env.Data.Server.Upgrade || env.Data.Server.Current != oldImage || env.Data.Server.Target != newImage {
		t.Fatalf("plan: exit %d\n%s", code, out)
	}
	if deployedImage(t, home) != oldImage || len(dk.images) != 0 {
		t.Fatalf("the plan deployed %s (ups %v)", deployedImage(t, home), dk.images)
	}

	bins.calls = nil
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	env = decodeApply(t, out)
	if code != output.OK || env.Data.Server == nil || !env.Data.Server.Upgraded {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := deployedImage(t, home); got != newImage {
		t.Errorf("deployed %s", got)
	}
	if bins.ran("0.2.0 server upgrade") != 2 { // --plan, then the upgrade
		t.Errorf("calls %q", bins.calls)
	}
	if len(dk.unlocked) > 0 {
		t.Errorf("docker calls without the lock: %q", dk.unlocked)
	}
	if !strings.HasPrefix(shimOf(t, pathsOf(home)), filepath.Join(pathsOf(home).Data, "bin", "0.2.0-")) {
		t.Errorf("shim %s", shimOf(t, pathsOf(home)))
	}
}

// The new version fails to start: the new binary's server upgrade puts
// the previous version and its data back, then update apply rolls back
// the client: everything as it was, exit 30.
func TestUpdateApplyServerFailureRollsEverythingBack(t *testing.T) {
	home, _, dk, claude, bins, oldImage := serverInstalledAt(t, "0.1.0", "0.2.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	dk.failImage, dk.migrateImage = newImage, newImage // migrates the data, then does not start
	old := shimOf(t, d)
	beforeState, _ := state.Load(d.StateFile())
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	if code != output.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if deployedImage(t, home) != oldImage || dk.rows != 25 || !dk.running || !slices.Equal(dk.restores, []int{25}) {
		t.Errorf("server: image %s rows %d running %v restores %v", deployedImage(t, home), dk.rows, dk.running, dk.restores)
	}
	st, _ := state.Load(d.StateFile())
	if shimOf(t, d) != old || st.PluginVersion != beforeState.PluginVersion || bins.ran("0.1.0 client install") != 1 {
		t.Errorf("client: shim %s plugin %q calls %q", shimOf(t, d), st.PluginVersion, bins.calls)
	}
	if !slices.Contains(mutating(claude), "plugin update "+plugin.PluginID()) {
		t.Errorf("claude %q", mutating(claude))
	}
	for _, p := range []string{d.RestoreMarker(), d.UpdateMarker(), d.MaintenanceLock()} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s left", p)
		}
	}
}

// The new binary's server upgrade dies with the upgrade marked in
// progress: update apply restores the safety archive itself, through
// restore and with its own pin.
func TestUpdateApplyRestoresWhenTheServerStepDied(t *testing.T) {
	home, archive, dk, _, bins, oldImage := serverInstalledAt(t, "0.1.0", "0.2.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	bins.fail["0.2.0 server upgrade --json"] = -1
	bins.onFail = func(string) {
		// What a killed upgrade leaves: the new version deployed, the
		// database migrated, the marker naming the safety archive.
		raw, _ := os.ReadFile(filepath.Join(composeDir(home), "compose.yaml"))
		_ = os.WriteFile(filepath.Join(composeDir(home), "compose.yaml"), []byte(strings.Replace(string(raw), oldImage, newImage, 1)), 0o644)
		dk.rows = 99
		_ = state.WriteRestoreMarker(d, state.RestoreMarker{Command: "server upgrade", Safety: archive, Archive: archive})
	}
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	if code != output.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if deployedImage(t, home) != oldImage || dk.rows != 10 || !dk.running || bins.ran("0.1.0 restore "+archive+" --no-safety --non-interactive") != 1 {
		t.Errorf("image %s rows %d (want the archive's 10) running %v calls %q", deployedImage(t, home), dk.rows, dk.running, bins.calls)
	}
	if state.RestoreIncomplete(d) {
		t.Error("marker left")
	}
}

// holdStateLock makes state.json's lock someone else's (a live process),
// as a tick or a hook writing state holds it.
func holdStateLock(t *testing.T, d paths.Dirs) {
	t.Helper()
	raw, _ := json.Marshal(state.Owner{PID: liveProcess(t), Command: "jobs tick", Since: time.Now().UTC().Truncate(time.Second)})
	_ = os.MkdirAll(d.LocksDir(), 0o700)
	if err := os.WriteFile(filepath.Join(d.LocksDir(), "state.lock"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func shortRecordWait(t *testing.T, w time.Duration) {
	t.Helper()
	old := recordWait
	recordWait = w
	t.Cleanup(func() { recordWait = old })
}

// Once the new binary's server upgrade passed, the update is committed: a
// record step that cannot get state.json's lock is never rolled back (the
// old pin would start on a database the new Hindsight migrated). It exits
// 40, keeps the marker, and a rerun of the new binary finishes it.
func TestUpdateApplyIsCommittedOnceTheServerUpgraded(t *testing.T) {
	home, _, dk, _, bins, oldImage := serverInstalledAt(t, "0.1.0", "0.2.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	shortRecordWait(t, 200*time.Millisecond)
	bins.after = func(line string, code int) {
		if line == "0.2.0 server upgrade --json" && code == output.OK {
			holdStateLock(t, d)
		}
	}
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.Partial || env.Error == nil || !strings.Contains(env.Error.Message, "rerun backsight update apply") || env.Data.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if deployedImage(t, home) != newImage || len(dk.restores) != 0 || bins.ran("0.1.0") != 0 {
		t.Errorf("rolled back: image %s (was %s), restores %v, calls %q", deployedImage(t, home), oldImage, dk.restores, bins.calls)
	}
	target := shimOf(t, d)
	if !strings.HasPrefix(target, filepath.Join(d.Data, "bin", "0.2.0-")) {
		t.Errorf("shim %s", target)
	}
	m, err := state.ReadUpdateMarker(d)
	if err != nil || m == nil || !m.Committed || m.Safety == "" || m.Safety != env.Data.Server.Safety {
		t.Fatalf("marker %+v %v (result safety %q)", m, err, env.Data.Server.Safety)
	}
	if c := findCheck(doctorReport(t), "update-incomplete"); c == nil || !strings.Contains(c.Fix, "update apply") || strings.Contains(c.Fix, "client install") {
		t.Errorf("doctor %+v", c)
	}
	// The old binary must not roll a committed update back.
	_ = os.Remove(filepath.Join(d.LocksDir(), "state.lock"))
	// The message as decoded: JSON escapes a Windows path's backslashes.
	if code, out, _ := run(t, "update", "apply", "--source", src, "--json"); code != output.NeedsUser || decodeApply(t, out).Error == nil ||
		!strings.Contains(decodeApply(t, out).Error.Message, target+" update apply") {
		t.Errorf("the old binary on a committed update: exit %d\n%s", code, out)
	}
	// The new binary resumes: only the record was left.
	bins.calls = nil
	setBuild(t, "0.2.0", "abc1234")
	code, out, _ = run(t, "update", "apply", "--source", src, "--json")
	if env := decodeApply(t, out); code != output.OK || !env.Data.Resumed || env.Data.Target != "0.2.0" {
		t.Fatalf("resume: exit %d\n%s", code, out)
	}
	if bins.ran("0.2.0 server upgrade --json") != 0 || bins.ran("0.2.0 client install") != 0 {
		t.Errorf("a committed update ran its steps again: %q", bins.calls)
	}
	if st, _ := state.Load(d.StateFile()); st.ShimTarget != target || st.Client == nil || st.Client.ShimPrevious == "" {
		t.Errorf("not recorded: %+v", st)
	}
	if state.UpdateIncomplete(d) {
		t.Error("marker left")
	}
}

// A state lock held for a moment is waited for.
func TestUpdateApplyRecordWaitsForTheStateLock(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	shortRecordWait(t, 10*time.Second)
	bins.after = func(line string, code int) {
		if strings.HasPrefix(line, "0.2.0 client install") {
			holdStateLock(t, d)
			go func() { time.Sleep(300 * time.Millisecond); _ = os.Remove(filepath.Join(d.LocksDir(), "state.lock")) }()
		}
	}
	if code, out, _ := run(t, "update", "apply", "--source", src, "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if st, _ := state.Load(d.StateFile()); st.ShimTarget != shimOf(t, d) {
		t.Errorf("not recorded: %q", st.ShimTarget)
	}
}

// The new binary's server upgrade dies after it passed (its marker
// cleared, the server on the new pin): update apply sees the upgrade done
// and finishes, without touching the server.
func TestUpdateApplyKeepsAnUpgradeThatPassedBeforeItsChildDied(t *testing.T) {
	home, _, dk, _, bins, oldImage := serverInstalledAt(t, "0.1.0", "0.2.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	bins.fail["0.2.0 server upgrade --json"] = -1
	bins.onFail = func(string) {
		raw, _ := os.ReadFile(filepath.Join(composeDir(home), "compose.yaml"))
		_ = os.WriteFile(filepath.Join(composeDir(home), "compose.yaml"), []byte(strings.Replace(string(raw), oldImage, newImage, 1)), 0o644)
		dk.rows = 99 // migrated by the new version
	}
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || env.Data.Server == nil || !env.Data.Server.Upgraded || !strings.Contains(out, "it passed") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if deployedImage(t, home) != newImage || dk.rows != 99 || len(dk.restores) != 0 || bins.ran("0.1.0") != 0 {
		t.Errorf("the server was rolled back: image %s rows %d restores %v calls %q", deployedImage(t, home), dk.rows, dk.restores, bins.calls)
	}
	if state.UpdateIncomplete(d) || !strings.HasPrefix(shimOf(t, d), filepath.Join(d.Data, "bin", "0.2.0-")) {
		t.Errorf("not finished: shim %s", shimOf(t, d))
	}
}

// The rollback puts state.json and config.toml back before the previous
// binary restores the server: that binary cannot read the files the new
// one migrated (here a renamed bank, which the safety archive's restore
// refuses).
func TestUpdateApplyRollbackRestoresTheFilesBeforeTheServer(t *testing.T) {
	home, archive, dk, _, bins, oldImage := serverInstalledAt(t, "0.1.0", "0.2.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	beforeState, _ := os.ReadFile(d.StateFile())
	beforeConfig, _ := os.ReadFile(d.ConfigFile())
	bins.migrations["0.2.0"] = []migrate.Migration{{ID: "0001-newer-files", Run: func(_ context.Context, e migrate.Env) error {
		raw, _ := os.ReadFile(e.Dirs.ConfigFile())
		if err := os.WriteFile(e.Dirs.ConfigFile(), []byte(strings.Replace(string(raw), "[server]\n", "[server]\nbank = \"renamed\"\n", 1)), 0o600); err != nil {
			return err
		}
		return state.Update(e.Dirs, "test", func(s *state.State) error { s.PluginVersion = "migrated"; return nil })
	}}}
	bins.fail["0.2.0 server upgrade --json"] = -1
	bins.onFail = func(string) { // killed half way: new pin deployed, data migrated, marker left
		raw, _ := os.ReadFile(filepath.Join(composeDir(home), "compose.yaml"))
		_ = os.WriteFile(filepath.Join(composeDir(home), "compose.yaml"), []byte(strings.Replace(string(raw), oldImage, newImage, 1)), 0o644)
		dk.rows = 99
		_ = state.WriteRestoreMarker(d, state.RestoreMarker{Command: "server upgrade", Safety: archive, Archive: archive})
	}
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	if code != output.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if bins.ran("0.1.0 restore "+archive+" --no-safety") != 1 || deployedImage(t, home) != oldImage || dk.rows != 10 || len(dk.restores) != 1 {
		t.Errorf("server: calls %q image %s rows %d restores %v", bins.calls, deployedImage(t, home), dk.rows, dk.restores)
	}
	if st, _ := os.ReadFile(d.StateFile()); string(st) != string(beforeState) {
		t.Errorf("state.json:\n%s", st)
	}
	if cfg, _ := os.ReadFile(d.ConfigFile()); string(cfg) != string(beforeConfig) {
		t.Errorf("config.toml:\n%s", cfg)
	}
}

// The rollback writes state.json under its lock: a tick holding it is
// waited for, not overwritten.
func TestUpdateApplyRollbackWritesTheStateUnderItsLock(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	before := snapshot(t, d)
	var duringLock string
	done := make(chan struct{})
	bins.migrations["0.2.0"] = []migrate.Migration{{ID: "0001-fails", Run: func(_ context.Context, e migrate.Env) error {
		_ = os.WriteFile(e.Dirs.StateFile(), []byte(`{"schema_version": 1, "plugin_version": "migrated"}`), 0o600)
		holdStateLock(t, d)
		go func() {
			defer close(done)
			time.Sleep(300 * time.Millisecond)
			raw, _ := os.ReadFile(e.Dirs.StateFile())
			duringLock = string(raw)
			_ = os.Remove(filepath.Join(d.LocksDir(), "state.lock"))
		}()
		return errors.New("cannot migrate")
	}}}
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	<-done
	if code != output.RolledBack {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(duringLock, "migrated") {
		t.Errorf("state.json written while someone else held its lock: %s", duringLock)
	}
	if after := snapshot(t, d); after != before {
		t.Errorf("not rolled back:\n%s\nwas:\n%s", after, before)
	}
}

// killedAfterTheShim leaves what an update apply from 0.1.0 to 0.2.0
// killed right after its shim switch leaves: the new binary installed, its
// marker and snapshot, the shim on it, and state.json changed since (a
// migration that ran). It returns the previous binary, the new one and
// state.json as it was before the update.
func killedAfterTheShim(t *testing.T, d paths.Dirs, src string) (old, target string, beforeState []byte) {
	t.Helper()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	old = shimOf(t, d)
	target = binaryPath(d.Data, "0.2.0", sha(fakeBody("0.2.0")))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(target, fakeBody("0.2.0"), 0o755)
	saved, err := takeSnapshot(d)
	if err != nil {
		t.Fatal(err)
	}
	beforeState = saved[d.StateFile()]
	if err := state.WriteUpdateSnapshot(d, saved); err != nil {
		t.Fatal(err)
	}
	m := state.UpdateMarker{From: "0.1.0", To: "0.2.0", Previous: old, Binary: target, WayBack: old + " client install", RemoveBinary: true, StartedAt: time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)}
	if err := state.WriteUpdateMarker(d, m); err != nil {
		t.Fatal(err)
	}
	if err := pointShim(target, filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS))); err != nil {
		t.Fatal(err)
	}
	_ = state.Update(d, "test", func(s *state.State) error { s.AppliedMigrations = []string{"0001-ran"}; return nil })
	return old, target, beforeState
}

// The new binary, run on an update to itself that was killed half way,
// resumes it (migrations, plugin, server, record) instead of answering
// "up to date".
func TestUpdateApplyResumesAnUpdateToItself(t *testing.T) {
	home, claude, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	old, target, _ := killedAfterTheShim(t, d, src)
	setBuild(t, "0.2.0", "abc1234") // the shim's binary
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || env.Data.UpToDate || !env.Data.Resumed || env.Data.Previous != old || env.Data.Installed != "0.1.0" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if bins.ran("0.2.0 update migrate") != 1 || bins.ran("0.2.0 client install") != 1 || !slices.Contains(mutating(claude), "plugin update "+plugin.PluginID()) {
		t.Errorf("steps not run again: %q", bins.calls)
	}
	st, _ := state.Load(d.StateFile())
	if st.ShimTarget != target || st.Client == nil || st.Client.ShimPrevious != old || shimOf(t, d) != target {
		t.Errorf("record: %+v", st)
	}
	if state.UpdateIncomplete(d) {
		t.Error("marker left")
	}
	if _, err := os.Stat(d.UpdateSnapshot()); err == nil {
		t.Error("snapshot left")
	}
}

// A resume that fails rolls back to the marker's previous install, with
// state.json as the snapshot persisted before the update kept it.
func TestUpdateApplyResumeRollsBackToTheSnapshot(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	old, target, beforeState := killedAfterTheShim(t, d, src)
	bins.migrations["0.2.0"] = []migrate.Migration{{ID: "0002-fails", Run: func(context.Context, migrate.Env) error { return errors.New("cannot migrate") }}}
	setBuild(t, "0.2.0", "abc1234")
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	if code != output.RolledBack || !decodeApply(t, out).Data.RolledBack {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if raw, _ := os.ReadFile(d.StateFile()); string(raw) != string(beforeState) {
		t.Errorf("state.json is not the snapshot's:\n%s", raw)
	}
	if shimOf(t, d) != old || state.UpdateIncomplete(d) {
		t.Errorf("shim %s, marker %v", shimOf(t, d), state.UpdateIncomplete(d))
	}
	if _, err := os.Stat(filepath.Dir(target)); err == nil {
		t.Error("the new binary, installed by the update, was kept")
	}
}

// The previous binary on an unfinished update replaces it from the
// install it started from: the marker's previous binary (the shim runs
// the half-installed one) and the persisted snapshot (state.json changed
// since) are the way back.
func TestUpdateApplyReplacesAnUnfinishedUpdateFromItsMarker(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	old, target, beforeState := killedAfterTheShim(t, d, src)
	bins.migrations["0.2.0"] = []migrate.Migration{{ID: "0002-fails", Run: func(context.Context, migrate.Env) error { return errors.New("cannot migrate") }}}
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.RolledBack || env.Data.Previous != old || env.Data.Installed != "0.1.0" ||
		!slices.ContainsFunc(env.Data.Warnings, func(w string) bool {
			return strings.Contains(w, "did not finish") && strings.Contains(w, "this one replaces it")
		}) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if raw, _ := os.ReadFile(d.StateFile()); string(raw) != string(beforeState) {
		t.Errorf("state.json is not the snapshot's:\n%s", raw)
	}
	if shimOf(t, d) != old {
		t.Errorf("shim %s, want %s", shimOf(t, d), old)
	}
	if _, err := os.Stat(filepath.Dir(target)); err == nil {
		t.Error("the binary the unfinished update installed was kept")
	}
	// Now passing: the update replaces the unfinished one and records the
	// marker's previous binary as the one to roll back to.
	bins.migrations["0.2.0"] = nil
	if code, out, _ := run(t, "update", "apply", "--source", src, "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if st, _ := state.Load(d.StateFile()); st.Client == nil || st.Client.ShimPrevious != old {
		t.Errorf("previous %+v, want %s", st.Client, old)
	}
}

// No shim: nothing to update, the user runs setup (exit 10).
func TestUpdateApplyWithoutAnInstallNeedsSetup(t *testing.T) {
	home := devInstall(t, true)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	setBuild(t, "0.1.0", "abc1234")
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.NeedsUser || env.Error == nil || len(env.Error.Actions) != 1 || env.Error.Actions[0].Cmd != "backsight setup" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if ents, _ := os.ReadDir(filepath.Join(pathsOf(home).Data, "bin")); len(ents) != 0 {
		t.Errorf("installed something: %v", ents)
	}
}

// A child that needs the user (Claude Code too old for the new plugin):
// after the rollback, update apply exits 10 with the child's actions and
// says it rolled back.
func TestUpdateApplyPassesOnAChildsNeedsUser(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	bins.fail["0.2.0 client install"] = output.NeedsUser
	before := snapshot(t, d)
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.NeedsUser || !env.Data.RolledBack || env.Error == nil || len(env.Error.Actions) != 1 || env.Error.Actions[0].Cmd != "npm install -g @anthropic-ai/claude-code" ||
		!strings.Contains(env.Error.Message, "rolled back") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if bins.ran("0.1.0 client install") != 1 || snapshot(t, d) != before {
		t.Errorf("not rolled back: %q", bins.calls)
	}
}

// A remote server is upgraded on its host: the server step is skipped,
// and so are the bank template and the docs, plan and run (the host's
// backsight applies its own).
func TestUpdateApplySkipsARemoteServer(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	raw, _ := os.ReadFile(d.ConfigFile())
	_ = os.WriteFile(d.ConfigFile(), []byte(strings.Replace(string(raw), "[server]\n", "[server]\nmode = \"remote\"\n", 1)), 0o600)
	hostOnly := func(env applyEnvelope) bool {
		return hasApplyStep(env.Data.Steps, "bank", "skipped", "the server's host applies its bank template (backsight bank apply there)") &&
			hasApplyStep(env.Data.Steps, "docs", "skipped", "the server's host indexes its docs")
	}
	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	if env := decodeApply(t, out); code != output.OK || !hostOnly(env) {
		t.Errorf("plan: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || bins.ran("0.2.0 server") != 0 || !hostOnly(env) ||
		!slices.ContainsFunc(env.Data.Steps, func(s step) bool {
			return s.Name == "server" && s.Status == "skipped" && strings.Contains(s.Detail, "remote")
		}) {
		t.Errorf("exit %d, calls %q\n%s", code, bins.calls, out)
	}
	if bins.ran("0.2.0 bank apply") != 0 || bins.ran("0.2.0 guide index") != 0 {
		t.Errorf("a client ran the host's steps: %q", bins.calls)
	}
}

// --plan verifies like the update: a release that fails it is refused.
func TestUpdateApplyPlanRefusesAReleaseThatFailsVerification(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{tamperArchive: true})
	before := snapshot(t, d)
	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	if code != output.Blocked || !strings.Contains(out, "fails the checksum check") || len(bins.calls) != 0 || snapshot(t, d) != before {
		t.Errorf("exit %d, calls %q\n%s", code, bins.calls, out)
	}
}

// fakeSource serves one release whose assets are bodies, listed with the
// sizes given (the body's own when absent); stall makes Open's reader
// block until its context ends.
type fakeSource struct {
	rel   release.Release
	body  map[string][]byte
	stall bool
}

func (s fakeSource) Name() string { return "fake" }
func (s fakeSource) Releases(context.Context) ([]release.Release, error) {
	return []release.Release{s.rel}, nil
}
func (s fakeSource) Open(ctx context.Context, _ release.Release, name string) (io.ReadCloser, error) {
	if s.stall {
		return io.NopCloser(stalled{ctx}), nil
	}
	return io.NopCloser(bytes.NewReader(s.body[name])), nil
}

type stalled struct{ ctx context.Context }

func (s stalled) Read([]byte) (int, error) { <-s.ctx.Done(); return 0, s.ctx.Err() }

// download takes no more than the size the release lists, exactly that
// size, never more than maxAsset, and gives up at its deadline.
func TestDownloadBoundsTheAsset(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "a")
	src := func(listed int64, body string) (fakeSource, release.Release) {
		rel := release.Release{Version: "0.2.0", Assets: []release.Asset{{Name: "a", Size: listed}}}
		return fakeSource{rel: rel, body: map[string][]byte{"a": []byte(body)}}, rel
	}
	s, rel := src(5, "hello")
	if err := download(context.Background(), s, rel, "a", dst); err != nil {
		t.Fatalf("an asset of its listed size: %v", err)
	}
	if raw, _ := os.ReadFile(dst); string(raw) != "hello" {
		t.Errorf("%q", raw)
	}
	for name, c := range map[string]struct {
		listed int64
		body   string
		want   string
	}{
		"longer than listed":   {3, "hello", "larger than the 3 bytes"},
		"shorter than listed":  {9, "hello", "is 5 bytes, the release lists 9"},
		"listed above the cap": {maxAsset + 1, "x", "more than the"},
	} {
		s, rel := src(c.listed, c.body)
		if err := download(context.Background(), s, rel, "a", dst); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	old := maxAsset
	maxAsset = 4
	t.Cleanup(func() { maxAsset = old })
	if s, rel := src(0, "hello"); download(context.Background(), s, rel, "a", dst) == nil {
		t.Error("an unlisted size above the cap passed")
	}
	maxAsset = old
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s, rel = src(5, "")
	s.stall = true
	start := time.Now()
	if err := download(ctx, s, rel, "a", dst); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("a stalled download: %v after %s", err, time.Since(start))
	}
}

// The archive must hold the binary as a regular file within the size
// bound.
func TestUnpackBinaryRefusals(t *testing.T) {
	dir := t.TempDir()
	name := binaryName() // backsight, on Windows backsight.exe
	tgz := func(file string, h tar.Header, body []byte) string {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&h)
		_, _ = tw.Write(body)
		_ = tw.Close()
		_ = gz.Close()
		p := filepath.Join(dir, file+".tar.gz")
		_ = os.WriteFile(p, b.Bytes(), 0o644)
		return p
	}
	// Windows' releases are .zip archives.
	zipped := func(file, entry string, body []byte) string {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		w, _ := zw.Create(entry)
		_, _ = w.Write(body)
		_ = zw.Close()
		p := filepath.Join(dir, file+".zip")
		_ = os.WriteFile(p, b.Bytes(), 0o644)
		return p
	}
	dst := filepath.Join(dir, "out", name)
	for _, good := range []string{tgz("ok", tar.Header{Name: name, Mode: 0o755, Size: 4, Typeflag: tar.TypeReg}, []byte("body")), zipped("ok", "dist/"+name, []byte("body"))} {
		_ = os.Remove(dst)
		if err := unpackBinary(good, dst); err != nil {
			t.Fatalf("a good archive %s: %v", good, err)
		}
		if raw, _ := os.ReadFile(dst); string(raw) != "body" {
			t.Errorf("%s: %q", good, raw)
		}
	}
	for what, bad := range map[string]string{
		"no binary":     tgz("none", tar.Header{Name: "LICENSE", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg}, []byte("MIT")),
		"a symlink":     tgz("link", tar.Header{Name: name, Linkname: "/bin/sh", Typeflag: tar.TypeSymlink}, nil),
		"no binary zip": zipped("none", "LICENSE", []byte("MIT")),
		"a directory":   zipped("dir", name+"/", nil),
	} {
		if err := unpackBinary(bad, dst); err == nil || !strings.Contains(err.Error(), "no "+name) {
			t.Errorf("%s: %v", what, err)
		}
	}
	old := maxBinary
	maxBinary = 3
	t.Cleanup(func() { maxBinary = old })
	for _, big := range []string{tgz("big", tar.Header{Name: name, Mode: 0o755, Size: 4, Typeflag: tar.TypeReg}, []byte("body")), zipped("big", name, []byte("body"))} {
		if err := unpackBinary(big, dst); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("an oversized binary in %s: %v", big, err)
		}
	}
}

// With auto_apply, jobs maintain (run by the shim, the new binary)
// finishes an update to itself that did not finish.
func TestMaintainResumesAnUnfinishedUpdate(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	_, target, _ := killedAfterTheShim(t, d, src)
	configure(t, home, "[update]\nsource = "+testtool.TOMLStr(src)+"\nauto_apply = true")
	setBuild(t, "0.2.0", "abc1234")
	code, out, _ := run(t, "jobs", "maintain", "--json")
	if code != output.OK || !strings.Contains(out, `"update": "applied 0.2.0"`) || bins.ran("0.2.0 client install") != 1 {
		t.Fatalf("exit %d, calls %q\n%s", code, bins.calls, out)
	}
	if st, _ := state.Load(d.StateFile()); state.UpdateIncomplete(d) || st.ShimTarget != target {
		t.Errorf("not finished: marker %v, shim target %q", state.UpdateIncomplete(d), st.ShimTarget)
	}
}

// "Newer" is against the version the shim runs, not the binary running
// update apply (an older one left on PATH): it never downgrades the shim.
func TestUpdateApplyComparesWithTheShimsVersion(t *testing.T) {
	home, _, bins := installedAt(t, "0.2.0")
	d := pathsOf(home)
	key := trustTestKey(t)
	src := t.TempDir()
	for _, v := range []string{"0.1.5", "0.2.0"} {
		signedRelease(t, src, v, key, relOpts{})
	}
	setBuild(t, "0.1.0", "abc1234") // an older binary runs the command
	before := snapshot(t, d)
	code, out, _ := run(t, "update", "apply", "--source", src, "--version", "0.1.5", "--json")
	if env := decodeApply(t, out); code != output.Usage || env.Error == nil || !strings.Contains(env.Error.Message, "not newer than the installed 0.2.0") {
		t.Errorf("--version 0.1.5: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "update", "apply", "--source", src, "--json")
	if env := decodeApply(t, out); code != output.OK || !env.Data.UpToDate || env.Data.Installed != "0.2.0" {
		t.Errorf("newest: exit %d\n%s", code, out)
	}
	if snapshot(t, d) != before || len(bins.calls) != 0 {
		t.Errorf("changed (calls %q)", bins.calls)
	}
}

// fakeScheduler fakes the OS scheduler without touching the shim (withSys
// writes one, which would overwrite the installed binary through it).
func fakeScheduler(t *testing.T, home string) *fakeSys {
	t.Helper()
	f := &fakeSys{xdg: filepath.Join(home, "xdg")}
	oldR, oldG := sysRunner, schedulerGOOS
	sysRunner, schedulerGOOS = f, "linux"
	t.Cleanup(func() { sysRunner, schedulerGOOS = oldR, oldG })
	return f
}

func schedulerStepIn(steps []step) step {
	for _, s := range steps {
		if s.Name == "scheduler" {
			return s
		}
	}
	return step{}
}

// update apply brings the scheduler entries to the new binary's templates
// through its client install (issue 222): --plan says so and writes
// nothing; the update rewrites the stale unit and only reloads (never a
// stop or restart); a refresh that fails is a warning, never a rollback.
func TestUpdateApplyRefreshesTheSchedulerEntries(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: systemd units and their timers' links (Windows tasks: jobs_tasks_test.go)")
	home, _, bins := installedAt(t, "0.1.0")
	sys := fakeScheduler(t, home)
	if code, out, _ := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("jobs install: exit %d\n%s", code, out)
	}
	svc := filepath.Join(home, "xdg", "systemd", "user", "backsight-maintain.service")
	oldTemplate(t, svc, "KillMode=mixed\nTimeoutStopSec=45min\n")
	src := t.TempDir()
	key := trustTestKey(t)
	signedRelease(t, src, "0.2.0", key, relOpts{})

	sys.calls = nil
	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	env := decodeApply(t, out)
	if s := schedulerStepIn(env.Data.Steps); code != output.OK || s.Status != "planned" || !strings.Contains(s.Detail, svc) {
		t.Fatalf("plan: exit %d %+v\n%s", code, s, out)
	}
	if strings.Contains(readFile(t, svc), "KillMode") || len(mutatingSys(sys)) != 0 || bins.ran("0.2.0 jobs refresh --plan") != 1 {
		t.Fatalf("the plan changed the unit or ran %q / %q", sys.calls, bins.calls)
	}

	sys.calls, bins.calls = nil, nil
	code, out, _ = run(t, "update", "apply", "--source", src, "--json")
	env = decodeApply(t, out)
	if s := schedulerStepIn(env.Data.Steps); code != output.OK || s.Status != "done" || !strings.Contains(readFile(t, svc), "KillMode=mixed\nTimeoutStopSec=45min\n") {
		t.Fatalf("apply: exit %d %+v\n%s", code, s, out)
	}
	for _, c := range sys.calls {
		if strings.Contains(c, "restart") || strings.Contains(c, " stop") || strings.Contains(c, ".service") {
			t.Errorf("a running job would be interrupted: %s", c)
		}
	}
	// The new binary's client install refreshes the entries; update apply
	// does not run a second jobs refresh of its own.
	reloads := 0
	for _, c := range sys.calls {
		if c == "systemctl --user daemon-reload" {
			reloads++
		}
	}
	if reloads != 1 || bins.ran("0.2.0 jobs refresh") != 0 {
		t.Errorf("refreshed %d times, %d jobs refresh runs: %q / %q", reloads, bins.ran("0.2.0 jobs refresh"), sys.calls, bins.calls)
	}

	// The next release finds entries it cannot refresh in place: the
	// update still passes, with a warning.
	signedRelease(t, src, "0.3.0", key, relOpts{})
	setBuild(t, "0.2.0", "abc1234")
	sys.xdg = filepath.Join(home, "elsewhere")
	code, out, _ = run(t, "update", "apply", "--source", src, "--json")
	env = decodeApply(t, out)
	if s := schedulerStepIn(env.Data.Steps); code != output.OK || env.Data.RolledBack || s.Status != "failed" ||
		!slices.ContainsFunc(env.Data.Warnings, func(w string) bool { return strings.Contains(w, "scheduler") }) {
		t.Errorf("failed refresh: exit %d %+v\n%s", code, s, out)
	}
}

// Without Claude Code wired there is no client install: the new binary's
// own jobs refresh runs.
func TestUpdateApplyRefreshesTheSchedulerWithoutAClient(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: systemd units and their timers' links (Windows tasks: jobs_tasks_test.go)")
	home, _, bins := installedAt(t, "0.1.0")
	fakeScheduler(t, home)
	run(t, "jobs", "install")
	d := pathsOf(home)
	_ = state.Update(d, "test", func(s *state.State) error { s.PluginVersion = ""; return nil })
	svc := filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.service")
	oldTemplate(t, svc, "KillMode=mixed\n")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if s := schedulerStepIn(env.Data.Steps); code != output.OK || s.Status != "done" || bins.ran("0.2.0 jobs refresh") != 1 || !strings.Contains(readFile(t, svc), "KillMode=mixed") {
		t.Errorf("exit %d %+v calls %q\n%s", code, s, bins.calls, out)
	}
}

// update apply's rule for the Windows supervisor (decision 0080,
// "Updates"): restarted on another protocol, or when it still runs the old
// backsight after a server upgrade; else kept, and the step says so.
func TestSupervisorRestartRule(t *testing.T) {
	st := supervise.Status{Proto: 1, Version: "0.1.0"}
	for _, c := range []struct {
		name     string
		st       supervise.Status
		proto    int
		upgraded bool
		restart  bool
		why      string
	}{
		{"same protocol", st, 1, false, false, "the server keeps running backsight 0.1.0 until its next restart"},
		{"another protocol", st, 2, false, true, "restarted: it spoke protocol 1, backsight 0.2.0 speaks 2"},
		{"a binary that does not say", st, 0, false, true, "restarted: it spoke protocol 1, backsight 0.2.0 speaks 0"},
		{"upgraded, still the old one", st, 1, true, true, "restarted: it still ran backsight 0.1.0 after the server upgrade"},
		{"upgraded, restarted by it", supervise.Status{Proto: 1, Version: "0.2.0"}, 1, true, false, "the server upgrade restarted it: it runs backsight 0.2.0"},
		{"upgraded, restarted by it, v-prefixed", supervise.Status{Proto: 1, Version: "v0.2.0"}, 1, true, false, "the server upgrade restarted it: it runs backsight 0.2.0"},
	} {
		restart, why := supervisorRestart(c.st, c.proto, c.upgraded, "0.2.0")
		if restart != c.restart || why != c.why {
			t.Errorf("%s: %v %q, want %v %q", c.name, restart, why, c.restart, c.why)
		}
	}
}

// On Linux update apply never asks a supervisor: the units run no
// backsight.
func TestUpdateApplyAsksNoSupervisorOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows asks it (update_apply_windows_test.go)")
	}
	home, _, _ := installedAt(t, "0.1.0")
	raw, _ := os.ReadFile(pathsOf(home).ConfigFile())
	raw, _ = config.SetRuntime(raw, "native")
	_ = os.WriteFile(pathsOf(home).ConfigFile(), raw, 0o600)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	code, out, _ := run(t, "update", "apply", "--source", src, "--json") // TestMain refuses the pipe
	env := decodeApply(t, out)
	if code != output.OK || applyStepIndex(env.Data.Steps, "supervisor") >= 0 {
		t.Errorf("exit %d\n%s", code, out)
	}
}
