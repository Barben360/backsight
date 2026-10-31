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
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// Kinds a note can have (SPEC §4).
var Kinds = map[string]bool{"decision": true, "correction": true, "convention": true, "pitfall": true, "state": true, "lesson": true}

// Default outbox size limits (SPEC §9.1), the defaults of hooks.outbox_warn_mb
// and hooks.outbox_max_mb.
const (
	DefaultOutboxWarnBytes int64 = 50 << 20
	DefaultOutboxMaxBytes  int64 = 500 << 20
)

// ErrOutboxFull refuses a note when the outbox reached its hard limit.
var ErrOutboxFull = errors.New("the notes outbox is full: the server has been unreachable for long; check backsight doctor")

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
	Source      string    `json:"source,omitempty"` // "" (a note) | subagent
	AgentType   string    `json:"agent_type,omitempty"`
	// Attribution: why a subagent's report has its project (the path
	// rule's source), kept in the document's metadata with the project so
	// the decision survives this machine's state (decision 0072).
	Attribution string `json:"attribution,omitempty"`
	// Session: the Claude Code session a note was stored from ("" from a
	// shell), kept in the document's metadata so `memory notes` shows it.
	Session string `json:"session,omitempty"`
	// Replaces and Evidence: the memories a correction reported wrong or
	// outdated replaces, and what in the session showed it, kept in the
	// document's metadata.
	Replaces []string `json:"replaces,omitempty"`
	Evidence string   `json:"evidence,omitempty"`
}

// NewNote builds a note with its document and operation ids.
// NewSubagentNote is a subagent's final report as document
// subagent:<session>:<agent> (SPEC §6.2 SubagentStop).
func NewSubagentNote(bank, sid, agentID, agentType, project, content string, at time.Time) Note {
	doc := "subagent:" + sid + ":" + agentID
	sum := sha256.Sum256([]byte(content))
	return Note{OperationID: uuid5(nsBacksight, bank+"\n"+doc+"\n"+hex.EncodeToString(sum[:])), DocumentID: doc,
		Source: "subagent", AgentType: agentType, Project: project, Content: content, CreatedAt: at.UTC()}
}

func NewNote(bank, kind, project, content string, at time.Time) Note {
	doc := "note:" + uuid5(nsBacksight, bank+"\n"+project+"\n"+at.UTC().Format(time.RFC3339Nano)+"\n"+content)
	sum := sha256.Sum256([]byte(content))
	return Note{OperationID: uuid5(nsBacksight, bank+"\n"+doc+"\n"+hex.EncodeToString(sum[:])), DocumentID: doc,
		Kind: kind, Project: project, Content: content, CreatedAt: at.UTC()}
}

// Item is the retain item of a note.
func (n Note) Item(machineID string) hindsight.RetainItem {
	ts := n.CreatedAt
	if n.Source == "subagent" {
		item := hindsight.RetainItem{
			Content: n.Content, Context: "final report of a subagent", DocumentID: n.DocumentID, Strategy: "subagent",
			Tags:              []string{"project:" + n.Project, "source:subagent", "harness:claude-code"},
			ObservationScopes: [][]string{{"project:" + n.Project}},
			Timestamp:         &ts,
			Metadata:          map[string]string{"source": "subagent", "machine_id": machineID},
		}
		if t := n.AgentType; t != "" {
			// On the raw facts only: the scope stays the project (decision 0071).
			item.Tags = append(item.Tags, "agent:"+t)
			item.Metadata["agent_type"] = t
		}
		if n.Attribution != "" {
			item.Metadata["attribution"], item.Metadata["project"] = n.Attribution, n.Project
		}
		return item
	}
	item := hindsight.RetainItem{
		Content: n.Content, Context: "note stored by the user or the agent", DocumentID: n.DocumentID, Strategy: "document",
		Tags:              []string{"project:" + n.Project, "source:note", "kind:" + n.Kind, "harness:claude-code"},
		ObservationScopes: [][]string{{"project:" + n.Project}},
		Timestamp:         &ts,
		Metadata:          map[string]string{"source": "note", "kind": n.Kind, "machine_id": machineID},
	}
	if n.Session != "" {
		item.Metadata["session"] = n.Session
	}
	if len(n.Replaces) > 0 {
		item.Metadata["replaces"] = strings.Join(n.Replaces, ",")
	}
	if n.Evidence != "" {
		item.Metadata["evidence"] = n.Evidence
	}
	return item
}

