package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The ids of the chain's fixture: ledger.db credits m-db and hides h-db,
// ledger.json (written by hand after the build) credits m-json and hides
// h-json, so every answer says which file it came from.
var chainIDs = []string{"m-db", "h-db", "m-json", "h-json"}

// chainHome is a dev install whose ledger.db and ledger.json hold the
// chain's fixture, and its config.
func chainHome(t *testing.T) (string, config.Config) {
	t.Helper()
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	cfg, _, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := pathsOf(home)
	dir := d.FeedbackDir()
	at := time.Now().UTC().Add(-time.Hour)
	j := feedback.NewJournal(dir, feedback.Common{Session: "s0"})
	var settled []feedback.Settled
	for i := range 10 {
		settled = append(settled, feedback.Settled{Key: fmt.Sprintf("k%d", i), ID: "m-db", At: at, Outcome: feedback.OutcomeCredit})
	}
	if err := j.Append(feedback.Event{Kind: "settle", TS: at, Settled: settled}); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendFolded(feedback.Event{Kind: feedback.KindHide, IDs: []string{"h-db"}, Verdict: "wrong"}); err != nil {
		t.Fatal(err)
	}
	buildLedgerDB(t, home)
	// ledger.json as exported (its watermark and journal position: no
	// tail), its memories replaced.
	l, err := feedback.LoadLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Memories["m-db"]; !ok {
		t.Fatalf("the build exported no m-db: %+v", l.Memories)
	}
	l.Memories = map[string]feedback.Entry{
		"m-json": {Shown: 10, Credits: 10, CreditsAt: at, LastShown: at, LastCredited: at},
		"h-json": {Shown: 1, LastShown: at, Hidden: &feedback.Hidden{Since: at, Verdict: "wrong"}},
	}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	writeLedger(t, home, string(raw))
	return home, cfg
}

