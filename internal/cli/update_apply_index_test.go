package cli

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// indexUserVersion is metrics.db's schema version, read without opening
// the index (which would migrate it).
func indexUserVersion(t *testing.T, d paths.Dirs) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(d.IndexDir(), index.FileName)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// indexAtSchema1 builds metrics.db with this binary's registry (head 1)
// and makes "0.2.0" a binary of head 2 (0002-test).
func indexAtSchema1(t *testing.T, home string, bins *fakeBinaries) {
	t.Helper()
	catchUpIndex(t, home)
	if v := indexUserVersion(t, pathsOf(home)); v != 1 {
		t.Fatalf("fixture at schema %d", v)
	}
	bins.indexes["0.2.0"] = testRegistry(t, "CREATE TABLE added(x)")
}

// applyIndexData is the update result's index_migrations.
func applyIndexData(t *testing.T, out string) []string {
	t.Helper()
	var r struct {
		IndexMigrations []string `json:"index_migrations"`
	}
	jsonData(t, out, &r)
	return r.IndexMigrations
}

func applyStep(steps []step, name string) (int, step) {
	for i, s := range steps {
		if s.Name == name {
			return i, s
		}
	}
	return -1, step{}
}

const indexWarningFailed = "the metrics index was not migrated (failed): the next jobs tick retries"

func TestUpdateApplyMigratesTheIndexAfterTheCommitPoint(t *testing.T) {
	home, _, _, _, bins, _ := serverInstalledAt(t, "0.1.0", "0.2.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)
	var committed *bool
	bins.before = func(_ context.Context, line string) {
		if strings.HasPrefix(line, "0.2.0 stats index migrate") {
			m, err := state.ReadUpdateMarker(d)
			c := err == nil && m != nil && m.Committed
			committed = &c
		}
	}

	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || env.Data.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if v := indexUserVersion(t, d); v != 2 {
		t.Errorf("user_version %d", v)
	}
	if bins.ran("0.2.0 stats index migrate") != 1 {
		t.Errorf("calls %q", bins.calls)
	}
	if committed == nil || !*committed {
		t.Errorf("the index migration ran before the commit point (committed %v)", committed)
	}
	iServer, _ := applyStep(env.Data.Steps, "server")
	iDocs, _ := applyStep(env.Data.Steps, "docs")
	iIndex, st := applyStep(env.Data.Steps, "index")
	if st.Status != "done" || st.Detail != "migrated the metrics index (1 change)" || iServer < 0 || iDocs < 0 || iIndex < iServer || iIndex < iDocs {
		t.Errorf("steps %+v", env.Data.Steps)
	}
	if got := applyIndexData(t, out); !slices.Equal(got, []string{"0002-test"}) {
		t.Errorf("index_migrations %q", got)
	}
	if slices.Contains(env.Data.Warnings, indexWarningFailed) {
		t.Errorf("warnings %q", env.Data.Warnings)
	}
}

// The text says how many migrations ran, never their ids (they read like
// the design-record numbers user-visible text never shows).
func TestUpdateApplyIndexTextNamesNoMigrationID(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)
	code, out, errOut := run(t, "update", "apply", "--source", src)
	if code != output.OK || !strings.Contains(out, "done     index: migrated the metrics index (1 change)\n") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if strings.Contains(out+errOut, "0002") {
		t.Errorf("a migration id in the text:\n%s%s", out, errOut)
	}
	if v := indexUserVersion(t, pathsOf(home)); v != 2 {
		t.Errorf("user_version %d", v)
	}
}

func TestUpdateApplyKeepsTheUpdateWhenTheIndexMigrationFails(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)
	bins.fail["0.2.0 stats index migrate"] = output.Blocked
	bins.failStderr = "STDERR-CANARY /home/someone/secret/metrics.db\n"

	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || !env.OK || env.Data.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.HasPrefix(shimOf(t, d), filepath.Join(d.Data, "bin", "0.2.0-")) {
		t.Errorf("shim %s", shimOf(t, d))
	}
	if st, _ := state.Load(d.StateFile()); !strings.HasPrefix(st.ShimTarget, filepath.Join(d.Data, "bin", "0.2.0-")) {
		t.Errorf("not recorded: %q", st.ShimTarget)
	}
	if !slices.Contains(env.Data.Warnings, indexWarningFailed) {
		t.Errorf("warnings %q", env.Data.Warnings)
	}
	if _, st := applyStep(env.Data.Steps, "index"); st.Status != "failed" || st.Detail != "failed" {
		t.Errorf("steps %+v", env.Data.Steps)
	}
	// neither the child's stderr nor its envelope's text is relayed
	for _, canary := range []string{"STDERR-CANARY", "secret", "failing on purpose"} {
		if strings.Contains(out+errOut, canary) {
			t.Errorf("%q relayed:\n%s%s", canary, out, errOut)
		}
	}
	if v := indexUserVersion(t, d); v != 1 {
		t.Errorf("user_version %d", v)
	}
}