// Outbox is the write-ahead file of notes not yet accepted by the server.
type Outbox struct {
	Path, LockPath, QuarantinePath string
	// WarnBytes and MaxBytes are the warning and hard size limits; zero:
	// the defaults.
	WarnBytes, MaxBytes int64
	// Gate, when not nil, is asked before each note's send and again
	// after it: an error stops the flush, that note and every later one
	// kept as they were (no attempt counted), and Flush returns it. A
	// note the gate refuses after its send went to the server the flush
	// began with: it stays queued for the next one (operation ids are
	// per server).
	Gate func() error
	// LockDeadline, when set, is when a wait for the lock gives up,
	// instead of lockWait after it began: a caller with a hard deadline
	// of its own (the SubagentStop hook's child) passes what it can spend.
	LockDeadline time.Time
}

// WithLimitsMB returns o with the limits of hooks.outbox_warn_mb and
// hooks.outbox_max_mb (a value of 0 or less keeps the default).
func (o Outbox) WithLimitsMB(warn, max int) Outbox {
	if warn > 0 {
		o.WarnBytes = int64(warn) << 20
	}
	if max > 0 {
		o.MaxBytes = int64(max) << 20
	}
	return o
}

// Limits are the warning and hard size limits in bytes.
func (o Outbox) Limits() (warn, max int64) {
	warn, max = o.WarnBytes, o.MaxBytes
	if warn <= 0 {
		warn = DefaultOutboxWarnBytes
	}
	if max <= 0 {
		max = DefaultOutboxMaxBytes
	}
	return warn, max
}

// lockWait is how long an outbox operation waits for the lock unless its
// caller bounds the wait (LockDeadline, Flush's context). The lock is held
// by an append (the lock file's create, the line's write and fsync, the
// lock file's delete: about 5 ms on the Windows VM's SSD, up to about
// 100 ms on windows-latest's OS disk) and by a flush's read and rewrite.
// Fifty tries of 20 ms (about 1 s) let a burst of 20 appends on that disk
// refuse 5 (CI); 3 s covers about 30 of its appends. Callers without a hard
// deadline of their own (store, the jobs, backfill) wait that long.
const lockWait = 3 * time.Second

// lock takes the outbox lock, waiting until LockDeadline (when set, else
// lockWait from now) or ctx's deadline, whichever comes first; ctx ending
// stops the wait.
func (o Outbox) lock(ctx context.Context, command string) (func(), error) {
	deadline := o.LockDeadline
	if deadline.IsZero() {
		deadline = time.Now().Add(lockWait)
	}
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		l, err := state.TryAcquire(o.LockPath, command)
		if err == nil {
			return func() { _ = l.Release() }, nil
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		var held *state.HeldError
		if errors.As(err, &held) && held.Stale && state.BreakStale(held) == nil {
			continue
		}
		select { // short waits: notes are tiny
		case <-ctx.Done():
			return nil, err
		case <-time.After(min(20*time.Millisecond, time.Until(deadline))):
		}
	}
}

// whileAppending runs in Append with the lock held (tests make an append
// slow).
var whileAppending = func() {}

