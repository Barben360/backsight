package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// The hooks' circuit breaker (SPEC §14, A.1 network.breaker_*): after
// BreakerFailures recalls in a row where every call failed, recall pauses
// for BreakerPauseMin minutes, so an unreachable server does not cost
// every prompt the full recall budget.
type breakerState struct {
	Failures  int       `json:"failures"`
	OpenUntil time.Time `json:"open_until"`
}

func (h *hookEnv) breakerPath() string { return filepath.Join(h.dirs.State, "breaker.json") }

func (h *hookEnv) breaker() breakerState {
	var b breakerState
	if raw, err := os.ReadFile(h.breakerPath()); err == nil {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

// breakerOpen: recall is paused.
func (h *hookEnv) breakerOpen() bool { return now().Before(h.breaker().OpenUntil) }

// breakerRecord counts a recall that failed everywhere, or resets on any
// success; best effort, no lock (a lost count only delays the pause).
func (h *hookEnv) breakerRecord(failed bool) {
	b := h.breaker()
	switch {
	case !failed && b.Failures == 0 && b.OpenUntil.IsZero():
		return
	case !failed:
		b = breakerState{}
	default:
		b.Failures++
		if b.Failures >= h.cfg.Network.BreakerFailures {
			b.OpenUntil, b.Failures = now().Add(time.Duration(h.cfg.Network.BreakerPauseMin)*time.Minute), 0
		}
	}
	raw, _ := json.Marshal(b)
	_ = fsutil.WriteAtomic(h.breakerPath(), raw, 0o600)
}
