package feedback

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// LedgerReader is how anything but the folding reads the ledger, the
// lines journaled since its last fold applied: the ledger database, or
// CurrentReader (ledger.json) when the database cannot be read.
type LedgerReader interface {
	// Get is the entries of ids, only those that have one.
	Get(ids []string) (map[string]Entry, error)
	// Hidden is every entry with Hidden set.
	Hidden() (map[string]Entry, error)
	// All is the whole ledger.
	All() (Ledger, error)
	// Any reports whether the ledger holds any entry at all.
	Any() (bool, error)
	// GetAny is Get and Any in one read: what recall asks at each search.
	GetAny(ids []string) (map[string]Entry, bool, error)
}

// CurrentReader reads Dir's ledger as Current does: it never takes the
// ledger lock, so a settle in progress never makes it wait.
type CurrentReader struct{ Dir string }

// All is Current(r.Dir).
func (r CurrentReader) All() (Ledger, error) { return Current(r.Dir) }

// Get is the entries of Current(r.Dir) among ids.
func (r CurrentReader) Get(ids []string) (map[string]Entry, error) {
	m, _, err := r.GetAny(ids)
	return m, err
}

// Hidden is the hidden entries of Current(r.Dir).
func (r CurrentReader) Hidden() (map[string]Entry, error) {
	l, err := Current(r.Dir)
	if err != nil {
		return nil, err
	}
	out := map[string]Entry{}
	for id, e := range l.Memories {
		if e.Hidden != nil {
			out[id] = e
		}
	}
	return out, nil
}

// Any reports whether Current(r.Dir) holds an entry.
func (r CurrentReader) Any() (bool, error) {
	l, err := Current(r.Dir)
	return len(l.Memories) > 0, err
}

// GetAny is Get and Any over one read of Current(r.Dir).
func (r CurrentReader) GetAny(ids []string) (map[string]Entry, bool, error) {
	l, err := Current(r.Dir)
	if err != nil {
		return nil, false, err
	}
	out := map[string]Entry{}
	for _, id := range ids {
		if e, ok := l.Memories[id]; ok {
			out[id] = e
		}
	}
	return out, len(l.Memories) > 0, nil
}

// Stamp is what All reads, as the directory lists it: each file's name,
// size and mtime, and the newest mtime. Two equal stamps mean All reads
// the same only when no write can share an mtime with the first one: the
// caller trusts a stamp whose newest mtime is old enough.
type Stamp struct {
	Files  []StampFile
	Newest time.Time
}

// StampFile is one file of a Stamp; ModTime is in nanoseconds.
type StampFile struct {
	Name    string
	Size    int64
	ModTime int64
}

// Test seams.
var (
	stampStat    = os.Stat
	stampReadDir = os.ReadDir
)

// Stamp stamps ledger.json and every journal file of r.Dir (each name
// All's glob reads, journal-YYYY-MM.jsonl), and the file ledger.json's
// journal_at names when it is another (only a hand-edited ledger names
// one; Size -1 while it does not exist), sorted by name. A missing
// ledger.json is left out, a missing directory is the empty stamp (All
// reads an empty ledger there). A file listed but not stat'ed, or a
// ledger.json that does not parse, is an error: no stamp then.
func (r CurrentReader) Stamp() (Stamp, error) {
	ents, err := stampReadDir(r.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Stamp{}, nil
	}
	if err != nil {
		return Stamp{}, err
	}
	var st Stamp
	add := func(name string, fi os.FileInfo) {
		st.Files = append(st.Files, StampFile{Name: name, Size: fi.Size(), ModTime: fi.ModTime().UnixNano()})
		if fi.ModTime().After(st.Newest) {
			st.Newest = fi.ModTime()
		}
	}
	ledger := false
	for _, e := range ents {
		name := e.Name()
		if !stampedName(name) {
			continue
		}
		fi, err := stampStat(filepath.Join(r.Dir, name)) // follows a link, as All's reads do
		if err != nil {
			return Stamp{}, err
		}
		add(name, fi)
		ledger = ledger || name == "ledger.json"
	}
	if ledger {
		name, err := journalAtFile(r.Dir)
		if err != nil {
			return Stamp{}, err
		}
		if name != "" && filepath.Base(name) == name && !stampedName(name) { // as journalSince reads it
			fi, err := stampStat(filepath.Join(r.Dir, name))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				st.Files = append(st.Files, StampFile{Name: name, Size: -1})
			case err != nil:
				return Stamp{}, err
			default:
				add(name, fi)
			}
		}
	}
	slices.SortFunc(st.Files, func(a, b StampFile) int { return cmp.Compare(a.Name, b.Name) })
	return st, nil
}

// stampedName: ledger.json, or a name All's journal glob matches.
func stampedName(name string) bool {
	journal, _ := filepath.Match("journal-*.jsonl", name)
	return journal || name == "ledger.json"
}

// journalAtFile is the file name dir's ledger.json gives as journal_at,
// decoded as LoadLedger decodes it (the same key rules) but without the
// memories.
func journalAtFile(dir string) (string, error) {
	raw, err := os.ReadFile(ledgerFile(dir))
	if err != nil {
		return "", err
	}
	var h struct {
		JournalAt JournalPos `json:"journal_at"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", err
	}
	return h.JournalAt.File, nil
}

// Equal: the same files with the same sizes and mtimes, and the same
// newest mtime.
func (s Stamp) Equal(o Stamp) bool {
	return slices.Equal(s.Files, o.Files) && s.Newest.Equal(o.Newest)
}
