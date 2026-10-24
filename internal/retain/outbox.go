package retain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// Kinds a note can have (SPEC §4).
var Kinds = map[string]bool{"decision": true, "correction": true, "convention": true, "pitfall": true, "state": true, "lesson": true}

// Outbox size limits (SPEC §9.1); variables so tests can shrink them.
var (
	OutboxWarnBytes int64 = 50 << 20
	OutboxMaxBytes  int64 = 500 << 20
)

// ErrOutboxFull refuses a note when the outbox reached its hard limit.
var ErrOutboxFull = errors.New("the notes outbox is full (500 MB): the server has been unreachable for long; check backsight doctor")

// Note is one queued explicit fact.
type Note struct {
	OperationID string    `json:"operation_id"`
	DocumentID  string    `json:"document_id"`
	Kind        string    `json:"kind"`
	Project     string    `json:"project"`
	Content     string    `json:"content"`
	CreatedAt   time.Time `json:"created_at"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error,omitempty"`
}

// NewNote builds a note with its document and operation ids.
func NewNote(bank, kind, project, content string, at time.Time) Note {
	doc := "note:" + uuid5(nsBacksight, bank+"\n"+project+"\n"+at.UTC().Format(time.RFC3339Nano)+"\n"+content)
	sum := sha256.Sum256([]byte(content))
	return Note{OperationID: uuid5(nsBacksight, bank+"\n"+doc+"\n"+hex.EncodeToString(sum[:])), DocumentID: doc,
		Kind: kind, Project: project, Content: content, CreatedAt: at.UTC()}
}

// Item is the retain item of a note.
func (n Note) Item(machineID string) hindsight.RetainItem {
	ts := n.CreatedAt
	return hindsight.RetainItem{
		Content: n.Content, Context: "note stored by the user or the agent", DocumentID: n.DocumentID, Strategy: "document",
		Tags:              []string{"project:" + n.Project, "source:note", "kind:" + n.Kind, "harness:claude-code"},
		ObservationScopes: [][]string{{"project:" + n.Project}},
		Timestamp:         &ts,
		Metadata:          map[string]string{"source": "note", "kind": n.Kind, "machine_id": machineID},
	}
}

// Outbox is the write-ahead file of notes not yet accepted by the server.
type Outbox struct {
	Path, LockPath, QuarantinePath string
}

func (o Outbox) lock(command string) (func(), error) {
	var l *state.Lock
	var err error
	for i := 0; i < 50; i++ { // short waits: notes are tiny
		if l, err = state.TryAcquire(o.LockPath, command); err == nil {
			return func() { _ = l.Release() }, nil
		}
		var held *state.HeldError
		if errors.As(err, &held) && held.Stale && state.BreakStale(held) == nil {
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, err
}

// Append writes n durably. warn is true past the warning size.
func (o Outbox) Append(n Note) (warn bool, err error) {
	unlock, err := o.lock("store")
	if err != nil {
		return false, err
	}
	defer unlock()
	var size int64
	if fi, err := os.Stat(o.Path); err == nil {
		size = fi.Size()
	}
	if size >= OutboxMaxBytes {
		return true, ErrOutboxFull
	}
	raw, err := json.Marshal(n)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(o.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	return size+int64(len(raw)) >= OutboxWarnBytes, f.Close()
}

// FlushResult counts what Flush did.
type FlushResult struct {
	Sent        int    `json:"sent"`
	Kept        int    `json:"kept"`
	Quarantined int    `json:"quarantined"`
	LastError   string `json:"last_error,omitempty"`
}

// Flush sends every queued note; retryable failures stay queued.
func (o Outbox) Flush(ctx context.Context, c *hindsight.Client, bank, machineID string) (FlushResult, error) {
	var res FlushResult
	unlock, err := o.lock("notes flush")
	if err != nil {
		return res, err
	}
	defer unlock()
	notes, err := o.read()
	if err != nil || len(notes) == 0 {
		return res, err
	}
	var keep []Note
	for i, n := range notes {
		err := c.Retain(ctx, bank, []hindsight.RetainItem{n.Item(machineID)}, n.OperationID)
		var apiErr *hindsight.APIError
		switch {
		case err == nil:
			res.Sent++
		case errors.As(err, &apiErr) && !apiErr.Retryable():
			res.Quarantined++
			n.LastError = err.Error()
			_ = appendJSON(o.QuarantinePath, n)
		default:
			// Unreachable or busy: keep this note and the rest untouched.
			n.Attempts++
			n.LastError = err.Error()
			res.LastError = n.LastError
			keep = append(keep, n)
			keep = append(keep, notes[i+1:]...)
			res.Kept = len(keep)
			return res, o.rewrite(keep)
		}
	}
	return res, o.rewrite(keep)
}

func (o Outbox) read() ([]Note, error) {
	raw, err := os.ReadFile(o.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var notes []Note
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var n Note
		if len(bytes.TrimSpace(sc.Bytes())) == 0 || json.Unmarshal(sc.Bytes(), &n) != nil {
			continue // a torn last line from a crash is dropped, the rest kept
		}
		notes = append(notes, n)
	}
	return notes, sc.Err()
}

func (o Outbox) rewrite(notes []Note) error {
	var buf bytes.Buffer
	for _, n := range notes {
		raw, _ := json.Marshal(n)
		buf.Write(append(raw, '\n'))
	}
	return fsutil.WriteAtomic(o.Path, buf.Bytes(), 0o600)
}

// Pending counts queued notes.
func (o Outbox) Pending() (int, error) {
	notes, err := o.read()
	return len(notes), err
}

func appendJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s: %w", path, err)
	}
	return f.Close()
}
