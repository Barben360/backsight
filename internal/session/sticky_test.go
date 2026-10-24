package session

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStickyMutesAfterThreeInADayUnlessStrong(t *testing.T) {
	dir := t.TempDir()
	s := Sticky{Path: filepath.Join(dir, "sticky.json"), Locks: filepath.Join(dir, "locks")}
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := s.Record([]string{"m1"}, t0.Add(time.Duration(i)*time.Hour), 24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	counts := s.Load(t0.Add(3*time.Hour), 24*time.Hour)
	if !Muted(counts, "m1", 0.9, 3, 0.97) || Muted(counts, "m1", 0.97, 3, 0.97) || Muted(counts, "m2", 0.5, 3, 0.97) {
		t.Errorf("counts %v", counts)
	}
	// 24 h after the first, one injection leaves the window
	if Muted(s.Load(t0.Add(24*time.Hour), 24*time.Hour), "m1", 0.9, 3, 0.97) {
		t.Error("still muted after the window")
	}
	_ = s.Record([]string{"m3"}, t0.Add(48*time.Hour), 24*time.Hour)
	if c := s.Load(t0.Add(48*time.Hour), 240*time.Hour); len(c["m1"]) != 0 {
		t.Errorf("old times are dropped on write: %v", c)
	}
}
