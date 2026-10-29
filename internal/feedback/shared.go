package feedback

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

// SharedVersion is the shared document format this binary writes and
// reads; a reader skips a document of another.
const SharedVersion = 1

// A shared document holds at most sharedMaxEntries memories, leaving out
// one not hidden whose credits and misses both decayed below
// sharedMinCount; one whose at is older than sharedMaxAge is a machine no
// longer in use, ignored. A reader refuses a document of more than
// sharedMaxBytes or sharedMaxEntries, with a count below 0 or above
// sharedMaxCount, or a time before sharedEpoch or after sharedLastTime;
// it ignores one dated, or holding a time, more than sharedMaxSkew ahead
// of its clock.
const (
	sharedMaxEntries = 5000
	sharedMinCount   = 0.01
	sharedMaxAge     = 30 * 24 * time.Hour
	sharedMaxBytes   = 4 << 20
	sharedMaxCount   = 1e6
	sharedMaxSkew    = 24 * time.Hour
)

var (
	sharedEpoch    = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	sharedLastTime = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	// memoryID is a memory id as the server gives it, a lowercase UUID.
	memoryID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ErrSharedVersion: a shared document of a version this binary does not
// read, to skip.
var ErrSharedVersion = errors.New("shared ledger document of an unknown version")

// SharedDoc is what one machine's ledger tells the others through the
// user's server: by memory id, its credits and misses, each a decayed sum
// with its reference time, and since when it is hidden there. Memory
// ids, numbers and times only.
type SharedDoc struct {
	V       int
	Machine string
	At      time.Time
	Entries map[string]SharedEntry
}

// SharedEntry is one memory of a SharedDoc or a Remote; a zero
// HiddenSince: not hidden.
type SharedEntry struct {
	Credits     float64
	CreditsAt   time.Time
	Misses      float64
	MissesAt    time.Time
	HiddenSince time.Time
}

// sharedWire is a SharedDoc as its JSON: times in unix seconds (0 for a
// zero time), each entry [credits, credits_at, misses, misses_at,
// hidden_since].
type sharedWire struct {
	V       int                  `json:"v"`
	Machine string               `json:"machine"`
	At      float64              `json:"at"`
	Entries map[string][]float64 `json:"entries"`
}

// MarshalMeta is d as the server's metadata value: its JSON, floats
// rounded to 4 decimals, times in whole unix seconds.
func (d SharedDoc) MarshalMeta() (string, error) {
	w := sharedWire{V: d.V, Machine: d.Machine, At: unixSeconds(d.At), Entries: make(map[string][]float64, len(d.Entries))}
	for id, e := range d.Entries {
		w.Entries[id] = []float64{round4(e.Credits), unixSeconds(e.CreditsAt), round4(e.Misses), unixSeconds(e.MissesAt), unixSeconds(e.HiddenSince)}
	}
	raw, err := json.Marshal(w)
	return string(raw), err
}

// ParseSharedDoc reads a metadata value MarshalMeta wrote. A document of
// another version is ErrSharedVersion, whatever its entries hold; one too
// large, with a count or a time out of range, or a malformed entry is an
// error, never partly read; an entry under an id that is not a memory id
// is skipped.
func ParseSharedDoc(s string) (SharedDoc, error) {
	if len(s) > sharedMaxBytes {
		return SharedDoc{}, fmt.Errorf("shared ledger document of %d bytes, over %d", len(s), sharedMaxBytes)
	}
	var head struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal([]byte(s), &head); err != nil {
		return SharedDoc{}, fmt.Errorf("shared ledger document: %w", err)
	}
	if head.V != SharedVersion {
		return SharedDoc{}, fmt.Errorf("%w: v %d", ErrSharedVersion, head.V)
	}
	var w sharedWire
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		return SharedDoc{}, fmt.Errorf("shared ledger document: %w", err)
	}
	if len(w.Entries) > sharedMaxEntries {
		return SharedDoc{}, fmt.Errorf("shared ledger document of %d entries, over %d", len(w.Entries), sharedMaxEntries)
	}
	at, err := sharedTime(w.At)
	if err != nil || at.IsZero() {
		return SharedDoc{}, fmt.Errorf("shared ledger document: at %v out of range", w.At)
	}
	d := SharedDoc{V: w.V, Machine: w.Machine, At: at, Entries: make(map[string]SharedEntry, len(w.Entries))}
	for id, a := range w.Entries {
		if !memoryID.MatchString(id) {
			continue
		}
		e, err := sharedEntry(a)
		if err != nil {
			return SharedDoc{}, fmt.Errorf("shared ledger document: memory %s: %w", id, err)
		}
		d.Entries[id] = e
	}
	return d, nil
}

