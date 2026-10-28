package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
)

// Reach is <state>/reach.json: when the server last answered this machine,
// and since when it has not. The hooks' recall (through the breaker) and
// one health call per jobs tick write it; doctor's server-reach check and
// the session banner read it, with no call of their own, so a client never
// stops sending without saying so.
type Reach struct {
	LastOK       time.Time `json:"last_ok"`
	FailingSince time.Time `json:"failing_since,omitzero"` // zero while the server answers
	// SessionsWaiting: jobs tick's last count of sessions kept for retry.
	SessionsWaiting int `json:"sessions_waiting"`
}

// reachRewrite: an answer rewrites the file at most once a minute (the
// hooks record every recall).
const reachRewrite = time.Minute

func reachPath(d paths.Dirs) string { return filepath.Join(d.State, "reach.json") }

// LoadReach reads reach.json; absent or unreadable: nothing known (zero).
func LoadReach(d paths.Dirs) Reach {
	var r Reach
	if raw, err := os.ReadFile(reachPath(d)); err == nil && json.Unmarshal(raw, &r) != nil {
		return Reach{}
	}
	return r
}

// RecordReach records whether the server answered at at. It writes only on
// a change (the first failure, the first answer after failures) or when
// the last answer is a minute old or more. Best effort, no lock: a lost
// write is redone by the next call.
func RecordReach(d paths.Dirs, ok bool, at time.Time) {
	r := LoadReach(d)
	switch since := at.Sub(r.LastOK); {
	case !ok && !r.FailingSince.IsZero():
		return // still failing since the first failure
	case !ok:
		r.FailingSince = at
	case r.FailingSince.IsZero() && !r.LastOK.IsZero() && since >= 0 && since < reachRewrite:
		return
	default:
		r.LastOK, r.FailingSince = at, time.Time{}
	}
	saveReach(d, r)
}

// RecordWaiting records the number of sessions jobs tick kept for retry.
func RecordWaiting(d paths.Dirs, n int) {
	r := LoadReach(d)
	if r.SessionsWaiting == n {
		return
	}
	r.SessionsWaiting = n
	saveReach(d, r)
}

func saveReach(d paths.Dirs, r Reach) {
	raw, _ := json.Marshal(r)
	_ = fsutil.WriteAtomic(reachPath(d), raw, 0o600)
}

// Breaker is <state>/breaker.json, the hooks' circuit breaker (SPEC §14,
// A.1 network.breaker_*): after network.breaker_failures recalls in a row
// where every call failed, recall pauses until OpenUntil.
type Breaker struct {
	Failures  int       `json:"failures"`
	OpenUntil time.Time `json:"open_until"`
}

// BreakerPath is where the breaker is kept.
func BreakerPath(d paths.Dirs) string { return filepath.Join(d.State, "breaker.json") }

// LoadBreaker reads breaker.json; absent or unreadable: closed (zero).
func LoadBreaker(d paths.Dirs) Breaker {
	var b Breaker
	if raw, err := os.ReadFile(BreakerPath(d)); err == nil && json.Unmarshal(raw, &b) != nil {
		return Breaker{}
	}
	return b
}
