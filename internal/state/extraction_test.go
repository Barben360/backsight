package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
)

// A hold is recorded for one bank and read back for it (and for the
// bank's legacy name); another bank, a cleared hold, an absent or garbled
// file hold nothing.
func TestExtractionHoldRoundTrip(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	if e := LoadExtraction(d); e.HeldFor(config.DefaultBank) {
		t.Fatalf("absent file holds: %+v", e)
	}
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if err := SaveExtraction(d, Extraction{Bank: config.DefaultBank, HeldSince: t0, KeyRejected: true, CheckedAt: t0}); err != nil {
		t.Fatal(err)
	}
	e := LoadExtraction(d)
	if !e.HeldFor(config.DefaultBank) || !e.HeldFor(config.LegacyBank) || !e.KeyRejected || !e.HeldSince.Equal(t0) {
		t.Fatalf("read back: %+v", e)
	}
	if e.HeldFor("other-bank") {
		t.Error("another bank is held")
	}
	if err := SaveExtraction(d, Extraction{Bank: config.DefaultBank, ProbeOKAt: t0}); err != nil {
		t.Fatal(err)
	}
	if e := LoadExtraction(d); e.HeldFor(config.DefaultBank) || !e.ProbeOKAt.Equal(t0) {
		t.Errorf("cleared hold: %+v", e)
	}
	if err := os.WriteFile(filepath.Join(d.State, "extraction.json"), []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if e := LoadExtraction(d); e.HeldFor(config.DefaultBank) {
		t.Errorf("garbled file holds: %+v", e)
	}
}
