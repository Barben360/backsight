package feedback

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	// OutcomeForgotten: its memory was forgotten at or after the show,
	// before it settled. Settled so that it is never decided again, and
	// counted nowhere.
	OutcomeForgotten = "forgotten"
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

// record is p as a settle event carries it.
func (p Params) record() map[string]float64 {
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
// turn by its agent or the main agent) or the show openTurnLimit old. Events after now are not
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
	for _, e := range sortedByTime(seen) {
		switch e.Kind {
		case "turn":
			turns[e.Session] = append(turns[e.Session], e)
		case "shows":
			if e.Path == PathSources {
				continue // only lets the next verdict name a listed fact: never settled
			}
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
		if age < openTurnLimit && !turnOver(s, turn, turns[s.session], stopped) {
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
		settled = append(settled, Settled{Key: s.Key, ID: s.ID, At: s.At, Outcome: outcome})
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

// afterSettleRead, when set (tests), runs once Settle has read the journal.
var afterSettleRead func()

// Settle decides the shows of dir's journal that can settle at now, the
// clock read once it holds the ledger lock, journals them as one settle
// event carrying p, and brings ledger.json up to date: the settle,
// forget, retire, hide and unhide events since its watermark, in time
// order, each settle key once. Under the ledger lock: a concurrent settle
// gets a *state.HeldError and changes nothing. A ledger of another schema
// is left alone, and nothing is journaled; one that does not parse is
// kept aside and rebuilt from the journal first (rebuildCorrupt).
func Settle(dir string, p Params, clock func() time.Time) (SettleResult, error) {
	var res SettleResult
	release, err := lockLedger(dir)
	if err != nil {
		return res, err
	}
	defer release()
	now := clock()
	l, err := LoadLedger(dir)
	if errors.Is(err, ErrCorruptLedger) {
		l, res.Rebuilt, err = rebuildCorrupt(dir, p, now)
	}
	if err != nil {
		return res, err
	}
	at := journalEnd(dir)
	events, _, err := Read(dir, now.Add(-settleWindow))
	if err != nil {
		return res, err
	}
	if afterSettleRead != nil {
		afterSettleRead()
	}
	var apply []Event
	applied := map[string]bool{} // the keys of the settle events the ledger holds
	for _, e := range events {
		switch {
		case e.Kind == "settle" && !e.TS.After(l.Watermark):
			for _, s := range e.Settled {
				applied[s.Key] = true
			}
		case (e.Kind == "settle" || folded(e.Kind)) && e.TS.After(l.Watermark) && !e.TS.After(now):
			apply = append(apply, e) // a ledger write that failed, and forgets and retirements since the last settle
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
		if e.Kind == "settle" {
			e = unapplied(e, applied) // a settle run while its lock was wrongly broken
		}
		l.Apply(e, p)
	}
	l.Watermark = now.UTC()
	// Current reads on from where this read began, unless a folded event
	// before it is newer than the watermark: left for Current to apply,
	// it needs the read from the watermark. So does a journal that already
	// ends in a later month than now's: a folded event stamped just after
	// the watermark goes to now's month, before that end.
	l.JournalAt = at
	if at.File > monthFile(now) ||
		slices.ContainsFunc(events, func(e Event) bool { return folded(e.Kind) && e.TS.After(now) }) {
		l.JournalAt = JournalPos{}
	}
	if err := saveLedger(dir, l); err != nil {
		return res, err
	}
	res.Late = late
	for _, s := range settled {
		if s.Outcome == OutcomeForgotten {
			continue
		}
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

// rebuildCorrupt rebuilds dir's ledger from the journal, as feedback
// rebuild does, copies the ledger.json that did not parse to
// ledger.json.corrupt-<now in unix s> (a -2, -3… suffix when an earlier
// copy has that name: none is ever replaced), and writes the rebuilt one
// over it atomically; it returns the ledger and the copy's name. For a
// caller holding the ledger lock. A journal it cannot read, or a copy it
// cannot make, leaves the file as it was, for the next settle to try
// again.
func rebuildCorrupt(dir string, p Params, now time.Time) (Ledger, string, error) {
	l, err := rebuild(dir, p, true)
	if err != nil {
		return l, "", err
	}
	raw, err := os.ReadFile(ledgerFile(dir))
	if err != nil {
		return l, "", err
	}
	aside, err := keepAside(ledgerFile(dir)+".corrupt-"+strconv.FormatInt(now.Unix(), 10), raw)
	if err != nil {
		return l, "", err
	}
	return l, filepath.Base(aside), saveLedger(dir, l)
}

// keepAside writes raw to a new file named base, or base-2, base-3… when
// that name is taken, and returns its path.
func keepAside(base string, raw []byte) (string, error) {
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name += "-" + strconv.Itoa(n)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) && n < 100 {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(raw)
		if serr := f.Sync(); werr == nil {
			werr = serr
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(name)
		}
		return name, werr
	}
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
