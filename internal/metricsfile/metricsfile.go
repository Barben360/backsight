// Package metricsfile appends the dashboard's local records as JSONL month
// files <dir>/<family>-YYYY-MM.jsonl (UTC month of ts): ids, numbers, enums
// and times only, never a text. Lines are written with one O_APPEND write
// each; readers skip torn lines.
package metricsfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/instructions"
)

const tsLayout = "2006-01-02T15:04:05.000Z"

// Writer appends records under Dir.
type Writer struct {
	Dir           string
	BinaryVersion string
	V             string // instruction-set version
	Now           func() time.Time
}

// New is a Writer for dir (the metrics directory).
func New(dir, binaryVersion string) *Writer {
	return &Writer{Dir: dir, BinaryVersion: binaryVersion, V: instructions.Version, Now: time.Now}
}

func monthFile(dir, family string, t time.Time) string {
	return filepath.Join(dir, family+"-"+t.UTC().Format("2006-01")+".jsonl")
}

// Append writes each record as one line carrying the common fields; all the
// lines of a call share one ts. Every record is checked first: when one is
// refused nothing is written.
func (w *Writer) Append(recs ...Record) error {
	if len(recs) == 0 {
		return nil
	}
	for _, r := range recs {
		if err := validate(r); err != nil {
			return err
		}
	}
	now := w.Now().UTC()
	type out struct {
		path string
		line []byte
	}
	lines := make([]out, 0, len(recs))
	for _, r := range recs {
		raw, err := json.Marshal(utc(r))
		if err != nil {
			return err
		}
		fields := map[string]any{}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // integers above 2^53 stay exact
		if err := dec.Decode(&fields); err != nil {
			return err
		}
		fields["schema"] = Schema
		fields["ts"] = now.Format(tsLayout)
		fields["kind"] = r.Kind()
		fields["v"] = w.V
		fields["binary_version"] = w.BinaryVersion
		line, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		lines = append(lines, out{monthFile(w.Dir, r.Family(), now), append(line, '\n')})
	}
	if err := fsutil.MkdirPrivate(w.Dir); err != nil {
		return err
	}
	for _, l := range lines {
		if err := appendLine(l.path, l.line); err != nil {
			return err
		}
	}
	return nil
}

// appendLine is one O_APPEND write of the whole line.
func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	n, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if n != len(line) {
		return fmt.Errorf("metricsfile: short write")
	}
	return cerr
}
