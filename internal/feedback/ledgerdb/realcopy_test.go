package ledgerdb

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
)

// TestIncrementalEqualsRebuildOnARealCopy replays a real journal into
// ledger.db as it grew: the month files of a copy of a state directory
// named by BACKSIGHT_LEDGER_BENCH_STATE (its feedback/ folder; never the
// live one) are copied into a fresh directory 50 lines at a time, a fold
// after each chunk. After every fold ledger.db equals a rebuild of what
// was copied (the oracle), floats bit for bit. When the copy reaches the
// copy's own ledger.json's journal_at, its memories and history import
// time are that file's, floats exactly: what v0.1.32 settled from the
// same lines (its watermark is its settle's clock, not a line's, so it is
// not compared). The folding params are [feedback]'s defaults, the
// copy's config's.
func TestIncrementalEqualsRebuildOnARealCopy(t *testing.T) {
	state := os.Getenv("BACKSIGHT_LEDGER_BENCH_STATE")
	if state == "" {
		t.Skip("BACKSIGHT_LEDGER_BENCH_STATE names no copy of a state directory")
	}
	src := paths.Dirs{State: state}.FeedbackDir()
	legacy, err := feedback.LoadLedger(src)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.JournalAt.File == "" || len(legacy.Memories) == 0 {
		t.Fatalf("the copy's ledger.json has no journal_at or no memory: %+v, %d memories", legacy.JournalAt, len(legacy.Memories))
	}
	cfg := config.Default(paths.Dirs{State: state}, "")
	p := feedback.Params{
		ConfidentRank:  cfg.Feedback.ConfidentRank,
		MissHalfLife:   time.Duration(cfg.Feedback.MissHalfLifeDays) * day,
		CreditHalfLife: time.Duration(cfg.Feedback.CreditHalfLifeDays) * day,
		TurnTimeout:    feedback.TurnTimeout,
		Weight:         cfg.Feedback.BoostWeight,
		PriorCredits:   cfg.Feedback.PriorCredits,
		PriorMisses:    cfg.Feedback.PriorMisses,
	}

	// the copy's complete lines, month file by month file
	type line struct {
		file string
		end  int64 // the offset after the line
		raw  []byte
	}
	files, err := feedback.MonthFiles(src)
	if err != nil || len(files) == 0 {
		t.Fatalf("month files %v: %v", files, err)
	}
	var lines []line
	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		off := int64(0)
		for {
			i := bytes.IndexByte(raw[off:], '\n')
			if i < 0 {
				break
			}
			end := off + int64(i) + 1
			lines = append(lines, line{name, end, raw[off:end]})
			off = end
		}
	}

	o := Options{Dirs: paths.Dirs{State: filepath.Join(t.TempDir(), "state")}, Getenv: func(string) string { return "" }, BinaryVersion: "v-test"}
	dir := feedbackDir(o)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	mustFold(t, w, p)
	const chunk = 50
	folds, refolds, metLegacy := 0, 0, false
	for start := 0; start < len(lines); {
		n := min(chunk, len(lines)-start)
		// a chunk ends where ledger.json's journal_at is
		for k := range n {
			if l := lines[start+k]; l.file == legacy.JournalAt.File && l.end == legacy.JournalAt.Offset {
				n = k + 1
				break
			}
		}
		for _, l := range lines[start : start+n] {
			appendBytes(t, filepath.Join(dir, l.file), l.raw)
		}
		start += n
		res := mustFold(t, w, p)
		folds++
		if res.Refolded {
			refolds++
		}
		last := lines[start-1]
		sameLedger(t, fmt.Sprintf("%s %d: folded", last.file, last.end), folded(t, o), oracle(t, dir, p))
		if c, ends := cursorsOf(t, o), journalEnds(t, o); !maps.Equal(c, ends) {
			t.Fatalf("at %s %d: cursors %v, want %v", last.file, last.end, c, ends)
		}
		if last.file == legacy.JournalAt.File && last.end == legacy.JournalAt.Offset {
			metLegacy = true
			got := folded(t, o)
			want := legacy
			got.Watermark, want.Watermark = time.Time{}, time.Time{}
			sameLedger(t, "the copy's ledger.json", got, want)
		}
		if t.Failed() {
			t.Fatalf("diverged at %s offset %d", last.file, last.end)
		}
	}
	if !metLegacy {
		t.Fatalf("no complete line of the copy ends at ledger.json's journal_at %+v", legacy.JournalAt)
	}
	got := folded(t, o)
	t.Logf("%d lines of %v in %d folds (%d refolds): %d memories, %d in the copy's ledger.json at %+v",
		len(lines), files, folds, refolds, len(got.Memories), len(legacy.Memories), legacy.JournalAt)
}
