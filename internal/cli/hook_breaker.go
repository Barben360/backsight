package cli

import (
	"encoding/json"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/state"
)

// The hooks' circuit breaker (SPEC §14, A.1 network.breaker_*): after
// BreakerFailures recalls in a row where every call failed, recall pauses
// for BreakerPauseMin minutes, so an unreachable server does not cost
// every prompt the full recall budget.

// breakerOpen: recall is paused.
func (h *hookEnv) breakerOpen() bool { return now().Before(state.LoadBreaker(h.dirs).OpenUntil) }

// breakerRecord counts a recall that failed everywhere, or resets on any
// success; best effort, no lock (a lost count only delays the pause). It
// also records whether the server answered (state.Reach), at most once a
// minute while it does.
func (h *hookEnv) breakerRecord(failed bool) {
	state.RecordReach(h.dirs, !failed, now())
	b := state.LoadBreaker(h.dirs)
	switch {
	case !failed && b.Failures == 0 && b.OpenUntil.IsZero():
		return
	case !failed:
		b = state.Breaker{}
	default:
		b.Failures++
		if b.Failures >= h.cfg.Network.BreakerFailures {
			b.OpenUntil, b.Failures = now().Add(time.Duration(h.cfg.Network.BreakerPauseMin)*time.Minute), 0
		}
	}
	raw, _ := json.Marshal(b)
	_ = fsutil.WriteAtomic(state.BreakerPath(h.dirs), raw, 0o600)
}
