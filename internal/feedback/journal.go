// Package feedback records which memories were shown to Claude and which it
// used, in an append-only journal of ids, numbers and enums (never memory,
// prompt or reply text).
package feedback

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// Schema is the journal line format this binary writes and reads.
const Schema = 1

// Item is one memory of a show, with its place in the list shown and the
// project it belongs to (empty when the path does not tell, and on lines
// written before the field). Boost is what the ledger added to its score,
// RankWithoutBoost its place in the same list without it (both empty when
// recall had no ledger, and on lines written before the fields).
// ScoreWithoutBoost is its score before the boost: the server's score
// with the current-project boost, what feedback replay orders a list by
// again; nil on lines written before the field (a score of 0 is written).
type Item struct {
	ID                string   `json:"id"`
	Rank              int      `json:"rank,omitempty"`
	Reranker          float64  `json:"reranker,omitempty"`
	Project           string   `json:"project,omitempty"`
	Boost             float64  `json:"boost,omitempty"`
	RankWithoutBoost  int      `json:"rank_without_boost,omitempty"`
	ScoreWithoutBoost *float64 `json:"score_without_boost,omitempty"`
}

// Settled is the outcome a settle gave one show: credit, confident_miss,
// shown or unknown. Key names the show (session, turn, call, id). An
// import event's items are Settled too, shown or credit, with the show's
// rank.
type Settled struct {
	Key     string    `json:"key"`
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	Rank    int       `json:"rank,omitempty"`
}

// Event is one journal line. Kind is turn, shows, credit, stop, settle,
// forget, retire, hide, unhide, stale, recall (KindRecall) or import
// (KindImport); each kind fills the fields it needs.
// Note is the correction note a retire or hide comes with (its id, never
// its text). Source is where an import event's shows were found
// (telemetry, transcripts).
type Event struct {
	Schema    int                `json:"schema"`
	Seq       int64              `json:"seq"`
	TS        time.Time          `json:"ts"`
	Kind      string             `json:"kind"`
	Session   string             `json:"session,omitempty"`
	Turn      string             `json:"turn,omitempty"`
	Agent     string             `json:"agent,omitempty"`
	Project   string             `json:"project,omitempty"`
	Binary    string             `json:"binary_version,omitempty"`
	V         string             `json:"v,omitempty"`
	N         int                `json:"n,omitempty"`
	Machine   bool               `json:"machine,omitempty"`
	Path      string             `json:"path,omitempty"`
	Call      string             `json:"call,omitempty"`
	Items     []Item             `json:"items,omitempty"`
	IDs       []string           `json:"ids,omitempty"`
	Verdict   string             `json:"verdict,omitempty"`
	Outcome   string             `json:"outcome,omitempty"`
	StopCheck string             `json:"stop_check,omitempty"`
	Flags     []string           `json:"flags,omitempty"`
	Settled   []Settled          `json:"settled,omitempty"`
	Params    map[string]float64 `json:"params,omitempty"`
	Note      string             `json:"note,omitempty"`
	Source    string             `json:"source,omitempty"`
}

// Kinds of the events that retire a memory shown in a session (a raw fact
// invalidated, an observation hidden): each counts toward the session's
// feedback.max_retirements. Unhide undoes a retire or a hide.
const (
	KindRetire = "retire"
	KindHide   = "hide"
	KindUnhide = "unhide"
)

// KindStale: the memories `backsight memory stale` listed to the user.
// The ledger does not fold it.
const KindStale = "stale"

// KindRecall: what a memory_search returned, with each hit's boost and
// rank without it, and whether the boost changed the set or the order
// (FlagSetChanged, FlagOrderChanged). An audit: neither settled nor
// folded; the hook journals the same hits as shows.
const (
	KindRecall       = "recall"
	FlagSetChanged   = "set_changed"
	FlagOrderChanged = "order_changed"
)

// Common fields Append puts on every event that leaves them empty.
type Common struct {
	Session, Project, Binary, V string
}

// seq numbers the lines of this process: (session, seq) is unique even when
// several processes write the same journal.
var seq atomic.Int64

// Journal appends events to <dir>/journal-YYYY-MM.jsonl, the UTC month of
// each event's ts.
type Journal struct {
	dir    string
	common Common
	now    func() time.Time
}

func NewJournal(dir string, c Common) *Journal {
	return &Journal{dir: dir, common: c, now: time.Now}
}

func (j *Journal) file(t time.Time) string { return filepath.Join(j.dir, monthFile(t)) }

// monthFile is the base name of the journal file of t's UTC month.
func monthFile(t time.Time) string { return "journal-" + t.UTC().Format("2006-01") + ".jsonl" }

