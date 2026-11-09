package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/telemetry"
)

// TestEveryReaderReadsTheDB: ledger.db hides a memory and has the earlier
// history imported; ledger.json, edited by hand after the build, holds
// neither. memory stale, feedback log --id, the tick's hidden checks and
// history import, the shared upload and the metrics index's ledger table
// all answer with the database's values, and none fell back.
func TestEveryReaderReadsTheDB(t *testing.T) {
	port := fakeHindsight(t)
	home := devInstall(t, true)
	pointAt(t, home, port)
	cfg, _, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := pathsOf(home)
	dir := d.FeedbackDir()
	// An id the shared document keeps, hidden 8 days ago to the second
	// (the shared document holds seconds; memory stale lists it after 7).
	const hid = shareM1
	hidAt := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	j := feedback.NewJournal(dir, feedback.Common{Session: "s0"})
	for _, e := range []feedback.Event{
		{Kind: feedback.KindHide, TS: hidAt, IDs: []string{hid}, Verdict: "wrong"},
		{Kind: feedback.KindImport, TS: hidAt.Add(time.Minute), Outcome: feedback.ImportComplete},
	} {
		if err := j.AppendFolded(e); err != nil {
			t.Fatal(err)
		}
	}
	buildLedgerDB(t, home)
	l, err := feedback.LoadLedger(dir)
	if err != nil || l.Memories[hid].Hidden == nil || l.HistoryImported.IsZero() {
		t.Fatalf("the build exported %+v, %v: want %s hidden and the history imported", l, err, hid)
	}
	l.Memories, l.HistoryImported = map[string]feedback.Entry{}, time.Time{}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	writeLedger(t, home, string(raw))
	tw := telemetry.New(d.State, telemetry.Common{})

	t.Run("memory stale", func(t *testing.T) {
		code, out, errOut := run(t, "memory", "stale", "--json")
		var stale staleOut
		if err := json.Unmarshal([]byte(out), &stale); err != nil || code != output.OK {
			t.Fatalf("exit %d: %v\n%s%s", code, err, out, errOut)
		}
		if len(stale.Data.Hidden) != 1 || stale.Data.Hidden[0].ID != hid {
			t.Errorf("hidden %+v, want %s", stale.Data.Hidden, hid)
		}
	})
	t.Run("feedback log --id", func(t *testing.T) {
		code, out, errOut := run(t, "feedback", "log", "--id", hid, "--json")
		var res struct {
			Data struct {
				Entry *feedback.Entry `json:"entry"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil || code != output.OK {
			t.Fatalf("exit %d: %v\n%s%s", code, err, out, errOut)
		}
		if res.Data.Entry == nil || res.Data.Entry.Hidden == nil || !res.Data.Entry.Hidden.Since.Equal(hidAt) {
			t.Errorf("entry %+v, want hidden since %v", res.Data.Entry, hidAt)
		}
	})
	t.Run("the tick's hidden checks", func(t *testing.T) {
		var mu sync.Mutex
		var read []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			read = append(read, r.URL.Path)
			mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		}))
		t.Cleanup(srv.Close)
		cl, err := hindsight.New(srv.URL, "k", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if n := checkHidden(context.Background(), cl, "b", ledgerReader(d, cfg, false, tw), dir, time.Now()); n != 0 {
			t.Errorf("lifted %d", n)
		}
		if len(read) != 1 || !strings.Contains(read[0], "/memories/"+hid+"/history") {
			t.Errorf("read %q, want the hidden memory's history", read)
		}
	})
	t.Run("the tick's history import", func(t *testing.T) {
		if got := importInTick(context.Background(), d, home, cfg, tw); got != "" {
			t.Errorf("importInTick %q, want nothing to do", got)
		}
	})
	t.Run("the shared upload", func(t *testing.T) {
		cl, err := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "k", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if up, err := uploadShared(context.Background(), cl, d, cfg, "pc-a", time.Now(), tw); !up || err != nil {
			t.Fatalf("uploaded %v, %v", up, err)
		}
		docs, err := retain.ControlDocuments(context.Background(), cl, cfg.Server.Bank, sharedDocPrefix)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(docs, func(it hindsight.DocumentItem) bool { return it.ID == sharedDocPrefix+"pc-a" })
		if i < 0 {
			t.Fatalf("no document of pc-a among %d", len(docs))
		}
		doc, err := feedback.ParseSharedDoc(docs[i].Metadata[sharedMetaKey])
		if err != nil {
			t.Fatal(err)
		}
		if !doc.Entries[hid].HiddenSince.Equal(hidAt) {
			t.Errorf("entries %+v, want %s hidden since %v", doc.Entries, hid, hidAt)
		}
	})
	t.Run("stats index rebuild", func(t *testing.T) {
		if code, out, errOut := run(t, "stats", "index", "rebuild"); code != output.OK {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		if n := indexCount(t, home, `SELECT count(*) FROM ledger WHERE memory_id = ? AND hidden_since IS NOT NULL`, hid); n != 1 {
			t.Errorf("%d hidden h-db row(s) in the ledger table, want 1", n)
		}
	})
	noLedgerFallback(t, home)
}

// ledgerTableIDs is the ids the metrics index's ledger table holds.
func ledgerTableIDs(t *testing.T, ix *index.Index) []string {
	t.Helper()
	var ids []string
	err := ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT memory_id FROM ledger ORDER BY memory_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestIndexLedgerFollowsTheDBOnceItIsBack: the metrics index mirrors
// ledger.db. While the database cannot be read, but its stamp can (its
// memories table gone), a catch-up mirrors ledger.json; once the
// database reads again, the next catch-up mirrors it, though its stamp
// is the one the fallback's read was taken with.
func TestIndexLedgerFollowsTheDBOnceItIsBack(t *testing.T) {
	home, _ := chainHome(t)
	d := pathsOf(home)
	ctx := context.Background()
	ix, err := index.Open(ctx, indexOptions(d, home, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	catchUp := func(want ...string) {
		t.Helper()
		if _, err := ix.CatchUp(ctx, time.Minute); err != nil {
			t.Fatal(err)
		}
		if got := ledgerTableIDs(t, ix); !slices.Equal(got, want) {
			t.Errorf("ledger table %q, want %q", got, want)
		}
	}
	catchUp("h-db", "m-db")
	// A line the ledgers ignore changes the stamp; the rows cannot be read.
	j := feedback.NewJournal(d.FeedbackDir(), feedback.Common{Session: "s0"})
	if err := j.Append(feedback.Event{Kind: "turn", Turn: "p1"}); err != nil {
		t.Fatal(err)
	}
	execLedgerDB(t, home, "ALTER TABLE memories RENAME TO memories_away")
	catchUp("h-json", "m-json")
	execLedgerDB(t, home, "ALTER TABLE memories_away RENAME TO memories")
	catchUp("h-db", "m-db")
}

// TestMaintainSweepsOldRebuildPlans: jobs maintain removes what a
// feedback rebuild --plan that died left in <state>/staging (a copy of
// the journal) once a day old, and nothing else there.
func TestMaintainSweepsOldRebuildPlans(t *testing.T) {
	home := devInstall(t, true)
	configure(t, home, "[backup]\nenabled = false")
	staging := filepath.Join(pathsOf(home).State, "staging")
	old := time.Now().Add(-25 * time.Hour)
	mk := func(name string, at time.Time) string {
		t.Helper()
		p := filepath.Join(staging, name)
		if err := os.MkdirAll(filepath.Join(p, "feedback"), 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(p, "feedback", "journal-2026-11.jsonl")
		if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{f, filepath.Join(p, "feedback"), p} {
			if err := os.Chtimes(q, at, at); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	dead := mk("rebuild-plan-111", old)
	running := mk("rebuild-plan-222", time.Now().Add(-time.Hour))
	other := mk("restore-333", old)
	code, out, _ := run(t, "jobs", "maintain", "--json")
	var env maintainEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d: %v\n%s", code, err, out)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("the day-old plan: %v", err)
	}
	for _, p := range []string{running, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
		}
	}
	if !strings.Contains(env.Data.FeedbackLedger, "rebuild-plan-111") || strings.Contains(env.Data.FeedbackLedger, "222") {
		t.Errorf("feedback_ledger %q, want the plan removed named", env.Data.FeedbackLedger)
	}
	if code, out, _ := run(t, "jobs", "maintain", "--json"); code != output.OK || strings.Contains(out, "rebuild-plan") {
		t.Errorf("second run: exit %d\n%s", code, out)
	}
}
