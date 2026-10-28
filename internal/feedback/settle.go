package feedback

import (
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/state"
)

// Outcomes a settle gives a show.
const (
	OutcomeCredit  = "credit"         // a used credit of its session attached to it
	OutcomeMiss    = "confident_miss" // shown near the top, its agent declared the turn, and did not use it
	OutcomeShown   = "shown"
	OutcomeUnknown = "unknown" // its agent never declared the turn: undeclared, interrupted or compacted
)

const (
	// TurnTimeout: a show settles no earlier than this after it was shown,
	// so a credit a few turns later still attaches.
	TurnTimeout = 30 * time.Minute
	// openTurnLimit: a show whose turn never ended (no later turn, no
	// stop) settles once this old.
	openTurnLimit = 2 * time.Hour
	// settleWindow is how far back Settle reads the journal.
	settleWindow = 62 * 24 * time.Hour
)

// Params are the values settling and folding use; a half-life of 0 means
// no decay.
type Params struct {
	ConfidentRank                int
	MissHalfLife, CreditHalfLife time.Duration
	TurnTimeout                  time.Duration
}

// record is p as a settle event carries it.
func (p Params) record() map[string]float64 {
	return map[string]float64{
		"confident_rank":        float64(p.ConfidentRank),
		"miss_half_life_days":   p.MissHalfLife.Hours() / 24,
		"credit_half_life_days": p.CreditHalfLife.Hours() / 24,
		"turn_timeout_minutes":  p.TurnTimeout.Minutes(),
	}
}

// SettleResult counts what one settle decided: shows by outcome, and the
// used credits that found no unsettled show to attach to (late).
type SettleResult struct {
	Shows   int `json:"shows"`
	Credits int `json:"credits"`
	Misses  int `json:"misses"`
	Unknown int `json:"unknown"`
	Late    int `json:"late"`
}

// missPath: a show on this path can be a confident miss. The briefing,
// reads and failure recalls never are.
func missPath(path string) bool {
	return path == "prompt" || path == "search" || path == "gate"
}

type pendingShow struct {
	Show
	session           string
	credited, settled bool
}

// Decide is the outcome of every show of events not yet in a settle event
// that can settle at now: at least TurnTimeout old, and its turn over (a
// later turn of its session, or a stop of its turn by its agent or the
// main agent) or the show openTurnLimit old. Events after now are not
// seen. Each used credit, in time order, attaches to its id's latest
// earlier show of its session neither credited nor settled; late counts
// the used credits since the last settle event that found none.
func Decide(events []Event, p Params, now time.Time) (settled []Settled, late int) {
	var seen []Event
	var lastSettle time.Time
	for _, e := range events {
		if e.TS.After(now) {
			continue
		}
		seen = append(seen, e)
		if e.Kind == "settle" {
			lastSettle = laterOf(lastSettle, e.TS)
		}
	}
	var shows []*pendingShow
	byKey := map[string]*pendingShow{}
	byID := map[string][]*pendingShow{} // session|id, in time order
	done := map[string]bool{}           // keys of settle events
	declared := map[string]bool{}       // session|turn|agent with a credit
	stopped := map[string]bool{}        // session|turn|agent with a stop
	turns := map[string][]Event{}       // session's turn events, in time order
	for _, e := range sortedByTime(seen) {
		switch e.Kind {
		case "turn":
			turns[e.Session] = append(turns[e.Session], e)
		case "shows":
			for _, s := range showsOf(e) {
				if byKey[s.Key] != nil {
					continue
				}
				ps := &pendingShow{Show: s, session: e.Session}
				shows, byKey[s.Key] = append(shows, ps), ps
				byID[e.Session+"|"+s.ID] = append(byID[e.Session+"|"+s.ID], ps)
			}
		case "settle":
			for _, s := range e.Settled {
				done[s.Key] = true
				if ps := byKey[s.Key]; ps != nil {
					ps.settled = true
				}
			}
		case "credit":
			declared[e.Session+"|"+e.Turn+"|"+e.Agent] = true
			if e.Verdict != "used" {
				continue
			}
			for _, id := range e.IDs {
				if !attach(byID[e.Session+"|"+id]) && e.TS.After(lastSettle) {
					late++
				}
			}
		case "stop":
			stopped[e.Session+"|"+e.Turn+"|"+e.Agent] = true
		}
	}
	for _, s := range shows {
		age := now.Sub(s.At)
		if done[s.Key] || age < p.TurnTimeout {
			continue
		}
		turn := ownTurn(s.Show, turns[s.session])
		if age < openTurnLimit && !turnOver(s, turn, turns[s.session], stopped) {
			continue
		}
		outcome := OutcomeShown
		switch {
		case s.credited:
			outcome = OutcomeCredit
		case !declared[s.session+"|"+turn+"|"+s.Agent]:
			outcome = OutcomeUnknown
		case s.Rank >= 1 && s.Rank <= p.ConfidentRank && missPath(s.Path):
			outcome = OutcomeMiss
		}
		settled = append(settled, Settled{Key: s.Key, ID: s.ID, At: s.At, Outcome: outcome})
	}
	return settled, late
}