// The warning says why in fixed words: an index in use, not the child's
// own sentence.
func TestUpdateApplyIndexWarningSaysWhyInFixedWords(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)
	old := indexMigrate
	indexMigrate = func(context.Context, index.Options, bool) (index.MigrateResult, error) {
		return index.MigrateResult{}, index.ErrInUse
	}
	t.Cleanup(func() { indexMigrate = old })
	code, out, errOut := run(t, "update", "apply", "--source", src)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "warning  the metrics index was not migrated (the index is in use): the next jobs tick retries\n") ||
		!strings.Contains(out, "failed   index: the index is in use\n") {
		t.Errorf("text:\n%s", out)
	}
	if strings.Contains(out+errOut, "close the dashboard") {
		t.Errorf("the child's sentence relayed:\n%s%s", out, errOut)
	}
}

func TestUpdateApplyRollbackNeverMeetsAMigratedIndex(t *testing.T) {
	home, _, dk, _, bins, _ := serverInstalledAt(t, "0.1.0", "0.2.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)
	dk.failImage, dk.migrateImage = newImage, newImage
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.RolledBack || !env.Data.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if bins.ran("0.2.0 stats index migrate") != 0 {
		t.Errorf("calls %q", bins.calls)
	}
	if v := indexUserVersion(t, d); v != 1 {
		t.Errorf("user_version %d after a rollback", v)
	}
	if i, _ := applyStep(env.Data.Steps, "index"); i >= 0 {
		t.Errorf("steps %+v", env.Data.Steps)
	}
}

func TestUpdateApplyPlanListsPendingIndexMigrations(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)

	code, out, errOut := run(t, "update", "apply", "--plan", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || !env.Data.Plan {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	iSched, _ := applyStep(env.Data.Steps, "scheduler")
	iIndex, st := applyStep(env.Data.Steps, "index")
	if st.Status != "planned" || st.Detail != "would migrate the metrics index (1 change)" || iIndex < iSched {
		t.Errorf("steps %+v", env.Data.Steps)
	}
	if got := applyIndexData(t, out); !slices.Equal(got, []string{"0002-test"}) {
		t.Errorf("index_migrations %q", got)
	}
	if v := indexUserVersion(t, d); v != 1 {
		t.Errorf("user_version %d after a plan", v)
	}
	if bins.ran("0.2.0 stats index migrate --plan") != 1 || bins.ran("0.2.0 stats index migrate") != 1 {
		t.Errorf("calls %q", bins.calls)
	}

	code, out, errOut = run(t, "update", "apply", "--plan", "--source", src)
	if code != output.OK || !strings.Contains(out, "planned  index: would migrate the metrics index (1 change)\n") || strings.Contains(out+errOut, "0002") {
		t.Errorf("plan text: exit %d\n%s%s", code, out, errOut)
	}
}

func TestUpdateApplyWithoutAnIndex(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	bins.indexes["0.2.0"] = testRegistry(t, "CREATE TABLE added(x)")
	db := filepath.Join(d.IndexDir(), index.FileName)

	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	env := decodeApply(t, out)
	if _, st := applyStep(env.Data.Steps, "index"); code != output.OK || st.Status != "skipped" || st.Detail != "no metrics index yet" {
		t.Errorf("plan: exit %d steps %+v", code, env.Data.Steps)
	}

	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	env = decodeApply(t, out)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if _, st := applyStep(env.Data.Steps, "index"); st.Status != "skipped" || st.Detail != "no metrics index yet" {
		t.Errorf("steps %+v", env.Data.Steps)
	}
	if bins.ran("0.2.0 stats index migrate --json") != 1 {
		t.Errorf("calls %q", bins.calls)
	}
	if _, err := os.Stat(db); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the update created %s: %v", db, err)
	}
	if len(applyIndexData(t, out)) != 0 {
		t.Errorf("index_migrations in %s", out)
	}
}

func TestUpdateApplyIndexUpToDate(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	catchUpIndex(t, home) // 0.2.0 has this binary's registry: head 1
	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	if _, st := applyStep(decodeApply(t, out).Data.Steps, "index"); code != output.OK || st.Status != "skipped" || st.Detail != "the metrics index is up to date" {
		t.Errorf("plan: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "update", "apply", "--source", src, "--json")
	if _, st := applyStep(decodeApply(t, out).Data.Steps, "index"); code != output.OK || st.Status != "done" || st.Detail != "the metrics index is up to date" {
		t.Errorf("exit %d\n%s", code, out)
	}
	if bins.ran("0.2.0 stats index migrate") != 2 {
		t.Errorf("calls %q", bins.calls)
	}
}

func TestUpdateApplyIndexNewerThanTheNewBinary(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	withRegistry(t, testRegistry(t, "CREATE TABLE later(x)")) // a newer backsight built it
	catchUpIndex(t, home)
	bins.indexes["0.2.0"] = index.Embedded
	before, _ := os.ReadFile(filepath.Join(d.IndexDir(), index.FileName))
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	if _, st := applyStep(decodeApply(t, out).Data.Steps, "index"); code != output.OK || st.Status != "skipped" || st.Detail != "the metrics index is newer than this backsight" {
		t.Errorf("exit %d\n%s", code, out)
	}
	if after, _ := os.ReadFile(filepath.Join(d.IndexDir(), index.FileName)); string(after) != string(before) {
		t.Error("the newer metrics.db changed")
	}
}

// A new binary without the command (exit 2) has no index to migrate.
func TestUpdateApplyIndexWithABinaryWithoutIt(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)
	bins.fail["0.2.0 stats index migrate"] = output.Usage
	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	if _, st := applyStep(decodeApply(t, out).Data.Steps, "index"); code != output.OK || st.Status != "skipped" || st.Detail != "the new binary has no metrics index" {
		t.Errorf("plan: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if _, st := applyStep(env.Data.Steps, "index"); code != output.OK || st.Status != "skipped" || st.Detail != "the new binary has no metrics index" || strings.Contains(strings.Join(env.Data.Warnings, "\n"), "metrics index") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A plan never fails on the index: the new binary's failure to list its
// migrations is a skipped step, in fixed words.
func TestUpdateApplyPlanSkipsAnIndexItCannotList(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)
	bins.fail["0.2.0 stats index migrate --plan"] = output.Blocked
	bins.failStderr = "STDERR-CANARY\n"
	code, out, errOut := run(t, "update", "apply", "--plan", "--source", src, "--json")
	env := decodeApply(t, out)
	if _, st := applyStep(env.Data.Steps, "index"); code != output.OK || st.Status != "skipped" || st.Detail != "the new binary cannot list the metrics index migrations (failed)" {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if strings.Contains(out+errOut, "STDERR-CANARY") || strings.Contains(out+errOut, "failing on purpose") {
		t.Errorf("child text relayed:\n%s%s", out, errOut)
	}
}

// update apply bounds the child; one past the bound is a warning that says
// so.
func TestUpdateApplyIndexMigrationIsBounded(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)
	old := indexMigrateWait
	t.Cleanup(func() { indexMigrateWait = old })
	if old != 60*time.Second {
		t.Fatalf("bound %v", old)
	}
	indexMigrateWait = time.Nanosecond
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || !slices.Contains(env.Data.Warnings, "the metrics index was not migrated (timed out): the next jobs tick retries") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if _, st := applyStep(env.Data.Steps, "index"); st.Status != "failed" || st.Detail != "timed out" {
		t.Errorf("steps %+v", env.Data.Steps)
	}
}

// The deadline the child gets is update apply's bound.
func TestUpdateApplyIndexMigrationGetsTheBound(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	indexAtSchema1(t, home, bins)
	old := indexMigrateWait
	t.Cleanup(func() { indexMigrateWait = old })
	indexMigrateWait = 7 * time.Second
	var left time.Duration
	var has bool
	bins.before = func(ctx context.Context, line string) {
		if strings.HasPrefix(line, "0.2.0 stats index migrate") {
			var dl time.Time
			dl, has = ctx.Deadline()
			left = time.Until(dl)
		}
	}
	if code, out, _ := run(t, "update", "apply", "--source", src, "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !has || left <= 5*time.Second || left > 7*time.Second {
		t.Errorf("deadline %v (set %v)", left, has)
	}
}

// Each failure stats index migrate reports maps to its fixed phrase;
// anything else is "failed", never the child's text.
func TestIndexMigrateReason(t *testing.T) {
	envOf := func(err error) error {
		var oe *output.Error
		if !errors.As(indexError(err), &oe) {
			t.Fatalf("%v: not an output.Error", err)
		}
		return &childError{code: oe.Code, env: oe, msg: oe.Message}
	}
	bg := context.Background()
	cases := []struct {
		err  error
		want string
	}{
		{envOf(index.ErrUpdateInProgress), "an update is in progress"},
		{envOf(index.ErrInUse), "the index is in use"},
		{envOf(index.ErrDiskFull), "the disk is full"},
		{envOf(index.ErrUnreadable), "the index is unreadable"},
		{envOf(&fs.PathError{Op: "open", Path: "/x", Err: fs.ErrPermission}), "a disk error"},
		{envOf(errors.New("CANARY other")), "failed"},
		{&childError{code: output.Blocked, msg: "CANARY stderr tail"}, "failed"},
		{&childError{code: output.Blocked, env: &output.Error{Code: output.Blocked, Message: "CANARY sentence"}, msg: "CANARY sentence"}, "failed"},
		{errors.New("CANARY exec failure"), "failed"},
	}
	for _, c := range cases {
		if got := indexMigrateReason(bg, bg, c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
	done, cancel := context.WithCancel(bg)
	cancel()
	if got := indexMigrateReason(bg, done, errors.New("x")); got != "timed out" {
		t.Errorf("past the bound: %q", got)
	}
	if got := indexMigrateReason(done, done, errors.New("x")); got != "interrupted" {
		t.Errorf("interrupted: %q", got)
	}
}
