package feedback

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// LedgerReader is how anything but the folding reads the ledger. Today:
// CurrentReader, ledger.json plus the folded lines since its watermark,
// without the ledger lock. Block 7 swaps the implementation, nothing else.
type LedgerReader interface{ All() (Ledger, error) }

// CurrentReader reads Dir's ledger as Current does: it never takes the
// ledger lock, so a settle in progress never makes it wait.
type CurrentReader struct{ Dir string }

// All is Current(r.Dir).
func (r CurrentReader) All() (Ledger, error) { return Current(r.Dir) }

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
// All's glob reads, journal-YYYY-MM.jsonl), sorted by name. A missing
// ledger.json is left out, a missing directory is the empty stamp (All
// reads an empty ledger there). A file listed but not stat'ed is an
// error: no stamp then.
func (r CurrentReader) Stamp() (Stamp, error) {
	ents, err := stampReadDir(r.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Stamp{}, nil
	}
	if err != nil {
		return Stamp{}, err
	}
	var st Stamp
	for _, e := range ents {
		name := e.Name()
		if journal, _ := filepath.Match("journal-*.jsonl", name); !journal && name != "ledger.json" {
			continue
		}
		fi, err := stampStat(filepath.Join(r.Dir, name)) // follows a link, as All's reads do
		if err != nil {
			return Stamp{}, err
		}
		st.Files = append(st.Files, StampFile{Name: name, Size: fi.Size(), ModTime: fi.ModTime().UnixNano()})
		if fi.ModTime().After(st.Newest) {
			st.Newest = fi.ModTime()
		}
	}
	slices.SortFunc(st.Files, func(a, b StampFile) int { return cmp.Compare(a.Name, b.Name) })
	return st, nil
}

// Equal: the same files with the same sizes and mtimes, and the same
// newest mtime.
func (s Stamp) Equal(o Stamp) bool {
	return slices.Equal(s.Files, o.Files) && s.Newest.Equal(o.Newest)
}
