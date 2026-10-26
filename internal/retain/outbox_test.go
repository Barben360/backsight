package retain

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
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
	if _, ok := it.Metadata["session"]; ok {
		t.Errorf("a note without a session names one: %v", it.Metadata)
	}
	// the session it was stored from goes with it, for review (memory notes)
	n.Session = "s-42"
	if it := n.Item("m1"); it.Metadata["session"] != "s-42" {
		t.Errorf("session metadata %v", it.Metadata)
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
	o.WarnBytes, o.MaxBytes = 300, 600
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

func TestOutboxErrorClasses(t *testing.T) {
	for status, quarantined := range map[int]bool{400: true, 422: true, 401: false, 404: false, 302: false, 500: false} {
		e := newEnv(t)
		o := outbox(t)
		_, _ = o.Append(NewNote("b", "lesson", "p", "one", at))
		e.hs.status = status
		res, _ := o.Flush(context.Background(), e.f.Client, "b", "m1")
		if (res.Quarantined == 1) != quarantined || (res.Kept == 1) == quarantined {
			t.Errorf("HTTP %d: %+v", status, res)
		}
	}
}

// A note is never dropped because its quarantine record cannot be written.
func TestOutboxKeepsNoteWhenQuarantineFails(t *testing.T) {
	e := newEnv(t)
	o := outbox(t)
	_ = os.MkdirAll(o.QuarantinePath, 0o700) // a directory: appends fail
	_, _ = o.Append(NewNote("b", "lesson", "p", "one", at))
	e.hs.status = 422
	res, _ := o.Flush(context.Background(), e.f.Client, "b", "m1")
	if n, _ := o.Pending(); n != 1 || res.Quarantined != 0 {
		t.Errorf("note lost: pending %d, %+v", n, res)
	}
}

func TestRequeue(t *testing.T) {
	e := newEnv(t)
	o := outbox(t)
	_, _ = o.Append(NewNote("b", "lesson", "p", "one", at))
	e.hs.status = 422
	_, _ = o.Flush(context.Background(), e.f.Client, "b", "m1")
	if n, _ := o.Quarantined(); n != 1 {
		t.Fatalf("quarantined %d", n)
	}
	if n, err := o.Requeue(); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ := o.Pending(); n != 1 {
		t.Errorf("pending %d", n)
	}
	if n, _ := o.Quarantined(); n != 0 {
		t.Errorf("still quarantined %d", n)
	}
}

// After a torn line, the next note starts on a new line and both survive.
func TestAppendAfterTornLine(t *testing.T) {
	o := outbox(t)
	_, _ = o.Append(NewNote("b", "lesson", "p", "before", at))
	f, _ := os.OpenFile(o.Path, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString(`{"operation_id":"torn","conte`)
	_ = f.Close()
	if _, err := o.Append(NewNote("b", "lesson", "p", "after", at)); err != nil {
		t.Fatal(err)
	}
	notes, _ := o.read()
	if len(notes) != 2 || notes[0].Content != "before" || notes[1].Content != "after" {
		t.Errorf("%+v", notes)
	}
}

// store can append while a flush waits on a slow server; the new note is
// kept after the flush.
func TestAppendDuringSlowFlush(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := hindsight.New(srv.URL, "k", 5*time.Second)
	o := outbox(t)
	_, _ = o.Append(NewNote("b", "lesson", "p", "first", at))
	done := make(chan FlushResult)
	go func() { r, _ := o.Flush(context.Background(), c, "b", "m"); done <- r }()
	time.Sleep(50 * time.Millisecond) // the flush is now waiting on the server
	if _, err := o.Append(NewNote("b", "lesson", "p", "during", at)); err != nil {
		t.Fatalf("append blocked by a flush in flight: %v", err)
	}
	close(release)
	r := <-done
	notes, _ := o.read()
	if r.Sent != 1 || len(notes) != 1 || notes[0].Content != "during" {
		t.Errorf("%+v %+v", r, notes)
	}
}

// A note failing MaxAttempts times goes to quarantine instead of blocking
// every note queued after it (M2 block 1 review).
func TestOutboxQuarantinesANoteThatKeepsFailing(t *testing.T) {
	e := newEnv(t)
	o := outbox(t)
	_, _ = o.Append(NewNote("b", "lesson", "p", "stuck", at))
	_, _ = o.Append(NewNote("b", "lesson", "p", "next", at))
	notes, _ := o.read()
	notes[0].Attempts = MaxAttempts - 1
	_ = o.rewrite(notes)
	e.hs.status = http.StatusInternalServerError
	res, _ := o.Flush(context.Background(), e.f.Client, "b", "m1")
	left, _ := o.read()
	if res.Quarantined != 1 || len(res.QuarantinedDocs) != 1 || len(left) != 1 || left[0].Content != "next" {
		t.Errorf("%+v %+v", res, left)
	}
	e.hs.status = 0
	if res, _ := o.Flush(context.Background(), e.f.Client, "b", "m1"); res.Sent != 1 || res.SentDocs[0] != left[0].DocumentID {
		t.Errorf("%+v", res)
	}
}

func TestSubagentNoteWithoutAgentTypeHasNoAgentTag(t *testing.T) {
	it := NewSubagentNote("b", "s1", "agent-7", "", "billing", "Found the leak.", at).Item("m1")
	if strings.Contains(strings.Join(it.Tags, ","), "agent:") || it.Metadata["agent_type"] != "" {
		t.Errorf("%+v", it)
	}
}

func TestSubagentNoteItem(t *testing.T) {
	n := NewSubagentNote("b", "s1", "agent-7", "Explore", "billing", "Found the leak in pool.go.", at)
	it := n.Item("m1")
	if n.DocumentID != "subagent:s1:agent-7" || it.Strategy != "subagent" || strings.Join(it.Tags, ",") != "project:billing,source:subagent,harness:claude-code,agent:Explore" ||
		len(it.ObservationScopes) != 1 || len(it.ObservationScopes[0]) != 1 || it.ObservationScopes[0][0] != "project:billing" ||
		it.Metadata["source"] != "subagent" || it.Metadata["agent_type"] != "Explore" {
		t.Errorf("%+v %+v", n, it)
	}
}

func TestOutboxLimitsDefaultsAndMB(t *testing.T) {
	if w, m := (Outbox{}).Limits(); w != 50<<20 || m != 500<<20 {
		t.Errorf("defaults: %d %d", w, m)
	}
	if w, m := (Outbox{}).WithLimitsMB(3, 7).Limits(); w != 3<<20 || m != 7<<20 {
		t.Errorf("configured: %d %d", w, m)
	}
	if w, m := (Outbox{}).WithLimitsMB(0, 0).Limits(); w != 50<<20 || m != 500<<20 {
		t.Errorf("zero keeps the defaults: %d %d", w, m)
	}
}

// A max of 1 MB refuses at 1 MB, where the default would take the note;
// a warn of 1 MB warns where the default would not.
func TestOutboxConfiguredLimitsAreHonoured(t *testing.T) {
	o := outbox(t).WithLimitsMB(1, 2)
	_ = os.MkdirAll(filepath.Dir(o.Path), 0o700)
	big := strings.Repeat("x", 1<<20+10)
	if err := os.WriteFile(o.Path, []byte(big+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	warn, err := o.Append(NewNote("b", "lesson", "p", "past warn", at))
	if err != nil || !warn {
		t.Fatalf("1 MB queued, warn 1 MB: want a warning and the note kept, got %v %v", warn, err)
	}
	if err := os.WriteFile(o.Path, []byte(strings.Repeat("x", 2<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Append(NewNote("b", "lesson", "p", "refused", at)); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("2 MB queued, max 2 MB: want ErrOutboxFull, got %v", err)
	}
	if _, err := outbox(t).Append(NewNote("b", "lesson", "p", "default", at)); err != nil {
		t.Errorf("the default still takes notes: %v", err)
	}
}