// attach credits the latest of shows neither credited nor settled; false
// when there is none.
func attach(shows []*pendingShow) bool {
	for i := len(shows) - 1; i >= 0; i-- {
		if s := shows[i]; !s.credited && !s.settled {
			s.credited = true
			return true
		}
	}
	return false
}

// ownTurn is the turn s belongs to: its own, or for a show before any
// prompt (the briefing) the first turn after it.
func ownTurn(s Show, turns []Event) string {
	if s.Turn != "" {
		return s.Turn
	}
	for _, t := range turns {
		if !t.TS.Before(s.At) {
			return t.Turn
		}
	}
	return ""
}

// turnOver: a later turn of the session began, or turn stopped for s's
// agent or the main agent (a subagent's stop does not end the main turn).
func turnOver(s *pendingShow, turn string, turns []Event, stopped map[string]bool) bool {
	if stopped[s.session+"|"+turn+"|"+s.Agent] || stopped[s.session+"|"+turn+"|"] {
		return true
	}
	for _, t := range turns {
		if t.TS.After(s.At) && t.Turn != turn {
			return true
		}
	}
	return false
}

// Settle decides the shows of dir's journal that can settle at now,
// journals them as one settle event carrying p, and brings ledger.json up
// to date: the settle and forget events since its watermark, in time
// order. Under the ledger lock: a concurrent settle gets a
// *state.HeldError and changes nothing. A ledger of another schema is left
// alone, and nothing is journaled.
func Settle(dir string, p Params, now time.Time) (SettleResult, error) {
	var res SettleResult
	release, err := lockLedger(dir)
	if err != nil {
		return res, err
	}
	defer release()
	l, err := LoadLedger(dir)
	if err != nil {
		return res, err
	}
	events, _, err := Read(dir, now.Add(-settleWindow))
	if err != nil {
		return res, err
	}
	var apply []Event
	for _, e := range events {
		if (e.Kind == "settle" || e.Kind == "forget") && e.TS.After(l.Watermark) && !e.TS.After(now) {
			apply = append(apply, e) // a ledger write that failed, and forgets since the last settle
		}
	}
	settled, late := Decide(events, p, now)
	if len(settled) > 0 || late > 0 {
		// Even with no show, the event marks where these late credits were
		// counted: the next settle counts only later ones.
		e := Event{Kind: "settle", TS: now.UTC(), Settled: settled, Params: p.record()}
		j := NewJournal(dir, Common{Binary: buildinfo.Version})
		j.now = func() time.Time { return now }
		if err := j.Append(e); err != nil {
			return res, err
		}
		apply = append(apply, e)
	}
	for _, e := range sortedByTime(apply) {
		l.Apply(e, p)
	}
	l.Watermark = now.UTC()
	if err := saveLedger(dir, l); err != nil {
		return res, err
	}
	res.Late = late
	for _, s := range settled {
		res.Shows++
		switch s.Outcome {
		case OutcomeCredit:
			res.Credits++
		case OutcomeMiss:
			res.Misses++
		case OutcomeUnknown:
			res.Unknown++
		}
	}
	return res, nil
}

// lockLedger takes <state>/locks/feedback-ledger.lock (dir is
// <state>/feedback), breaking a lock whose owner is gone.
func lockLedger(dir string) (release func(), err error) {
	l, _, err := state.Acquire(filepath.Join(filepath.Dir(dir), "locks", "feedback-ledger.lock"), "feedback ledger")
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}
