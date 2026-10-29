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
)

// Schema is the journal line format this binary writes and reads.
const Schema = 1

// Item is one memory of a show, with its place in the list shown and the
// project it belongs to (empty when the path does not tell, and on lines
// written before the field).
type Item struct {
	ID       string  `json:"id"`
	Rank     int     `json:"rank,omitempty"`
	Reranker float64 `json:"reranker,omitempty"`
	Project  string  `json:"project,omitempty"`
}

// Settled is the outcome a settle gave one show: credit, confident_miss,
// shown or unknown. Key names the show (session, turn, call, id).
type Settled struct {
	Key     string    `json:"key"`
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
}

// Event is one journal line. Kind is turn, shows, credit, stop, settle,
// forget, retire, hide or unhide; each kind fills the fields it needs.
// Note is the correction note a retire or hide comes with (its id, never
// its text).
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
}

// Kinds of the events that retire a memory shown in a session (a raw fact
// invalidated, an observation hidden): each counts toward the session's
// feedback.max_retirements. Unhide undoes a retire or a hide.
const (
	KindRetire = "retire"
	KindHide   = "hide"
	KindUnhide = "unhide"
)

// Common fields Append puts on every event that leaves them empty.
type Common struct {
	Session, Project, Binary, V string
}

// seq numbers the lines of this process: (session, seq) is unique even when
// several processes write the same journal.
var seq atomic.Int64

// Journal appends events to <dir>/journal-YYYY-MM.jsonl (UTC month).
type Journal struct {
	dir    string
	common Common
	now    func() time.Time
}

func NewJournal(dir string, c Common) *Journal {
	return &Journal{dir: dir, common: c, now: time.Now}
}

func (j *Journal) file(t time.Time) string {
	return filepath.Join(j.dir, "journal-"+t.UTC().Format("2006-01")+".jsonl")
}

// Append writes e as one line, after setting its schema, its seq, its ts
// (now, unless set) and the common fields it leaves empty. Callers fail
// open on its error: a lost event never fails a hook or a recall.
func (j *Journal) Append(e Event) error {
	now := j.now()
	e.Schema, e.Seq = Schema, seq.Add(1)
	if e.TS.IsZero() {
		e.TS = now.UTC()
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
	if err := os.MkdirAll(j.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(j.file(now), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
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
	first := "journal-" + from.UTC().Format("2006-01") + ".jsonl"
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
