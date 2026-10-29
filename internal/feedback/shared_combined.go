package feedback

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// <state>/feedback/shared-combined is CombineRemote of the cached
// documents, which jobs tick writes with each refresh, so that a hook
// looks up the memories it ranks instead of parsing every document. A
// header line "v1 <unix s combined at> <credit half-life ns> <miss
// half-life ns> <unix s until>" (CombinedBasis; an older binary wrote
// the time alone), then one line per memory sorted by id:
// "<id>\t<credits>\t<credits_at>\t<misses>\t<misses_at>\t<hidden_since>",
// times in unix seconds (0 for none). Each sum keeps its reference time:
// decay composes, so reading it later equals combining the documents
// then.
const combinedHeader = "v1 "

// CombinedBasis is what a combined view was built under: the half-lives,
// and when the first of the documents it counts turns sharedMaxAge old
// (zero: it counts none). Past that time, or under other half-lives, the
// view is no longer the documents combined (SharedCombinedOutdated).
type CombinedBasis struct {
	CreditHalfLife, MissHalfLife time.Duration
	Until                        time.Time
}

// BasisOf is the basis of CombineRemote(docs, self, now, p).
func BasisOf(docs []SharedDoc, self string, now time.Time, p Params) CombinedBasis {
	b := CombinedBasis{CreditHalfLife: p.CreditHalfLife, MissHalfLife: p.MissHalfLife}
	for _, d := range newestCurrent(docs, self, now) {
		if until := d.At.Add(sharedMaxAge); b.Until.IsZero() || until.Before(b.Until) {
			b.Until = until
		}
	}
	return b
}

// errCombinedStale: a combined view older than sharedMaxAge, or dated
// more than sharedMaxSkew ahead: no longer the other machines' counts.
var errCombinedStale = errors.New("combined shared counts out of date")

// SharedCombinedFile is where dir (the feedback directory) keeps the
// combined view.
func SharedCombinedFile(dir string) string { return filepath.Join(dir, "shared-combined") }

// SaveSharedCombined replaces dir's combined view with r, combined at at
// under basis, in one rename.
func SaveSharedCombined(dir string, at time.Time, r Remote, basis CombinedBasis) error {
	var b strings.Builder
	until := int64(0)
	if !basis.Until.IsZero() {
		until = basis.Until.Unix()
	}
	fmt.Fprintf(&b, "%s%d %d %d %d\n", combinedHeader, at.Unix(), int64(basis.CreditHalfLife), int64(basis.MissHalfLife), until)
	num := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	for _, id := range slices.Sorted(maps.Keys(r)) {
		e := r[id]
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\n", id, num(e.Credits), num(unixSeconds(e.CreditsAt)), num(e.Misses), num(unixSeconds(e.MissesAt)), num(unixSeconds(e.HiddenSince)))
	}
	return fsutil.WriteAtomic(SharedCombinedFile(dir), []byte(b.String()), 0o600)
}

// Combined is the other machines' counts as jobs tick last combined
// them, read in one file read: Get finds a memory by binary search and
// parses its line alone, so a hook pays for the memories it ranks.
type Combined struct {
	at    time.Time
	basis *CombinedBasis // nil: an older binary's view
	body  []byte         // the sorted lines
}

// LoadSharedCombined reads dir's combined view: an error when it is
// missing, not of this version, or older than sharedMaxAge or more than
// sharedMaxSkew ahead at now.
func LoadSharedCombined(dir string, now time.Time) (Combined, error) {
	c, err := readCombined(dir)
	if err != nil {
		return Combined{}, err
	}
	if now.Sub(c.at) > sharedMaxAge || c.at.After(now.Add(sharedMaxSkew)) {
		return Combined{}, errCombinedStale
	}
	return c, nil
}