// sharedEntry is one entry's [credits, credits_at, misses, misses_at,
// hidden_since], each value in range.
func sharedEntry(a []float64) (SharedEntry, error) {
	if len(a) != 5 {
		return SharedEntry{}, fmt.Errorf("%d values, want 5", len(a))
	}
	for _, c := range []float64{a[0], a[2]} {
		if !(c >= 0 && c <= sharedMaxCount) {
			return SharedEntry{}, fmt.Errorf("count %v out of range", c)
		}
	}
	var times [3]time.Time
	for i, v := range []float64{a[1], a[3], a[4]} {
		t, err := sharedTime(v)
		if err != nil {
			return SharedEntry{}, err
		}
		times[i] = t
	}
	return SharedEntry{Credits: a[0], CreditsAt: times[0], Misses: a[2], MissesAt: times[1], HiddenSince: times[2]}, nil
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}

// sharedTime is unix seconds s as a time: 0 a zero time, else between
// sharedEpoch and sharedLastTime.
func sharedTime(s float64) (time.Time, error) {
	if s == 0 {
		return time.Time{}, nil
	}
	if !(s >= float64(sharedEpoch.Unix()) && s <= float64(sharedLastTime.Unix())) {
		return time.Time{}, fmt.Errorf("time %v out of range", s)
	}
	return time.Unix(int64(math.Round(s)), 0).UTC(), nil
}

