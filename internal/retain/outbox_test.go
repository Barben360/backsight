package retain

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func outbox(t *testing.T) Outbox {
	d := t.TempDir()
	return Outbox{Path: filepath.Join(d, "outbox", "notes.jsonl"), LockPath: filepath.Join(d, "locks", "outbox.lock"),
		QuarantinePath: filepath.Join(d, "quarantine", "notes.jsonl")}
}

var at = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func TestNoteItem(t *testing.T) {
	n := NewNote("b", "decision", "billing", "Billing keeps PostgreSQL 16 for partial indexes.", at)
	it := n.Item("m1")
	if !strings.HasPrefix(n.DocumentID, "note:") || it.Strategy != "document" || it.DocumentID != n.DocumentID ||
		strings.Join(it.Tags, ",") != "project:billing,source:note,kind:decision,harness:claude-code" ||
		it.ObservationScopes[0][0] != "project:billing" || !it.Timestamp.Equal(at) || it.Metadata["kind"] != "decision" {
		t.Errorf("%+v", it)
	}
	if NewNote("b", "decision", "billing", "Billing keeps PostgreSQL 16 for partial indexes.", at).OperationID != n.OperationID {
		t.Error("operation id not deterministic")
	}
}

func TestOutboxFlushSendsAndEmpties(t *testing.T) {
	e := newEnv(t)
	o := outbox(t)
	for _, text := range []string{"one", "two"} {
		if _, err := o.Append(NewNote("claude-memory", "lesson", "p", text, at)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := o.Flush(context.Background(), e.f.Client, "claude-memory", "m1")
	if err != nil || res.Sent != 2 || len(e.hs.retains) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if n, _ := o.Pending(); n != 0 {
		t.Errorf("%d notes left", n)
	}
}

// Server busy: the note and everything after it stay, in order.
func TestOutboxKeepsOnRetryableError(t *testing.T) {
	e := newEnv(t)
	o := outbox(t)
	_, _ = o.Append(NewNote("b", "lesson", "p", "one", at))
	_, _ = o.Append(NewNote("b", "lesson", "p", "two", at))
	e.hs.status = http.StatusServiceUnavailable
	res, _ := o.Flush(context.Background(), e.f.Client, "b", "m1")
	notes, _ := o.read()
	if res.Kept != 2 || len(notes) != 2 || notes[0].Content != "one" || notes[0].Attempts != 1 || notes[0].LastError == "" || notes[1].Attempts != 0 {
		t.Errorf("%+v %+v", res, notes)
	}
	e.hs.status = 0
	if res, _ := o.Flush(context.Background(), e.f.Client, "b", "m1"); res.Sent != 2 {
		t.Errorf("%+v", res)
	}
}

func TestOutboxQuarantinesRefusedNotes(t *testing.T) {
	e := newEnv(t)
	o := outbox(t)
	_, _ = o.Append(NewNote("b", "lesson", "p", "one", at))
	e.hs.status = http.StatusUnprocessableEntity
	res, _ := o.Flush(context.Background(), e.f.Client, "b", "m1")
	if res.Quarantined != 1 {
		t.Errorf("%+v", res)
	}
	if n, _ := o.Pending(); n != 0 {
		t.Error("a refused note stayed queued")
	}
	if raw, _ := os.ReadFile(o.QuarantinePath); !strings.Contains(string(raw), `"content":"one"`) {
		t.Errorf("quarantine: %s", raw)
	}
}

func TestOutboxSizeLimits(t *testing.T) {
	o := outbox(t)
	oldWarn, oldMax := OutboxWarnBytes, OutboxMaxBytes
	OutboxWarnBytes, OutboxMaxBytes = 300, 600
	t.Cleanup(func() { OutboxWarnBytes, OutboxMaxBytes = oldWarn, oldMax })
	warn, err := o.Append(NewNote("b", "lesson", "p", "small", at))
	if warn || err != nil {
		t.Fatalf("%v %v", warn, err)
	}
	for i := 0; i < 5; i++ {
		warn, err = o.Append(NewNote("b", "lesson", "p", strings.Repeat("x", 150), at))
		if err != nil {
			break
		}
	}
	if !errors.Is(err, ErrOutboxFull) || !warn {
		t.Errorf("want a full outbox with a warning, got %v %v", warn, err)
	}
}

func TestOutboxConcurrentAppends(t *testing.T) {
	o := outbox(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = o.Append(NewNote("b", "lesson", "p", strings.Repeat("y", i+1), at))
		}(i)
	}
	wg.Wait()
	if n, _ := o.Pending(); n != 20 {
		t.Errorf("%d of 20 notes", n)
	}
}

// A crash mid-write leaves a torn last line: the other notes survive.
func TestOutboxToleratesATornLine(t *testing.T) {
	e := newEnv(t)
	o := outbox(t)
	_, _ = o.Append(NewNote("b", "lesson", "p", "whole", at))
	f, _ := os.OpenFile(o.Path, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString(`{"operation_id":"x","conte`)
	_ = f.Close()
	if res, err := o.Flush(context.Background(), e.f.Client, "b", "m1"); err != nil || res.Sent != 1 {
		t.Errorf("%+v %v", res, err)
	}
}
