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
// header line "v1 <unix s combined at>", then one line per memory sorted
// by id: "<id>\t<credits>\t<credits_at>\t<misses>\t<misses_at>\t<hidden_since>",
// times in unix seconds (0 for none). Each sum keeps its reference time:
// decay composes, so reading it later equals combining the documents
// then.
const combinedHeader = "v1 "

// errCombinedStale: a combined view older than sharedMaxAge, or dated
// more than sharedMaxSkew ahead: no longer the other machines' counts.
var errCombinedStale = errors.New("combined shared counts out of date")

// SharedCombinedFile is where dir (the feedback directory) keeps the
// combined view.
func SharedCombinedFile(dir string) string { return filepath.Join(dir, "shared-combined") }

// SaveSharedCombined replaces dir's combined view with r, combined at at,
// in one rename.
func SaveSharedCombined(dir string, at time.Time, r Remote) error {
	var b strings.Builder
	b.WriteString(combinedHeader + strconv.FormatInt(at.Unix(), 10) + "\n")
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
	at   time.Time
	body []byte // the sorted lines
}

// LoadSharedCombined reads dir's combined view: an error when it is
// missing, not of this version, or older than sharedMaxAge or more than
// sharedMaxSkew ahead at now.
func LoadSharedCombined(dir string, now time.Time) (Combined, error) {
	raw, err := os.ReadFile(SharedCombinedFile(dir))
	if err != nil {
		return Combined{}, err
	}
	head, body, ok := bytes.Cut(raw, []byte("\n"))
	if !ok || !bytes.HasPrefix(head, []byte(combinedHeader)) {
		return Combined{}, ErrSharedVersion
	}
	sec, err := strconv.ParseInt(string(head[len(combinedHeader):]), 10, 64)
	if err != nil {
		return Combined{}, fmt.Errorf("combined shared counts: %w", err)
	}
	at := time.Unix(sec, 0).UTC()
	if now.Sub(at) > sharedMaxAge || at.After(now.Add(sharedMaxSkew)) {
		return Combined{}, errCombinedStale
	}
	return Combined{at: at, body: body}, nil
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
