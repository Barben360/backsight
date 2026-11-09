package ledgerdb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/sqlitedb"
	"github.com/Barben360/backsight/internal/state"
)

func mustMigrate(t *testing.T, o Options, plan bool) MigrateResult {
	t.Helper()
	res, err := Migrate(context.Background(), o, plan)
	if err != nil {
		t.Fatalf("Migrate(plan %v): %v", plan, err)
	}
	return res
}

func sameMigrate(t *testing.T, what string, got, want MigrateResult) {
	t.Helper()
	if got.Built != want.Built || got.Newer != want.Newer || got.File != want.File ||
		!slices.Equal(got.Pending, want.Pending) || !slices.Equal(got.Applied, want.Applied) {
		t.Errorf("%s: %+v, want %+v", what, got, want)
	}
}

// journalTwoSettles journals two settles and a hide: what a build folds.
func journalTwoSettles(t *testing.T, o Options) {
	t.Helper()
	appendLines(t, o,
		settleOf(nov(10), show("s1|p1|1|m1", "m1", nov(9), feedback.OutcomeCredit), show("s1|p1|1|m2", "m2", nov(9), feedback.OutcomeMiss)),
		settleOf(nov(20), show("s1|p2|2|m1", "m1", nov(19), feedback.OutcomeShown)),
		hideOf(nov(21), "m2"),
	)
}

// TestMigrateBuildsAndPlans: --plan on a missing file lists the initial
// migration as pending and creates nothing; without it, Migrate builds
// the file from the journal, equal to a rebuild of it.
func TestMigrateBuildsAndPlans(t *testing.T) {
	o := tempOpts(t)
	journalTwoSettles(t, o)
	before, err := os.ReadDir(feedbackDir(o))
	if err != nil {
		t.Fatal(err)
	}

	sameMigrate(t, "plan, no file", mustMigrate(t, o, true),
		MigrateResult{File: FileName, Pending: []string{"0001-initial"}, Applied: []string{}})
	absent(t, mainPath(o))
	if after, _ := os.ReadDir(feedbackDir(o)); len(after) != len(before) {
		t.Errorf("a plan created files: %v, before %v", after, before)
	}

	sameMigrate(t, "build", mustMigrate(t, o, false),
		MigrateResult{File: FileName, Pending: []string{"0001-initial"}, Applied: []string{"0001-initial"}})
	if v := headerVersion(t, mainPath(o)); v != 1 {
		t.Errorf("user_version %d after Migrate, want 1", v)
	}
	matchesOracle(t, "after Migrate", o, feedback.Params{})
	sameLedger(t, "the reader after Migrate", readerAll(t, o), oracle(t, feedbackDir(o), feedback.Params{}))

	upToDate := MigrateResult{Built: true, File: FileName, Pending: []string{}, Applied: []string{}}
	sameMigrate(t, "plan, at the head", mustMigrate(t, o, true), upToDate)
	gen := metaInt(t, o, "generation")
	sameMigrate(t, "again, at the head", mustMigrate(t, o, false), upToDate)
	if got := metaInt(t, o, "generation"); got != gen {
		t.Errorf("a Migrate at the head folded: generation %d, want %d", got, gen)
	}
}

// A pending migration is listed by the plan and applied by Migrate, the
// rows kept; during an uncommitted update Migrate does nothing.
func TestMigrateAppliesAPendingMigration(t *testing.T) {
	o := tempOpts(t)
	journalTwoSettles(t, o)
	mustCloseWriter(t, mustOpenWriter(t, o))
	o.Migrations = withTestMigration(t)
	next := "0002-test"

	sameMigrate(t, "plan", mustMigrate(t, o, true),
		MigrateResult{Built: true, File: FileName, Pending: []string{next}, Applied: []string{}})
	if v := headerVersion(t, mainPath(o)); v != 1 {
		t.Errorf("user_version %d after a plan, want 1", v)
	}

	if err := state.WriteUpdateMarker(o.Dirs, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(context.Background(), o, false); !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("Migrate during an update: %v, want ErrUpdateInProgress", err)
	}
	if v := headerVersion(t, mainPath(o)); v != 1 {
		t.Errorf("user_version %d after a refused Migrate, want 1", v)
	}
	if err := state.WriteUpdateMarker(o.Dirs, state.UpdateMarker{From: "v1", To: "v2", Committed: true}); err != nil {
		t.Fatal(err)
	}

	sameMigrate(t, "migrate", mustMigrate(t, o, false),
		MigrateResult{Built: true, File: FileName, Pending: []string{next}, Applied: []string{next}})
	if v := headerVersion(t, mainPath(o)); v != 2 {
		t.Errorf("user_version %d after Migrate, want 2", v)
	}
	matchesOracle(t, "the rows after the migration", o, feedback.Params{})
}

// A ledger.db a newer binary migrated is left as it is: Migrate builds
// and uses this binary's side file.
func TestMigrateLeavesANewerFile(t *testing.T) {
	forceFS(t, sqlitedb.FSLocal)
	o := tempOpts(t)
	journalTwoSettles(t, o)
	o.Migrations = withTestMigration(t)
	mustCloseWriter(t, mustOpenWriter(t, o))
	o.Migrations = nil
	before := readBytes(t, mainPath(o))
	side := SideFileName(1)

	sameMigrate(t, "plan", mustMigrate(t, o, true),
		MigrateResult{Newer: true, File: side, Pending: []string{"0001-initial"}, Applied: []string{}})
	absent(t, filepath.Join(feedbackDir(o), side))
	sameMigrate(t, "migrate", mustMigrate(t, o, false),
		MigrateResult{Newer: true, File: side, Pending: []string{"0001-initial"}, Applied: []string{"0001-initial"}})
	if v := headerVersion(t, filepath.Join(feedbackDir(o), side)); v != 1 {
		t.Errorf("%s at user_version %d, want 1", side, v)
	}
	if got := readBytes(t, mainPath(o)); !bytes.Equal(got, before) {
		t.Error("the newer ledger.db changed")
	}
}