// Export is l as machine's shared document at now: each memory's sums at
// their own reference times, so that the same ledger exports the same
// entries later; the hidden memories, then the most recently shown (id
// order between equals), sharedMaxEntries at most, none whose credits and
// misses both decayed below sharedMinCount at now unless hidden.
func (l Ledger) Export(machine string, now time.Time, p Params) SharedDoc {
	ids := make([]string, 0, len(l.Memories))
	for id, e := range l.Memories {
		if d := e.DecayedTo(now, p); e.Hidden != nil || d.Credits >= sharedMinCount || d.Misses >= sharedMinCount {
			ids = append(ids, id)
		}
	}
	notHidden := func(e Entry) bool { return e.Hidden == nil }
	slices.SortFunc(ids, func(a, b string) int {
		x, y := l.Memories[a], l.Memories[b]
		return cmp.Or(compareBool(notHidden(x), notHidden(y)), y.LastShown.Compare(x.LastShown), strings.Compare(a, b))
	})
	doc := SharedDoc{V: SharedVersion, Machine: machine, At: now, Entries: make(map[string]SharedEntry, min(len(ids), sharedMaxEntries))}
	for _, id := range ids[:min(len(ids), sharedMaxEntries)] {
		e := l.Memories[id]
		s := SharedEntry{Credits: e.Credits, CreditsAt: e.CreditsAt, Misses: e.Misses, MissesAt: e.MissesAt}
		if e.Hidden != nil {
			s.HiddenSince = e.Hidden.Since
		}
		doc.Entries[id] = s
	}
	return doc
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// Remote is what the other machines' documents say of each memory: their
// credits and misses summed, read at the time they were combined, and
// the latest of their hidings.
type Remote map[string]SharedEntry

// CombineRemote sums docs into a Remote at now, under p's half-lives:
// the newest document of each machine but self, among those neither
// older than sharedMaxAge nor dated or holding a time more than
// sharedMaxSkew ahead of now.
func CombineRemote(docs []SharedDoc, self string, now time.Time, p Params) Remote {
	newest := newestCurrent(docs, self, now)
	r := Remote{}
	for _, m := range slices.Sorted(maps.Keys(newest)) {
		for id, e := range newest[m].Entries {
			r[id] = r[id].plus(e, now, p)
		}
	}
	return r
}

// newestCurrent is the newest document of each machine but self in docs,
// among those current at now: those CombineRemote sums.
func newestCurrent(docs []SharedDoc, self string, now time.Time) map[string]SharedDoc {
	newest := map[string]SharedDoc{}
	for _, d := range docs {
		if d.Machine == self || !d.current(now) {
			continue
		}
		if n, ok := newest[d.Machine]; !ok || d.At.After(n.At) {
			newest[d.Machine] = d
		}
	}
	return newest
}

// Expired: d's at is more than sharedMaxAge before now, a machine no
// longer in use that every reader ignores.
func (d SharedDoc) Expired(now time.Time) bool { return now.Sub(d.At) > sharedMaxAge }

// current: d is at most sharedMaxAge old at now, and neither it nor any
// of its times more than sharedMaxSkew ahead.
func (d SharedDoc) current(now time.Time) bool {
	late := now.Add(sharedMaxSkew)
	if d.Expired(now) || d.At.After(late) {
		return false
	}
	for _, e := range d.Entries {
		if e.CreditsAt.After(late) || e.MissesAt.After(late) || e.HiddenSince.After(late) {
			return false
		}
	}
	return true
}

// WithRemote is a copy of l whose memories r names have their credits
// and misses read at now with r's added, and r's hiding when not hidden
// here and newer than the memory's last unhide here; a memory only r
// names gets an entry never shown here. Every other field stays as it
// is: the copy serves recall, never saved.
func (l Ledger) WithRemote(r Remote, p Params, now time.Time) Ledger {
	out := l
	out.Memories = make(map[string]Entry, len(l.Memories)+len(r))
	maps.Copy(out.Memories, l.Memories)
	for id, s := range r {
		out.Memories[id] = l.Memories[id].WithRemote(s, p, now)
	}
	return out
}

// WithRemote is e (a zero Entry for a memory never shown here) with s's
// credits and misses added, both read at now, and s's hiding when e is
// not hidden and s's is newer than e's last unhide: one memory of
// Ledger.WithRemote.
func (e Entry) WithRemote(s SharedEntry, p Params, now time.Time) Entry {
	sum := SharedEntry{Credits: e.Credits, CreditsAt: e.CreditsAt, Misses: e.Misses, MissesAt: e.MissesAt}.plus(s, now, p)
	e.Credits, e.CreditsAt, e.Misses, e.MissesAt = sum.Credits, sum.CreditsAt, sum.Misses, sum.MissesAt
	if e.Hidden == nil && s.HiddenSince.After(e.UnhiddenAt) {
		e.Hidden = &Hidden{Since: s.HiddenSince}
	}
	return e
}

// plus is s with e's credits and misses added, both read at now, and the
// later of their hidings.
func (s SharedEntry) plus(e SharedEntry, now time.Time, p Params) SharedEntry {
	s.Credits, s.CreditsAt = merged(s.Credits, s.CreditsAt, e.Credits, e.CreditsAt, now, p.CreditHalfLife)
	s.Misses, s.MissesAt = merged(s.Misses, s.MissesAt, e.Misses, e.MissesAt, now, p.MissHalfLife)
	s.HiddenSince = laterOf(s.HiddenSince, e.HiddenSince)
	return s
}

// merged is v referenced at ref plus w referenced at wref, each read at
// now, referenced at the later of their times (later than now only for a
// sum referenced in the future); a zero sum adds nothing, its time
// neither.
func merged(v float64, ref time.Time, w float64, wref, now time.Time, h time.Duration) (float64, time.Time) {
	v, ref = readAt(v, ref, now, h)
	w, wref = readAt(w, wref, now, h)
	switch {
	case w == 0:
		return v, ref
	case v == 0:
		return w, wref
	}
	t := laterOf(ref, wref)
	return decay(v, ref, t, h) + decay(w, wref, t, h), t
}