// readCombined reads dir's combined view, however old: an error when it
// is missing or not of this version.
func readCombined(dir string) (Combined, error) {
	raw, err := os.ReadFile(SharedCombinedFile(dir))
	if err != nil {
		return Combined{}, err
	}
	head, body, ok := bytes.Cut(raw, []byte("\n"))
	if !ok || !bytes.HasPrefix(head, []byte(combinedHeader)) {
		return Combined{}, ErrSharedVersion
	}
	f := strings.Fields(string(head[len(combinedHeader):]))
	if len(f) == 0 {
		return Combined{}, errors.New("combined shared counts: no time")
	}
	sec, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return Combined{}, fmt.Errorf("combined shared counts: %w", err)
	}
	c := Combined{at: time.Unix(sec, 0).UTC(), body: body}
	if len(f) == 4 {
		var v [3]int64
		var errs [3]error
		for i := range v {
			v[i], errs[i] = strconv.ParseInt(f[i+1], 10, 64)
		}
		if errors.Join(errs[:]...) == nil {
			c.basis = &CombinedBasis{CreditHalfLife: time.Duration(v[0]), MissHalfLife: time.Duration(v[1])}
			if v[2] != 0 {
				c.basis.Until = time.Unix(v[2], 0).UTC()
			}
		}
	}
	return c, nil
}

// SharedCombinedOutdated: dir's combined view is to combine again at now
// under p: it is missing or unreadable, an older binary wrote it (no
// basis), it was built under other half-lives than p's, or a document it
// counts turned sharedMaxAge old since.
func SharedCombinedOutdated(dir string, now time.Time, p Params) bool {
	c, err := readCombined(dir)
	if err != nil || c.basis == nil {
		return true
	}
	b := *c.basis
	return b.CreditHalfLife != p.CreditHalfLife || b.MissHalfLife != p.MissHalfLife || (!b.Until.IsZero() && now.After(b.Until))
}

// Empty: c holds no memory.
func (c Combined) Empty() bool { return len(c.body) == 0 }

// At is when the tick combined c.
func (c Combined) At() time.Time { return c.at }

// Get is what c holds of id; false when nothing, or its line does not
// read (a number or a time out of range).
func (c Combined) Get(id string) (SharedEntry, bool) {
	lo, hi := 0, len(c.body)
	for lo < hi {
		mid := (lo + hi) / 2
		start := bytes.LastIndexByte(c.body[:mid], '\n') + 1
		end := bytes.IndexByte(c.body[start:], '\n')
		if end < 0 {
			end = len(c.body)
		} else {
			end += start
		}
		line := c.body[start:end]
		key, _, _ := bytes.Cut(line, []byte("\t"))
		switch strings.Compare(string(key), id) {
		case 0:
			return combinedEntry(line)
		case -1:
			lo = end + 1
		default:
			hi = start
		}
	}
	return SharedEntry{}, false
}

// Remote is all of c, lines that do not read left out.
func (c Combined) Remote() Remote {
	r := Remote{}
	for line := range bytes.SplitSeq(c.body, []byte("\n")) {
		if id, _, _ := bytes.Cut(line, []byte("\t")); len(id) > 0 {
			if e, ok := combinedEntry(line); ok {
				r[string(id)] = e
			}
		}
	}
	return r
}

// combinedEntry reads one line of the combined view.
func combinedEntry(line []byte) (SharedEntry, bool) {
	f := strings.Split(string(line), "\t")
	if len(f) != 6 {
		return SharedEntry{}, false
	}
	var v [5]float64
	for i, s := range f[1:] {
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return SharedEntry{}, false
		}
		v[i] = x
	}
	if !(v[0] >= 0 && v[0] <= sharedMaxCount*sharedMaxEntries && v[2] >= 0 && v[2] <= sharedMaxCount*sharedMaxEntries) {
		return SharedEntry{}, false
	}
	var e SharedEntry
	var errs [3]error
	e.Credits, e.Misses = v[0], v[2]
	e.CreditsAt, errs[0] = sharedTime(v[1])
	e.MissesAt, errs[1] = sharedTime(v[3])
	e.HiddenSince, errs[2] = sharedTime(v[4])
	return e, errors.Join(errs[:]...) == nil
}
