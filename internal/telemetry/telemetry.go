// Package telemetry appends local JSONL events (SPEC §15, Appendix A.4).
// Nothing is ever sent anywhere.
package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Common fields carried by every event.
type Common struct {
	V             string // instruction-set version
	Session       string
	Project       string
	BinaryVersion string
}

// Event is one telemetry record; its JSON fields are added to the common ones.
type Event interface{ EventName() string }

// Writer appends events to <state>/telemetry/events-YYYY-MM.jsonl (UTC month).
type Writer struct {
	dir      string
	common   Common
	now      func() time.Time
	failures atomic.Int64
}

func New(stateDir string, c Common) *Writer {
	return &Writer{dir: filepath.Join(stateDir, "telemetry"), common: c, now: time.Now}
}

// Failures counts events that could not be written by this Writer.
func (w *Writer) Failures() int64 { return w.failures.Load() }

func (w *Writer) currentFile() string { return w.file(w.now()) }

func (w *Writer) file(t time.Time) string {
	return filepath.Join(w.dir, "events-"+t.UTC().Format("2006-01")+".jsonl")
}

// Emit appends e. It never fails the caller: errors are only counted.
func (w *Writer) Emit(e Event) {
	if err := w.emit(e); err != nil {
		w.failures.Add(1)
	}
}

func (w *Writer) emit(e Event) error {
	now := w.now()
	fields := map[string]any{}
	if r, ok := e.(Raw); ok {
		for k, v := range r.Fields {
			fields[k] = v
		}
	} else {
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
	}
	fields["ts"] = now.UTC().Format("2006-01-02T15:04:05.000Z")
	fields["v"] = w.common.V
	fields["event"] = e.EventName()
	fields["session"] = w.common.Session
	fields["project"] = w.common.Project
	fields["binary_version"] = w.common.BinaryVersion
	line, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(w.file(now), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
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