// Append writes e as one line of its ts's month file, after setting its
// schema, its seq, its ts (now, unless set) and the common fields it
// leaves empty. Events the ledger folds as they come go through
// AppendFolded. Callers fail open on its error: a lost event never fails a
// hook or a recall.
func (j *Journal) Append(e Event) error {
	e.Schema, e.Seq = Schema, seq.Add(1)
	if e.TS.IsZero() {
		e.TS = j.now().UTC()
	}
	fill := func(field *string, v string) {
		if *field == "" {
			*field = v
		}
	}
	fill(&e.Session, j.common.Session)
	fill(&e.Project, j.common.Project)
	fill(&e.Binary, j.common.Binary)
	fill(&e.V, j.common.V)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := fsutil.MkdirPrivate(j.dir); err != nil {
		return err
	}
	f, err := os.OpenFile(j.file(e.TS), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	// A crash mid-write leaves a line without its newline: start a line of
	// our own, so only the torn one is lost. Racing another writer, this
	// can add an empty line, which readers ignore.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	// One write of the whole line: O_APPEND keeps lines from several
	// processes whole on local filesystems.
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// Position is where the current month's journal ends: ReadFrom(file, offset)
// later returns what was appended since.
func (j *Journal) Position() (file string, offset int64) {
	file = j.file(j.now())
	if fi, err := os.Stat(file); err == nil {
		offset = fi.Size()
	}
	return file, offset
}

// Read returns the events of every month file from from's month on, in
// file order. Lines that do not parse or carry another schema are skipped
// and counted, empty lines ignored; a missing dir is no events.
func Read(dir string, from time.Time) (events []Event, skipped int, err error) {
	files, err := filepath.Glob(filepath.Join(dir, "journal-*.jsonl"))
	if err != nil {
		return nil, 0, err
	}
	slices.Sort(files) // journal-YYYY-MM sorts by month
	first := monthFile(from)
	for _, f := range files {
		if filepath.Base(f) < first {
			continue
		}
		ev, sk, err := ReadFrom(f, 0)
		events, skipped = append(events, ev...), skipped+sk
		if err != nil {
			return events, skipped, err
		}
	}
	return events, skipped, nil
}

// decodedLine, when set (tests), runs for each line ReadFrom decodes.
var decodedLine func()

// ReadFrom returns the events of file from byte offset on; a missing file is
// no events.
func ReadFrom(file string, offset int64) (events []Event, skipped int, err error) {
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, err
	}
	r := bufio.NewReader(f)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if decodedLine != nil {
				decodedLine()
			}
			var e Event
			if json.Unmarshal(line, &e) != nil || e.Schema != Schema {
				skipped++
			} else {
				events = append(events, e)
			}
		}
		if errors.Is(rerr, io.EOF) {
			return events, skipped, nil
		}
		if rerr != nil {
			return events, skipped, rerr
		}
	}
}

// ReadSince returns the events of file from byte offset on, then those of
// the later month files beside it: a turn that began before a month's end
// goes on in the next file.
func ReadSince(file string, offset int64) (events []Event, skipped int, err error) {
	if file == "" {
		return nil, 0, errors.New("no journal position")
	}
	if events, skipped, err = ReadFrom(file, offset); err != nil {
		return events, skipped, err
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(file), "journal-*.jsonl"))
	if err != nil {
		return events, skipped, err
	}
	slices.Sort(files)
	for _, f := range files {
		if filepath.Base(f) <= filepath.Base(file) {
			continue
		}
		ev, sk, err := ReadFrom(f, 0)
		events, skipped = append(events, ev...), skipped+sk
		if err != nil {
			return events, skipped, err
		}
	}
	return events, skipped, nil
}

// foldedLockWait: how long AppendFolded waits for a settle to release the
// ledger lock (a settle reads and writes local files only). A variable so
// tests can shrink it.
var foldedLockWait = 2 * time.Second

// AppendFolded appends an event the ledger folds as it comes (forget,
// retire, hide, unhide, import) under the ledger lock, stamped at its ts
// (now, unless set) or just after the ledger's watermark when that is
// later.
// Settle reads the journal and moves the watermark under the same lock,
// and folds only what is newer: the event is either read by a settle or
// stamped after it, as a rebuild folds it. When the lock stays held, the
// event is journaled all the same (the journal counts a session's
// retirements) and the lock's error returned, for the caller to log.
func (j *Journal) AppendFolded(e Event) error {
	release, lerr := lockLedgerWait(j.dir, foldedLockWait)
	if lerr == nil {
		defer release()
	}
	if e.TS.IsZero() {
		e.TS = j.now().UTC()
	}
	if l, err := LoadLedger(j.dir); err == nil && !e.TS.After(l.Watermark) {
		e.TS = l.Watermark.Add(time.Nanosecond)
	}
	if err := j.Append(e); err != nil {
		return err
	}
	return lerr
}
