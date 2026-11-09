package feedback

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

// Outcomes a settle gives a show.
const (
	OutcomeCredit  = "credit"         // a used credit of its session attached to it
	OutcomeMiss    = "confident_miss" // shown near the top, its agent declared the turn, and did not use it
	OutcomeShown   = "shown"
	OutcomeUnknown = "unknown" // its agent never declared the turn: undeclared, interrupted or compacted
	// OutcomeForgotten: its memory was forgotten at or after the show,
	// before it settled. Settled so that it is never decided again, and
	// counted nowhere.
	OutcomeForgotten = "forgotten"
)

const (
	// TurnTimeout: a show settles no earlier than this after it was shown,
	// so a credit a few turns later still attaches.
	TurnTimeout = 30 * time.Minute
	// OpenTurnLimit: a show whose turn never ended (no later turn, no
	// stop) settles once this old.
	OpenTurnLimit = 2 * time.Hour
	// SettleWindow is how far back Settle reads the journal.
	SettleWindow = 62 * 24 * time.Hour
)

// Params are the values settling, folding and the boost use; a half-life
// of 0 means no decay. Weight (k) and the priors PriorCredits (α) and
// PriorMisses (β) shape Ledger.Boost.
type Params struct {
	ConfidentRank                     int
	MissHalfLife, CreditHalfLife      time.Duration
	TurnTimeout                       time.Duration
	Weight, PriorCredits, PriorMisses float64
}

// Keys of the half-lives in a settle event's params.
const (
	missHalfLifeKey   = "miss_half_life_days"
	creditHalfLifeKey = "credit_half_life_days"
)

// Record is p as a settle event carries it.
func (p Params) Record() map[string]float64 {
	return map[string]float64{
		"confident_rank":       float64(p.ConfidentRank),
		missHalfLifeKey:        p.MissHalfLife.Hours() / 24,
		creditHalfLifeKey:      p.CreditHalfLife.Hours() / 24,
		"turn_timeout_minutes": p.TurnTimeout.Minutes(),
	}
}

// SettleResult counts what one settle decided: shows by outcome, and the
// used credits that found no unsettled show to attach to (late). Rebuilt
// names the file a ledger.json that did not parse was kept as, before the
// ledger was rebuilt from the journal; "" when it parsed.
type SettleResult struct {
	Shows   int    `json:"shows"`
	Credits int    `json:"credits"`
	Misses  int    `json:"misses"`
	Unknown int    `json:"unknown"`
	Late    int    `json:"late"`
	Rebuilt string `json:"rebuilt,omitempty"`
}

// PathSources: the facts a memory_credit Sources: answer listed, journaled
// as shows so the next verdict can name them; they are never settled, so
// they count neither as shown nor as a miss.
const PathSources = "sources"

// PathStore: the similar memory a memory_store answer showed (0083 C4),
// journaled so memory_credit accepts it. It settles like any show but is
// never a miss, the end-of-turn check does not ask about it, and the
// ledger counts it only credited: a store showing a memory is no recall,
// so it never makes it "shown often, never used".
const PathStore = "store"

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

// Decide is the outcome of every show of events (PathSources ones aside)
// not yet in a settle event that can settle at now: at least TurnTimeout
// old, and its turn over (a later turn of its session, or a stop of its
// turn by its agent or the main agent) or the show OpenTurnLimit old. Events after now are not
// seen. Each used credit, in time order, attaches to its id's latest
// earlier show of its session neither credited nor settled; late counts
// the used credits since the last settle event that found none. A show
// whose memory was forgotten at or after it settles as forgotten.
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
	forgotten := map[string]time.Time{} // id: its latest forget
	for _, e := range SortByTime(seen) {
		switch e.Kind {
		case "turn":
			turns[e.Session] = append(turns[e.Session], e)
		case "shows":
			if e.Path == PathSources {
				continue // only lets the next verdict name a listed fact: never settled
			}
			for _, s := range ShowsOf(e) {
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
			if e.Verdict != "used" && e.Verdict != "confirmed" {
				continue // confirmed credits like used: the session checked the memory
			}
			for _, id := range e.IDs {
				if !attach(byID[e.Session+"|"+id]) && e.TS.After(lastSettle) {
					late++
				}
			}
		case "stop":
			stopped[e.Session+"|"+e.Turn+"|"+e.Agent] = true
		case "forget":
			for _, id := range e.IDs {
				forgotten[id] = laterOf(forgotten[id], e.TS)
			}
		}
	}
	for _, s := range shows {
		age := now.Sub(s.At)
		if done[s.Key] || age < p.TurnTimeout {
			continue
		}
		turn := ownTurn(s.Show, turns[s.session])
		if age < OpenTurnLimit && !turnOver(s, turn, turns[s.session], stopped) {
			continue
		}
		forgot, wasForgotten := forgotten[s.ID]
		outcome := OutcomeShown
		switch {
		case wasForgotten && !forgot.Before(s.At):
			outcome = OutcomeForgotten
		case s.credited:
			outcome = OutcomeCredit
		case !declared[s.session+"|"+turn+"|"+s.Agent]:
			outcome = OutcomeUnknown
		case s.Rank >= 1 && s.Rank <= p.ConfidentRank && missPath(s.Path):
			outcome = OutcomeMiss
		}
		st := Settled{Key: s.Key, ID: s.ID, At: s.At, Outcome: outcome}
		if s.Path == PathStore {
			st.Path = PathStore
		}
		settled = append(settled, st)
	}
	return settled, late
}

// Replay is the ledger events would give had every show been decided
// under p: the settle events they hold left out, every show decided
// again at now by Decide (a sources show never), and the outcomes
// folded at now with the import, forget, retire, hide and unhide events,
// as Fold does. Events after now are not seen. Pure: it writes nothing.
func Replay(events []Event, p Params, now time.Time) Ledger {
	var kept []Event
	for _, e := range events {
		if e.Kind != "settle" && !e.TS.After(now) {
			kept = append(kept, e)
		}
	}
	settled, _ := Decide(kept, p, now)
	return Fold(append(kept, Event{Kind: "settle", TS: now.UTC(), Settled: settled}), p)
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

// lockLedgerWait is lockLedger, trying again while a live owner holds
// the lock, for at most wait.
func lockLedgerWait(dir string, wait time.Duration) (release func(), err error) {
	deadline := time.Now().Add(wait)
	for {
		release, err = lockLedger(dir)
		var held *state.HeldError
		if !errors.As(err, &held) || !time.Now().Before(deadline) {
			return release, err
		}
		time.Sleep(10 * time.Millisecond)
	}
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