// buildLedgerDB builds home's ledger.db from its journal and exports
// ledger.json, as the first tick does.
func buildLedgerDB(t *testing.T, home string) {
	t.Helper()
	w, err := ledgerdb.OpenWriter(context.Background(), ledgerOptions(pathsOf(home), home))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Export(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func ledgerDBPath(home string) string {
	return filepath.Join(pathsOf(home).FeedbackDir(), ledgerdb.FileName)
}

// removeLedgerDB removes home's ledger.db and its companions.
func removeLedgerDB(t *testing.T, home string) {
	t.Helper()
	for _, sfx := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(ledgerDBPath(home) + sfx); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

// execLedgerDB runs statements on home's ledger.db, read-write.
func execLedgerDB(t *testing.T, home string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+ledgerDBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// holdLedgerDB turns home's ledger.db to the rollback journal (as on a
// network folder) and holds it in BEGIN EXCLUSIVE until the test ends:
// no reader gets in.
func holdLedgerDB(t *testing.T, home string) {
	t.Helper()
	execLedgerDB(t, home, "PRAGMA journal_mode=DELETE")
	holder, err := sql.Open("sqlite", "file:"+ledgerDBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		_ = conn.Close()
		_ = holder.Close()
	})
}

// adjustOf is what ledgerAdjust answers for the chain's ids, its
// failures logged to home's telemetry.
func adjustOf(t *testing.T, home string, cfg config.Config) map[string]recall.Adjustment {
	t.Helper()
	d := pathsOf(home)
	adjust := ledgerAdjust(d, cfg, time.Now(), telemetry.New(d.State, telemetry.Common{}))
	if adjust == nil {
		t.Fatal("no Adjust with feedback on")
	}
	return adjust(chainIDs)
}

// fromFile: got is the fixture's file of that name, and only it.
func fromFile(t *testing.T, got map[string]recall.Adjustment, file string) {
	t.Helper()
	other := map[string]string{"db": "json", "json": "db"}[file]
	if got["m-"+file].Boost <= 0 || got["m-"+file].Hidden || !got["h-"+file].Hidden {
		t.Errorf("m-%s %+v, h-%s %+v: want the boost and the hiding of the %s file", file, got["m-"+file], file, got["h-"+file], file)
	}
	if got["m-"+other] != (recall.Adjustment{}) || got["h-"+other] != (recall.Adjustment{}) {
		t.Errorf("m-%s %+v, h-%s %+v: want nothing of the %s file", other, got["m-"+other], other, got["h-"+other], other)
	}
}

// TestHookLedgerFallbackChain: recall reads ledger.db; when it cannot,
// ledger.json, with one feedback_error naming why; when that fails too,
// no learned ranking, with one more.
func TestHookLedgerFallbackChain(t *testing.T) {
	for _, c := range []struct {
		name    string
		break_  func(t *testing.T, home string)
		from    string // "db", "json", or "": no learned ranking
		reasons []string
	}{
		{"good", func(*testing.T, string) {}, "db", nil},
		{"missing", removeLedgerDB, "json", []string{telemetry.FeedbackReasonLedgerMissing}},
		{"garbage", func(t *testing.T, home string) {
			removeLedgerDB(t, home)
			if err := os.WriteFile(ledgerDBPath(home), []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "json", []string{telemetry.FeedbackReasonLedgerUnreadable}},
		{"user_version 2", func(t *testing.T, home string) { execLedgerDB(t, home, "PRAGMA user_version = 2") },
			"json", []string{telemetry.FeedbackReasonLedgerSchema}},
		{"held by a writer", holdLedgerDB, "json", []string{telemetry.FeedbackReasonLedgerBusy}},
		{"missing, with a garbage ledger.json", func(t *testing.T, home string) {
			removeLedgerDB(t, home)
			writeLedger(t, home, "{not json")
		}, "", []string{telemetry.FeedbackReasonLedgerMissing, telemetry.FeedbackReasonLedgerJSON}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, cfg := chainHome(t)
			c.break_(t, home)
			got := adjustOf(t, home, cfg)
			if c.from == "" {
				if got != nil {
					t.Errorf("a learned ranking without a ledger: %+v", got)
				}
			} else {
				fromFile(t, got, c.from)
			}
			if r := reasonsOf(t, home); !slices.Equal(r, c.reasons) {
				t.Errorf("reasons %q, want %q", r, c.reasons)
			}
		})
	}
}

// TestHookReaderWaitsAtMost20ms: a writer holding ledger.db costs the
// hook its 20 ms wait, not the 5 s of a command: recall then reads
// ledger.json. The wait handed to ledgerdb.Read is checked, not timed.
func TestHookReaderWaitsAtMost20ms(t *testing.T) {
	home, cfg := chainHome(t)
	holdLedgerDB(t, home)
	var busy []time.Duration
	t.Cleanup(func() { readLedgerDB = ledgerdb.Read })
	readLedgerDB = func(o ledgerdb.Options, d time.Duration, fn func(*ledgerdb.Reader) error) error {
		busy = append(busy, d)
		return ledgerdb.Read(o, d, fn)
	}
	fromFile(t, adjustOf(t, home, cfg), "json")
	// a literal, so a longer hookLedgerBusy fails too
	if len(busy) == 0 || slices.ContainsFunc(busy, func(d time.Duration) bool { return d != 20*time.Millisecond }) {
		t.Errorf("ledger.db read with busy waits %v, want each 20 ms (hookLedgerBusy %v)", busy, hookLedgerBusy)
	}
}

// TestAFreshHidingSkipsTheNextRecallWithTheDB: a hiding journaled after
// the tick built ledger.db drops the memory from the next prompt's
// recall, read from the database and its journal tail (no fallback).
func TestAFreshHidingSkipsTheNextRecallWithTheDB(t *testing.T) {
	home := hookHome(t)
	if code, out, errOut := run(t, "jobs", "tick"); code != output.OK {
		t.Fatalf("tick: exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(ledgerDBPath(home)); err != nil {
		t.Fatalf("the tick built no ledger.db: %v", err)
	}
	j := feedback.NewJournal(pathsOf(home).FeedbackDir(), feedback.Common{Session: "s0"})
	if err := j.AppendFolded(feedback.Event{Kind: feedback.KindHide, IDs: []string{"m1"}, Verdict: "wrong"}); err != nil {
		t.Fatal(err)
	}
	recallAnswer = hits(0.9, 0.85)
	c := prompt(t, home, "s1", longPrompt).Specific.Context
	if strings.Contains(c, "memory m1 about billing") || !strings.Contains(c, "memory m2 about billing") {
		t.Errorf("context, want m2 without the hidden m1:\n%s", c)
	}
	noLedgerFallback(t, home)
}

// noLedgerFallback: no ledger_* feedback_error was logged.
func noLedgerFallback(t *testing.T, home string) {
	t.Helper()
	for _, r := range reasonsOf(t, home) {
		if strings.HasPrefix(r, "ledger_") {
			t.Errorf("a ledger fallback was logged: %s", r)
		}
	}
}

// The verdict and restore chain of TestOutdatedFactIsListedStaleUntilRestored,
// with ledger.db built first: memory stale lists the fact reported, then
// no longer once restored, and no reader fell back.
func TestStaleChainWithTheDB(t *testing.T) {
	cs, home := creditSession(t)
	rawFact(creditA)
	showIn(t, home, "S", creditA)
	buildLedgerDB(t, home)
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "aaaaaaaa")); isErr || !strings.HasPrefix(out, "Retired.") {
		t.Fatalf("%v %s", isErr, out)
	}
	code, out, errOut := run(t, "memory", "stale")
	if code != output.OK || !strings.Contains(out, "Reported wrong or outdated\naaaaaaaa ") {
		t.Fatalf("stale after the verdict: exit %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut := run(t, "forget", "--restore", creditA); code != output.OK || !strings.Contains(out, creditA+" restored") {
		t.Fatalf("restore: exit %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut := run(t, "memory", "stale"); code != output.OK || strings.Contains(out, "aaaaaaaa") || !strings.HasPrefix(out, "Nothing needs review") {
		t.Errorf("stale after the restore: exit %d\n%s%s", code, out, errOut)
	}
	noLedgerFallback(t, home)
}

// TestAdjustNilWithoutLedger: an empty ledger.db and no shared counts are
// no ledger: no learned ranking, and the hits carry no rank without the
// boost.
func TestAdjustNilWithoutLedger(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	t.Setenv("BACKSIGHT_PROJECT", "billing")
	cfg, _, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	buildLedgerDB(t, home)
	if got := adjustOf(t, home, cfg); got != nil {
		t.Errorf("a learned ranking from an empty ledger: %+v", got)
	}
	recallAnswer = boostAnswer
	if hits := searchHits(t); strings.Contains(hits, "rank_without_boost") || strings.Contains(hits, `"boost"`) || !strings.Contains(hits, `"m1"`) {
		t.Errorf("hits %s, want them as without a ledger", hits)
	}
	noLedgerFallback(t, home)
}

// TestCommandReaderReadsTheSideFile: beside a ledger.db of a newer
// schema, a command reads the side file this binary's writers keep, a
// hook ledger.json (exported from it), logging ledger_schema.
func TestCommandReaderReadsTheSideFile(t *testing.T) {
	home, cfg := chainHome(t)
	execLedgerDB(t, home, "PRAGMA user_version = 2")
	// The side file's build exports ledger.json: the fixture's is put back.
	raw, err := os.ReadFile(filepath.Join(pathsOf(home).FeedbackDir(), "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	buildLedgerDB(t, home)
	writeLedger(t, home, string(raw))
	if _, err := os.Stat(filepath.Join(pathsOf(home).FeedbackDir(), ledgerdb.SideFileName(1))); err != nil {
		t.Fatalf("no side file: %v", err)
	}
	d := pathsOf(home)
	tw := telemetry.New(d.State, telemetry.Common{})
	got, err := ledgerReader(d, cfg, false, tw).Get(chainIDs)
	if _, ok := got["m-db"]; err != nil || !ok || got["h-db"].Hidden == nil {
		t.Errorf("command: %+v, %v; want the side file's m-db and h-db", got, err)
	}
	if r := reasonsOf(t, home); len(r) != 0 {
		t.Errorf("command: reasons %q", r)
	}
	got, err = ledgerReader(d, cfg, true, tw).Get(chainIDs)
	if _, ok := got["m-json"]; err != nil || !ok {
		t.Errorf("hook: %+v, %v; want ledger.json's m-json", got, err)
	}
	if r := reasonsOf(t, home); !slices.Equal(r, []string{telemetry.FeedbackReasonLedgerSchema}) {
		t.Errorf("hook: reasons %q", r)
	}
}

// TestIndexLedgerReadsTheDBSilently: the index's ledger is ledger.db,
// stamped by its build, else ledger.json, stamped by its files; it never
// writes telemetry (the index records its read errors itself), and reads
// whatever feedback's switch.
func TestIndexLedgerReadsTheDBSilently(t *testing.T) {
	home, _ := chainHome(t)
	appendConfig(t, home, "[feedback]\nenabled = false")
	d := pathsOf(home)
	type stamper interface {
		Stamp() (feedback.Stamp, error)
	}
	src := indexLedger(d)
	check := func(file, stampName string) {
		t.Helper()
		l, err := src.All()
		if _, ok := l.Memories["m-"+file]; err != nil || !ok || len(l.Memories) != 2 {
			t.Errorf("All: %+v, %v; want the %s file", l.Memories, err, file)
		}
		st, err := src.(stamper).Stamp()
		if err != nil || !slices.ContainsFunc(st.Files, func(f feedback.StampFile) bool { return strings.HasPrefix(f.Name, stampName) }) {
			t.Errorf("Stamp: %+v, %v; want a %s entry", st, err, stampName)
		}
	}
	check("db", ledgerdb.FileName+"#")
	removeLedgerDB(t, home)
	check("json", "ledger.json")
	if r := reasonsOf(t, home); len(r) != 0 {
		t.Errorf("reasons %q", r)
	}
}
