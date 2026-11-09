package cli

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/telemetry"
)

// TestLedgerLookupOnARealCopy measures what the prompt hook spends on the
// learned ranking: ledgerAdjust (ledger.db opened read-only) and one call
// for 20 ids, the hits of one prompt (one read transaction: the rows, the
// journal tail since the last fold, then the other machines' counts). It
// runs only on a copy of a real state directory named by
// BACKSIGHT_LEDGER_BENCH_STATE (its feedback/ folder; never the live
// one): the copy's journal and shared files are copied into a temporary
// state, ledger.db is built there from that journal as the tick would,
// and p50, p95 and max over 500 rounds are logged: the ledger lookup
// budget's check against its baseline (1.34-1.53 ms, on ledger.json).
// The temporary state has no ledger.json, and any fallback fails the
// test: the rounds read ledger.db.
func TestLedgerLookupOnARealCopy(t *testing.T) {
	state := os.Getenv("BACKSIGHT_LEDGER_BENCH_STATE")
	if state == "" {
		t.Skip("BACKSIGHT_LEDGER_BENCH_STATE names no copy of a state directory")
	}
	src := paths.Dirs{State: state}.FeedbackDir()
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	if err := os.MkdirAll(d.FeedbackDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		copied := strings.HasPrefix(name, "journal-") || strings.HasPrefix(name, "shared")
		if !e.Type().IsRegular() || !copied {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d.FeedbackDir(), name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default(d, "")
	o := ledgerdb.Options{Dirs: d, Getenv: func(string) string { return "" }, BinaryVersion: "v-test"}
	w, err := ledgerdb.OpenWriter(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Fold(context.Background(), feedbackParams(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var l feedback.Ledger
	if err := ledgerdb.Read(o, time.Second, func(r *ledgerdb.Reader) error {
		var err error
		l, err = r.All()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ids := slices.Sorted(maps.Keys(l.Memories))
	if len(ids) > 20 {
		ids = ids[:20]
	}
	if len(ids) == 0 {
		t.Fatal("the copy's journal folds to no memory")
	}

	tw := telemetry.New(d.State, telemetry.Common{})
	// what the rounds answer: each id's boost and hiding from the copy's
	// rows (read whole above), with the other machines' counts
	at := time.Now()
	adjust := ledgerAdjust(d, cfg, at, tw)
	if adjust == nil {
		t.Fatal("no learned ranking with feedback on")
	}
	p, shared, boosted := feedbackParams(cfg), sharedCounts(d, cfg, at), 0
	for id, a := range adjust(ids) {
		m := withShared(l.Memories[id], shared, id, p, at)
		want := recall.Adjustment{Hidden: m.Hidden != nil}
		if !want.Hidden {
			want.Boost = m.Boost(p, at)
		}
		if a != want {
			t.Errorf("%s: %+v, want %+v from the copy's ledger", id, a, want)
		}
		if a.Boost != 0 {
			boosted++
		}
	}
	if boosted == 0 {
		t.Fatalf("no boost among the %d ids: nothing to compare", len(ids))
	}

	const rounds, warmUp = 500, 20
	took := make([]time.Duration, 0, rounds)
	for i := range rounds + warmUp {
		start := time.Now()
		adjust := ledgerAdjust(d, cfg, time.Now(), tw)
		if adjust == nil {
			t.Fatal("no learned ranking with feedback on")
		}
		adjust(ids)
		if i >= warmUp {
			took = append(took, time.Since(start))
		}
	}
	files, _ := filepath.Glob(filepath.Join(d.TelemetryDir(), "events-*.jsonl"))
	for _, f := range files {
		if raw, err := os.ReadFile(f); err != nil || strings.Contains(string(raw), `"feedback_error"`) {
			t.Fatalf("the rounds fell back from ledger.db (%v):\n%s", err, raw)
		}
	}
	slices.Sort(took)
	t.Logf("ledger lookup over %d memories, %d ids: p50 %v, p95 %v, max %v (%d rounds)",
		len(l.Memories), len(ids), took[rounds/2], took[rounds*95/100], took[rounds-1], rounds)
}
