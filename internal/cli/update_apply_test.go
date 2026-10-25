package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/migrate"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/selfcheck"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/internal/state"
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
}

func withBinaries(t *testing.T, d paths.Dirs) *fakeBinaries {
	t.Helper()
	f := &fakeBinaries{t: t, migrations: map[string][]migrate.Migration{}, fail: map[string]int{}, dirs: d, claimVersion: map[string]string{}, images: map[string]string{}}
	old := binaryExec
	binaryExec = f
	t.Cleanup(func() { binaryExec = old })
	return f
}

func (f *fakeBinaries) Run(ctx context.Context, exe string, args ...string) ([]byte, []byte, int, error) {
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
			_ = output.WriteJSON(&out, "test", nil, &output.Error{Code: code, Message: "failing on purpose: " + prefix})
			return out.Bytes(), []byte("progress\n"), code, nil
		}
	}
	oldV, oldExe, oldMig, oldImg := buildinfo.Version, selfExe, migrations, server.HindsightImage
	if img, ok := f.images[v]; ok {
		server.HindsightImage = img
	}
	buildinfo.Version = v
	if c, ok := f.claimVersion[v]; ok {
		buildinfo.Version = c
	}
	selfExe = func() (string, error) { return exe, nil }
	migrations = func() []migrate.Migration { return f.migrations[v] }
	defer func() { buildinfo.Version, selfExe, migrations, server.HindsightImage = oldV, oldExe, oldMig, oldImg }()
	var out, errb bytes.Buffer
	code := Execute(args, &out, &errb)
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
	key           *minisign.PrivateKey // sign with this key instead
	tamperArchive bool                 // the archive differs from its checksum
	tamperBinary  bool                 // the archive's binary differs from the bare one's checksum
	noBareBinary  bool                 // checksums.txt lists no bare binary
	body          []byte               // the binary, default fakeBody(v)
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// signedRelease writes the goreleaser release of version v under
// root/v<v>: this platform's archive, the bare binary, checksums.txt and
// its signature.
func signedRelease(t *testing.T, root, v string, key minisign.PrivateKey, o relOpts) {
	t.Helper()
	dir := filepath.Join(root, "v"+v)
	_ = os.MkdirAll(dir, 0o755)
	body := o.body
	if body == nil {
		body = fakeBody(v)
	}
	inArchive := body
	if o.tamperBinary {
		inArchive = append(slices.Clone(body), []byte("# tampered\n")...)
	}
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	for name, b := range map[string][]byte{"LICENSE": []byte("MIT"), "backsight": inArchive} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(b)
	}
	_ = tw.Close()
	_ = gz.Close()
	archive := "backsight_" + v + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	bare := selfcheck.BinaryName(v, runtime.GOOS, runtime.GOARCH)
	sums := sha(tgz.Bytes()) + "  " + archive + "\n"
	if !o.noBareBinary {
		sums += sha(body) + "  " + bare + "\n"
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
		r := minisign.NewReader(strings.NewReader(sums))
		_, _ = io.Copy(io.Discard, r)
		files["checksums.txt.minisig"] = r.SignWithComments(k, "timestamp:1\tfile:checksums.txt\thashed", "x")
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
	f.marketplaces = `[{"name":"backsight","path":"` + filepath.Join(d.Data, "marketplace") + `"}]`
	f.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	return f, withBinaries(t, d)
}

func shimOf(t *testing.T, d paths.Dirs) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS)))
	if err != nil {
		t.Fatal(err)
	}
	return got
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
		} `json:"server"`
		RolledBack bool   `json:"rolled_back"`
		Steps      []step `json:"steps"`
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
	shim, _ := filepath.EvalSymlinks(filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS)))
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
	for i, held := range bins.lockHeld {
		if !held {
			t.Errorf("%s ran without update apply's maintenance lock", bins.calls[i])
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
	for name, o := range map[string]relOpts{
		"unsigned":         {unsigned: true},
		"untrusted key":    {key: &otherPriv},
		"tampered archive": {tamperArchive: true},
		"tampered binary":  {tamperBinary: true},
	} {
		t.Run(name, func(t *testing.T) {
			home, claude, bins := installedAt(t, "0.1.0")
			d := pathsOf(home)
			src := t.TempDir()
			signedRelease(t, src, "0.2.0", trustTestKey(t), o)
			before := snapshot(t, d)
			code, out, _ := run(t, "update", "apply", "--source", src, "--json")
			env := decodeApply(t, out)
			if code != output.Blocked || env.OK || env.Error == nil || !strings.Contains(env.Error.Message, "nothing was changed") {
				t.Fatalf("exit %d\n%s", code, out)
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
	bins.fail["0.2.0 client install"] = output.Partial
	bins.fail["0.1.0 client install"] = output.Blocked
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, old+" client install") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	m, err := state.ReadUpdateMarker(d)
	if err != nil || m == nil || m.Previous != old || m.To != "0.2.0" {
		t.Fatalf("marker %+v %v", m, err)
	}
	rep := doctorReport(t)
	c := findCheck(rep, "update-incomplete")
	if c == nil || c.Status != doctor.Error || !strings.Contains(c.Fix, old+" client install") {
		t.Errorf("doctor %+v", c)
	}
}

func TestUpdateApplyRefusesWhileMaintenanceRuns(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	// A live owner: this test's parent process.
	raw, _ := json.Marshal(state.Owner{PID: os.Getppid(), Command: "backup create"})
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
	configure(t, home, "[update]\nsource = \""+src+"\"")
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
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	exe := filepath.Join(t.TempDir(), "child")
	_ = os.WriteFile(exe, []byte("#!/bin/sh\necho \"$@\"\necho progress >&2\nexit 30\n"), 0o755)
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
	dk.failImage = newImage
	old := shimOf(t, d)
	beforeState, _ := state.Load(d.StateFile())
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	if code != output.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if deployedImage(t, home) != oldImage || dk.rows != 25 || !dk.running || len(dk.restores) != 1 {
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
	if deployedImage(t, home) != oldImage || dk.rows != 10 || !dk.running {
		t.Errorf("image %s rows %d (want the archive's 10) running %v", deployedImage(t, home), dk.rows, dk.running)
	}
	if state.RestoreIncomplete(d) {
		t.Error("marker left")
	}
}
