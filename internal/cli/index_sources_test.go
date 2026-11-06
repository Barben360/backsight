package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLocalStatusRow(t *testing.T) {
	root := t.TempDir()
	d := paths.Dirs{State: filepath.Join(root, "state"), Config: filepath.Join(root, "config")}
	T := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	F := T.Add(2 * time.Hour)
	B := T.Add(30 * time.Hour)
	const content = "canary-note-content-7f3a"

	ob := notesOutbox(d)
	for _, n := range []retain.Note{
		retain.NewNote("b", "decision", "p", content+" later", T.Add(time.Hour)),
		retain.NewNote("b", "decision", "p", content, T),
	} {
		if _, err := ob.Append(n); err != nil {
			t.Fatal(err)
		}
	}
	q := retain.NewNote("b", "pitfall", "p", content+" held", T.Add(-time.Hour))
	writeJSONFile(t, ob.QuarantinePath, q)
	writeJSONFile(t, filepath.Join(d.State, "reach.json"), state.Reach{LastOK: T.Add(time.Hour), FailingSince: F, SessionsWaiting: 2})
	writeJSONFile(t, state.BreakerPath(d), state.Breaker{Failures: 5, OpenUntil: B})
	st := state.State{SchemaVersion: state.SchemaVersion,
		BackupVerify: &state.BackupVerify{Archive: "/backups/a.tar", CreatedAt: T.Add(-24 * time.Hour), OK: true, VerifiedAt: T.Add(-23 * time.Hour)},
		UpdateCheck:  &state.UpdateCheck{CheckedAt: T.Add(-time.Minute), Newer: 1, Error: "rate limited"},
		Backfill:     &state.Backfill{Paused: "budget", Sessions: []string{"s1", "s2", "s3"}},
	}
	if err := state.Save(d.StateFile(), st); err != nil {
		t.Fatal(err)
	}

	ix, err := index.Open(context.Background(), indexOptions(d, t.TempDir(), nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	if _, err := ix.CatchUp(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	want := map[string]int64{
		"notes_queued": 2, "notes_oldest_at": ms(T), "quarantined": 1, "quarantine_oldest_at": ms(T.Add(-time.Hour)),
		"sessions_waiting": 2, "reach_last_ok": ms(T.Add(time.Hour)), "reach_failing_since": ms(F),
		"breaker_failures": 5, "breaker_open_until": ms(B),
		"backup_created_at": ms(T.Add(-24 * time.Hour)), "backup_verified_at": ms(T.Add(-23 * time.Hour)), "backup_ok": 1,
		"update_checked_at": ms(T.Add(-time.Minute)), "update_newer": 1, "update_check_failed": 1,
		"backfill_sessions": 3, "backfill_paused": 1,
	}
	err = ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
		for col, w := range want {
			var got sql.NullInt64
			if err := tx.QueryRow(`SELECT ` + col + ` FROM local_status WHERE id = 1`).Scan(&got); err != nil {
				return err
			}
			if !got.Valid || got.Int64 != w {
				t.Errorf("%s = %v, want %d", col, got, w)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	var db []byte
	for _, p := range []string{ix.Path(), ix.Path() + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		db = append(db, b...)
	}
	for _, s := range []string{"rate limited", content, "/backups/a.tar"} {
		if bytes.Contains(db, []byte(s)) {
			t.Errorf("%q in the index bytes", s)
		}
	}
}

// Nothing to read is a row of zeros and NULLs, not an error.
func TestLocalStatusOfAnEmptyState(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	ls, err := localStatusOf(d)()
	if err != nil {
		t.Fatal(err)
	}
	if ls != (index.LocalStatus{}) {
		t.Errorf("%+v", ls)
	}
}

// A state.json that does not parse is an error: the index keeps its row.
func TestLocalStatusOfABadStateFile(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	if err := os.MkdirAll(d.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.StateFile(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := localStatusOf(d)(); err == nil {
		t.Error("no error")
	}
}

func TestIndexOptions(t *testing.T) {
	d := paths.Dirs{State: "/s"}
	var buf bytes.Buffer
	o := indexOptions(d, "/h", &buf)
	if o.Dirs != d || o.Home != "/h" || o.Progress != &buf || o.LocalStatus == nil || o.Machines == nil || o.MachinesOff == nil {
		t.Errorf("%+v", o)
	}
}

// A check that went through and a running backfill are not failures.
func TestLocalStatusOfAGoodCheck(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	at := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	st := state.State{SchemaVersion: state.SchemaVersion, UpdateCheck: &state.UpdateCheck{CheckedAt: at},
		Backfill: &state.Backfill{Sessions: []string{"s1"}}}
	if err := state.Save(d.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	ls, err := localStatusOf(d)()
	if err != nil {
		t.Fatal(err)
	}
	if ls.UpdateCheckFailed || ls.BackfillPaused || !ls.UpdateCheckedAt.Equal(at) || ls.BackfillSessions != 1 {
		t.Errorf("%+v", ls)
	}
}

// A quarantined session counts with the quarantined notes, as doctor and
// report count them (issue 180); a released session's cursor does not.
func TestLocalStatusCountsQuarantinedSessions(t *testing.T) {
	root := t.TempDir()
	d := paths.Dirs{State: filepath.Join(root, "state"), Config: filepath.Join(root, "config")}
	q := retain.NewNote("b", "pitfall", "p", "held note", time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC))
	writeJSONFile(t, notesOutbox(d).QuarantinePath, q)
	for sid, held := range map[string]bool{"held-1": true, "held-2": true, "released": false} {
		if err := retain.SaveCursor(d.CursorsDir(), sid, retain.Cursor{Quarantined: held}); err != nil {
			t.Fatal(err)
		}
	}
	ls, err := localStatusOf(d)()
	if err != nil {
		t.Fatal(err)
	}
	if ls.Quarantined != 3 {
		t.Errorf("Quarantined = %d, want 3 (1 note + 2 sessions)", ls.Quarantined)
	}
}

// TestSharingOffEmptiesTheRows: the other PCs' rows are in the index only
// while feedback.enabled and share_ledger are on; the cache file stays on
// disk, and the rows return when sharing does.
func TestSharingOffEmptiesTheRows(t *testing.T) {
	root := t.TempDir()
	d := paths.Dirs{State: filepath.Join(root, "state"), Config: filepath.Join(root, "config"), Data: filepath.Join(root, "data")}
	home := t.TempDir()
	setSharing := func(enabled, share bool) {
		t.Helper()
		toml := fmt.Sprintf("[feedback]\nenabled = %v\nshare_ledger = %v\n", enabled, share)
		if err := os.MkdirAll(d.Config, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d.ConfigFile(), []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setSharing(true, true)

	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	const other = "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6"
	doc := machines.Doc{Machine: other, At: now.Add(-time.Hour).Truncate(time.Second), OS: "linux", FirstSeen: "2026-09-02",
		BinaryVersion: "v0.1.30-m7", Until: yesterday,
		Days: []machines.Day{{Day: yesterday, Figures: machines.Figures{Prompts: 4, Hit: machines.Pair{1, 2}}, Flow: machines.Pair{1, 1}}}}
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d.MetricsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := machines.SaveCache(d.MetricsDir(), machines.Cache{FetchedAt: now,
		Docs: []machines.CachedDoc{{Name: machines.DocPrefix + other, Raw: raw}}}); err != nil {
		t.Fatal(err)
	}

	ix, err := index.Open(context.Background(), indexOptions(d, home, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	rows := func() (docs, days int) {
		t.Helper()
		err := ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
			if err := tx.QueryRow(`SELECT count(*) FROM pc_docs`).Scan(&docs); err != nil {
				return err
			}
			return tx.QueryRow(`SELECT count(*) FROM pc_days`).Scan(&days)
		})
		if err != nil {
			t.Fatal(err)
		}
		return docs, days
	}
	catchUp := func() {
		t.Helper()
		if _, err := ix.CatchUp(context.Background(), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	catchUp()
	if d, n := rows(); d != 1 || n != 1 {
		t.Fatalf("sharing on: %d documents, %d days, want 1 and 1", d, n)
	}
	for name, on := range map[string][2]bool{"share_ledger off": {true, false}, "feedback off": {false, true}} {
		setSharing(on[0], on[1])
		catchUp()
		if d, n := rows(); d != 0 || n != 0 {
			t.Errorf("%s: %d documents, %d days remain", name, d, n)
		}
		if _, err := os.Stat(machines.CacheFile(d.MetricsDir())); err != nil {
			t.Errorf("%s: the cache file must stay: %v", name, err)
		}
		setSharing(true, true)
		catchUp()
		if d, n := rows(); d != 1 || n != 1 {
			t.Errorf("%s, then on again: %d documents, %d days, want 1 and 1", name, d, n)
		}
	}
}