// Append writes n durably. warn is true past the warning size.
func (o Outbox) Append(n Note) (warn bool, err error) {
	unlock, err := o.lock(context.Background(), "store")
	if err != nil {
		return false, err
	}
	defer unlock()
	var size int64
	if fi, err := os.Stat(o.Path); err == nil {
		size = fi.Size()
	}
	warnAt, maxAt := o.Limits()
	if size >= maxAt {
		return true, ErrOutboxFull
	}
	raw, err := json.Marshal(n)
	if err != nil {
		return false, err
	}
	if err := fsutil.MkdirPrivate(filepath.Dir(o.Path)); err != nil {
		return false, err
	}
	f, err := os.OpenFile(o.Path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	line := append(append([]byte{}, raw...), '\n')
	if size > 0 { // after a torn last line, start a new one
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, size-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return false, err
	}
	whileAppending()
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	return size+int64(len(raw)) >= warnAt, f.Close()
}

// FlushResult counts what Flush did.
type FlushResult struct {
	Sent        int    `json:"sent"`
	Kept        int    `json:"kept"`
	Quarantined int    `json:"quarantined"`
	LastError   string `json:"last_error,omitempty"`
	// SentDocs and QuarantinedDocs name the notes this flush sent or
	// quarantined, so a caller can tell what became of its own note.
	SentDocs        []string `json:"-"`
	QuarantinedDocs []string `json:"-"`
	// Outcomes is what became of each note this flush tried, in order: the
	// ones sent, set aside, and the one whose failure stopped it (it stays
	// queued, Retry). The notes after it were not tried.
	Outcomes []NoteOutcome `json:"-"`
}

// NoteOutcome is one note a flush tried: its document, Sent, Quarantined or
// Retry, and how long its send took.
type NoteOutcome struct {
	Document string
	Status   string
	MS       int
}

// MaxAttempts: a note failing this many times (about half a day of ticks)
// goes to quarantine instead of blocking every note queued after it.
const MaxAttempts = 50

// Flush sends every queued note; retryable failures stay queued. The lock
// is held only to read and rewrite the file, never during network calls,
// so store can append meanwhile.
func (o Outbox) Flush(ctx context.Context, c *hindsight.Client, bank, machineID string) (FlushResult, error) {
	var res FlushResult
	unlock, err := o.lock(ctx, "notes flush")
	if err != nil {
		return res, err
	}
	notes, err := o.read()
	unlock()
	if err != nil || len(notes) == 0 {
		return res, err
	}
	done := map[string]bool{}   // sent or quarantined: remove
	failed := map[string]Note{} // kept with its new attempt count
	var gateErr error
	for _, n := range notes {
		if o.Gate != nil {
			if gateErr = o.Gate(); gateErr != nil {
				break // this note and the rest stay as they are
			}
		}
		start := time.Now()
		err := c.Retain(ctx, bank, []hindsight.RetainItem{n.Item(machineID)}, n.OperationID)
		ms := int(time.Since(start).Milliseconds())
		if err == nil && o.Gate != nil {
			if gateErr = o.Gate(); gateErr != nil {
				break // connect started over during the send: kept for the new server
			}
		}
		if err == nil {
			res.Sent++
			res.SentDocs = append(res.SentDocs, n.DocumentID)
			res.Outcomes = append(res.Outcomes, NoteOutcome{n.DocumentID, Sent, ms})
			done[n.OperationID] = true
			continue
		}
		n.LastError = errSummary(err)
		if !refusesDocument(err) {
			n.Attempts++
		}
		if refusesDocument(err) || n.Attempts >= MaxAttempts {
			if qerr := appendJSON(o.QuarantinePath, n); qerr == nil {
				res.Quarantined++
				res.QuarantinedDocs = append(res.QuarantinedDocs, n.DocumentID)
				res.Outcomes = append(res.Outcomes, NoteOutcome{n.DocumentID, Quarantined, ms})
				done[n.OperationID] = true
				continue
			}
		}
		// Unreachable, misconfigured or busy: stop, keep this note and the rest.
		res.LastError = n.LastError
		res.Outcomes = append(res.Outcomes, NoteOutcome{n.DocumentID, Retry, ms})
		failed[n.OperationID] = n
		break
	}
	// What was sent is recorded whatever is left of ctx: a note left in
	// the file would go out again.
	unlock, err = o.lock(context.Background(), "notes flush")
	if err != nil {
		return res, err
	}
	defer unlock()
	current, err := o.read() // includes notes appended during the sends
	if err != nil {
		return res, err
	}
	var keep []Note
	for _, n := range current {
		if done[n.OperationID] {
			continue
		}
		if f, ok := failed[n.OperationID]; ok {
			n = f
		}
		keep = append(keep, n)
	}
	res.Kept = len(keep)
	if err := o.rewrite(keep); err != nil {
		return res, err
	}
	return res, gateErr
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

// Requeue moves quarantined notes back into the outbox.
func (o Outbox) Requeue() (int, error) {
	unlock, err := o.lock(context.Background(), "retry-quarantine")
	if err != nil {
		return 0, err
	}
	defer unlock()
	q := Outbox{Path: o.QuarantinePath}
	held, err := q.read()
	if err != nil || len(held) == 0 {
		return 0, err
	}
	notes, err := o.read()
	if err != nil {
		return 0, err
	}
	for i := range held {
		held[i].Attempts, held[i].LastError = 0, ""
	}
	if err := o.rewrite(append(notes, held...)); err != nil {
		return 0, err
	}
	return len(held), os.Remove(o.QuarantinePath)
}

// Quarantined counts quarantined notes.
func (o Outbox) Quarantined() (int, error) {
	notes, err := Outbox{Path: o.QuarantinePath}.read()
	return len(notes), err
}

// Pending counts queued notes.
func (o Outbox) Pending() (int, error) {
	notes, err := o.read()
	return len(notes), err
}

// Oldest is the earliest created_at of the queued notes; zero when there
// are none.
func (o Outbox) Oldest() (time.Time, error) {
	notes, err := o.read()
	var oldest time.Time
	for _, n := range notes {
		if oldest.IsZero() || n.CreatedAt.Before(oldest) {
			oldest = n.CreatedAt
		}
	}
	return oldest, err
}

func appendJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := fsutil.MkdirPrivate(filepath.Dir(path)); err != nil {
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
