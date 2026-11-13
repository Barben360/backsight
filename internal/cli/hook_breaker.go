package cli

import (
	"encoding/json"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/state"
)

// The hooks' circuit breaker (SPEC §14, A.1 network.breaker_*): after
// BreakerFailures recalls in a row where every call failed, or one prompt
// recall where a call ran out of its budget, recall pauses for
// BreakerPauseMin minutes, so a server that is down or hangs does not cost
// every prompt the recall budget. After the pause, the recalls probe the
// server within probeBudget until one answers. Best effort: breaker.json is
// read and written without a lock (hooks never wait on one), so two hooks
// at once can lose a count.

// breakerOpen: recall is paused.
func (h *hookEnv) breakerOpen() bool { return now().Before(state.LoadBreaker(h.dirs).OpenUntil) }

// breakerProbing: the pause is over and no recall has answered since, so
// the next one is a probe and gets the probe's budget.
func (h *hookEnv) breakerProbing() bool {
	b := state.LoadBreaker(h.dirs)
	return !b.OpenUntil.IsZero() && !now().Before(b.OpenUntil)
}

// budget is a recall's budget: full, or the probe's when the breaker
// probes and that is shorter.
func (h *hookEnv) budget(full time.Duration) time.Duration {
	if h.breakerProbing() {
		return min(full, probeBudget)
	}
	return full
}

// breakerRecord counts a recall that failed everywhere, or resets on any
// success; timedOut (a prompt recall where a call ran out of a budget at
// least the probe's: the server accepted it and did not answer) opens the
// breaker at once, even when another scope answered, since each such
// prompt waits the whole budget. A lost count only delays the pause. It
// also records whether the server answered (state.Reach), at most once a
// minute while it does.
func (h *hookEnv) breakerRecord(failed, timedOut bool) {
	// Another server answering at the joined address is not this PC's
	// server answering: only a failure is recorded then, as jobs tick does.
	if failed || otherServer(h.dirs, h.cfg) == nil {
		state.RecordReach(h.dirs, !failed, now())
	}
	b := state.LoadBreaker(h.dirs)
	pause := now().Add(time.Duration(h.cfg.Network.BreakerPauseMin) * time.Minute)
	switch {
	case timedOut:
		b.OpenUntil, b.Failures = pause, 0
	case !failed && b.Failures == 0 && b.OpenUntil.IsZero():
		return
	case !failed:
		b = state.Breaker{}
	default:
		b.Failures++
		if b.Failures >= h.cfg.Network.BreakerFailures {
			b.OpenUntil, b.Failures = pause, 0
		}
	}
	raw, _ := json.Marshal(b)
	_ = fsutil.WriteAtomic(state.BreakerPath(h.dirs), raw, 0o600)
}
