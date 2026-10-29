package feedback

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// LedgerSchema is the ledger.json format this binary writes and reads.
const LedgerSchema = 1

// Entry is what the settled journal says of one memory: how often it was
// shown, its credits and confident misses as sums decayed to CreditsAt and
// MissesAt (each event weighs 2^-(age/half-life)), when it was last shown
// and credited, how often it was retired as wrong or outdated, and its
// local hiding when its retirement could only hide it.
type Entry struct {
	Shown        int       `json:"shown"`
	Credits      float64   `json:"credits,omitempty"`
	CreditsAt    time.Time `json:"credits_at,omitzero"`
	Misses       float64   `json:"misses,omitempty"`
	MissesAt     time.Time `json:"misses_at,omitzero"`
	LastShown    time.Time `json:"last_shown,omitzero"`
	LastCredited time.Time `json:"last_credited,omitzero"`
	Wrong        int       `json:"wrong,omitempty"`
	Hidden       *Hidden   `json:"hidden,omitempty"`
}

// Hidden is an observation hidden on this machine: since when, under which
// verdict, with the correction note's id.
type Hidden struct {
	Since   time.Time `json:"since"`
	Verdict string    `json:"verdict,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// Ledger is a cache of the journal's settle, forget, retire, hide and
// unhide events, by memory id: Rebuild recomputes it. Watermark is when settling last ran.
type Ledger struct {
	Schema    int              `json:"schema"`
	Watermark time.Time        `json:"watermark,omitzero"`
	Memories  map[string]Entry `json:"memories"`
}

func newLedger() Ledger { return Ledger{Schema: LedgerSchema, Memories: map[string]Entry{}} }

// Apply adds a settle event's shows to the ledger, drops a forget event's
// memories, counts a retire, sets a hide, or clears both on an unhide (of
// a memory with an entry only). A settle decays under the half-lives it recorded, p's
// when it recorded none; a forgotten show counts nowhere. Sums decay
// order-independently, so applying settle events as they come and folding
// them all later agree.
func (l *Ledger) Apply(e Event, p Params) {
	if l.Memories == nil {
		l.Memories = map[string]Entry{}
	}
	switch e.Kind {
	case "settle":
		p = p.recorded(e.Params)
		for _, s := range e.Settled {
			if s.Outcome == OutcomeForgotten {
				continue
			}
			m := l.Memories[s.ID]
			m.Shown++
			m.LastShown = laterOf(m.LastShown, s.At)
			switch s.Outcome {
			case OutcomeCredit:
				m.Credits, m.CreditsAt = add(m.Credits, m.CreditsAt, s.At, p.CreditHalfLife)
				m.LastCredited = laterOf(m.LastCredited, s.At)
			case OutcomeMiss:
				m.Misses, m.MissesAt = add(m.Misses, m.MissesAt, s.At, p.MissHalfLife)
			}
			l.Memories[s.ID] = m
		}
	case "forget":
		for _, id := range e.IDs {
			delete(l.Memories, id)
		}
	case KindRetire:
		for _, id := range e.IDs {
			m := l.Memories[id]
			m.Wrong++
			l.Memories[id] = m
		}
	case KindHide:
		for _, id := range e.IDs {
			m := l.Memories[id]
			m.Hidden = &Hidden{Since: e.TS, Verdict: e.Verdict, Note: e.Note}
			l.Memories[id] = m
		}
	case KindUnhide:
		for _, id := range e.IDs {
			if m, ok := l.Memories[id]; ok {
				m.Wrong, m.Hidden = 0, nil
				l.Memories[id] = m
			}
		}
	}
}

// folded: an event Settle applies to the ledger as it comes, besides its
// own settle events.
func folded(kind string) bool {
	switch kind {
	case "forget", KindRetire, KindHide, KindUnhide:
		return true
	}
	return false
}

// recorded is p with the half-lives a settle event's params carry.
func (p Params) recorded(params map[string]float64) Params {
	if d, ok := params[missHalfLifeKey]; ok {
		p.MissHalfLife = days(d)
	}
	if d, ok := params[creditHalfLifeKey]; ok {
		p.CreditHalfLife = days(d)
	}
	return p
}

func days(d float64) time.Duration { return time.Duration(d * float64(24*time.Hour)) }

// Fold is the ledger of events' settle, forget, retire, hide and unhide
// events, applied in
// (ts, seq) order, each settle key once; its watermark is the last
// settle's time.
func Fold(events []Event, p Params) Ledger {
	l := newLedger()
	applied := map[string]bool{}
	for _, e := range sortedByTime(events) {
		if e.Kind == "settle" {
			var fresh []Settled
			for _, s := range e.Settled {
				if !applied[s.Key] {
					applied[s.Key] = true
					fresh = append(fresh, s)
				}
			}
			e.Settled = fresh
			l.Watermark = laterOf(l.Watermark, e.TS)
		}
		l.Apply(e, p)
	}
	return l
}

// sortedByTime is a copy of events in (ts, seq) order, file order kept
// between equals.
func sortedByTime(events []Event) []Event {
	out := slices.Clone(events)
	slices.SortStableFunc(out, func(a, b Event) int {
		if c := a.TS.Compare(b.TS); c != 0 {
			return c
		}
		return int(a.Seq - b.Seq)
	})
	return out
}

// add returns the decayed sum after adding 1 at time t to v referenced at
// ref: whatever the order of the additions, the sum ends referenced at the
// latest t with the same value.
func add(v float64, ref, t time.Time, h time.Duration) (float64, time.Time) {
	if !t.After(ref) {
		return v + decay(1, t, ref, h), ref
	}
	return decay(v, ref, t, h) + 1, t
}

// decay is v referenced at ref, read at t: halved every h. A t before ref,
// or a half-life of 0, leaves v as it is.
func decay(v float64, ref, t time.Time, h time.Duration) float64 {
	if h <= 0 || !t.After(ref) {
		return v
	}
	return v * math.Exp2(-float64(t.Sub(ref))/float64(h))
}

// DecayedTo is e with its credits and misses read at t under p's
// half-lives, each referenced at t from then on (a t before a reference
// leaves that sum as it is).
func (e Entry) DecayedTo(t time.Time, p Params) Entry {
	if e.Credits != 0 && t.After(e.CreditsAt) {
		e.Credits, e.CreditsAt = decay(e.Credits, e.CreditsAt, t, p.CreditHalfLife), t
	}
	if e.Misses != 0 && t.After(e.MissesAt) {
		e.Misses, e.MissesAt = decay(e.Misses, e.MissesAt, t, p.MissHalfLife), t
	}
	return e
}

// Boost is what recall adds to id's score: k × ((c + α) / (c + m + α + β)
// − α / (α + β)), c and m its credits and misses decayed to now, k, α
// and β p's Weight, PriorCredits and PriorMisses. Between −k × α / (α + β)
// and k × β / (α + β); exactly 0 for a memory without an entry, or with
// nothing settled, and for priors that do not add up to a positive sum.
func (l Ledger) Boost(id string, p Params, now time.Time) float64 {
	m, ok := l.Memories[id]
	a, b := p.PriorCredits, p.PriorMisses
	if !ok || a+b <= 0 {
		return 0
	}
	m = m.DecayedTo(now, p)
	return p.Weight * ((m.Credits+a)/(m.Credits+m.Misses+a+b) - a/(a+b))
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func ledgerFile(dir string) string { return filepath.Join(dir, "ledger.json") }

// LoadLedger reads dir's ledger. A missing file is an empty ledger; one of
// another schema is an empty ledger and an error, for doctor to report:
// never guessed.
func LoadLedger(dir string) (Ledger, error) {
	raw, err := os.ReadFile(ledgerFile(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return newLedger(), nil
	}
	if err != nil {
		return newLedger(), err
	}
	var l Ledger
	if err := json.Unmarshal(raw, &l); err != nil {
		return newLedger(), fmt.Errorf("%s: %w", ledgerFile(dir), err)
	}
	if l.Schema != LedgerSchema {
		return newLedger(), fmt.Errorf("%s has schema %d, this binary reads %d", ledgerFile(dir), l.Schema, LedgerSchema)
	}
	if l.Memories == nil {
		l.Memories = map[string]Entry{}
	}
	return l, nil
}

func saveLedger(dir string, l Ledger) error {
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(ledgerFile(dir), raw, 0o600)
}

// Rebuild folds every journal file of dir, under the ledger lock, and
// writes the result only when write. Each settle event folds with the
// half-lives it recorded, so a rebuild after a config change gives the
// live ledger back; p's half-lives serve only settle events that recorded
// none. Recomputing under other parameters is not a rebuild.
func Rebuild(dir string, p Params, write bool) (Ledger, error) {
	release, err := lockLedger(dir)
	if err != nil {
		return Ledger{}, err
	}
	defer release()
	events, _, err := Read(dir, time.Time{})
	if err != nil {
		return Ledger{}, err
	}
	l := Fold(events, p)
	if write {
		if err := saveLedger(dir, l); err != nil {
			return l, err
		}
	}
	return l, nil
}

// Current is dir's ledger with the forget, retire, hide and unhide events
// journaled since its watermark applied, in time order, written nowhere:
// what a command reads between two settles.
func Current(dir string) (Ledger, error) {
	l, err := LoadLedger(dir)
	if err != nil {
		return l, err
	}
	events, _, err := Read(dir, l.Watermark)
	if err != nil {
		return l, err
	}
	for _, e := range sortedByTime(events) {
		if folded(e.Kind) && e.TS.After(l.Watermark) {
			l.Apply(e, Params{})
		}
	}
	return l, nil
}
