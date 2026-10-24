package session

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/state"
)

// Sticky muting (SPEC §10.2, C12): a memory injected StickyMax times within
// StickyWindow is muted, across sessions, unless it matches very strongly.
type Sticky struct {
	Path  string // <state>/sticky.json
	Locks string
}

// Load returns {memory id: injection times} within the window of now.
func (s Sticky) Load(now time.Time, window time.Duration) map[string][]time.Time {
	out := map[string][]time.Time{}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return out
	}
	var all map[string][]time.Time
	if json.Unmarshal(raw, &all) != nil {
		return out
	}
	for id, ts := range all {
		for _, t := range ts {
			if now.Sub(t) < window {
				out[id] = append(out[id], t)
			}
		}
	}
	return out
}

// Muted: shown max times in the window, and below the bypass score.
func Muted(counts map[string][]time.Time, id string, score float64, maxShown int, bypass float64) bool {
	return len(counts[id]) >= maxShown && score < bypass
}

// Record adds an injection time for ids, dropping times outside the
// window; a busy lock skips the update (hooks never wait).
func (s Sticky) Record(ids []string, now time.Time, window time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	l, err := state.TryAcquire(filepath.Join(s.Locks, "sticky.lock"), "hook")
	var held *state.HeldError
	if errors.As(err, &held) && held.Stale && state.BreakStale(held) == nil {
		l, err = state.TryAcquire(filepath.Join(s.Locks, "sticky.lock"), "hook")
	}
	if errors.As(err, &held) {
		return ErrBusy
	}
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()
	counts := s.Load(now, window)
	for _, id := range ids {
		counts[id] = append(counts[id], now.UTC())
	}
	raw, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(s.Path, raw, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
