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
// MissesAt (each event weighs 2^-(age/half-life)), and when it was last
// shown and credited.
type Entry struct {
	Shown        int       `json:"shown"`
	Credits      float64   `json:"credits,omitempty"`
	CreditsAt    time.Time `json:"credits_at,omitzero"`
	Misses       float64   `json:"misses,omitempty"`
	MissesAt     time.Time `json:"misses_at,omitzero"`
	LastShown    time.Time `json:"last_shown,omitzero"`
	LastCredited time.Time `json:"last_credited,omitzero"`
}

// Ledger is a cache of the journal's settle and forget events, by memory
// id: Rebuild recomputes it. Watermark is when settling last ran.
type Ledger struct {
	Schema    int              `json:"schema"`
	Watermark time.Time        `json:"watermark,omitzero"`
	Memories  map[string]Entry `json:"memories"`
}

func newLedger() Ledger { return Ledger{Schema: LedgerSchema, Memories: map[string]Entry{}} }

// Apply adds a settle event's shows to the ledger, or drops a forget
// event's memories. Sums decay order-independently, so applying settle
// events as they come and folding them all later agree.
func (l *Ledger) Apply(e Event, p Params) {
	if l.Memories == nil {
		l.Memories = map[string]Entry{}
	}
	switch e.Kind {
	case "settle":
		for _, s := range e.Settled {
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
	}
}

// Fold is the ledger of events' settle and forget events, applied in
// (ts, seq) order; its watermark is the last settle's time.
func Fold(events []Event, p Params) Ledger {
	l := newLedger()
	for _, e := range sortedByTime(events) {
		l.Apply(e, p)
		if e.Kind == "settle" {
			l.Watermark = laterOf(l.Watermark, e.TS)
		}
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
	if h <= 0 {
		return v + 1, laterOf(ref, t)
	}
	if !t.After(ref) {
		return v + math.Exp2(-float64(ref.Sub(t))/float64(h)), ref
	}
	return v*math.Exp2(-float64(t.Sub(ref))/float64(h)) + 1, t
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
// writes the result only when write.
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
