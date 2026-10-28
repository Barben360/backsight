package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
)

func TestStoreSendsAtOnce(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	before := retained.Load()
	code, out, _ := run(t, "store", "--kind", "decision", "--project", "billing", "Billing", "keeps", "PostgreSQL", "16.", "--json")
	var env struct {
		Data storeResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || env.Data.Status != "stored" || env.Data.Project != "billing" || !strings.HasPrefix(env.Data.Document, "note:") || retained.Load() != before+1 {
		t.Errorf("exit %d\n%s", code, out)
	}
	if n, _ := notesOutbox(pathsOf(home)).Pending(); n != 0 {
		t.Errorf("%d notes left in the outbox", n)
	}
}

func TestStoreQueuesWhenServerDown(t *testing.T) {
	home := devInstall(t, true) // closed port
	code, out, _ := run(t, "store", "--kind", "lesson", "--project", "p", "Retry with backoff.")
	if code != output.OK || !strings.Contains(out, "queued") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if n, _ := notesOutbox(pathsOf(home)).Pending(); n != 1 {
		t.Errorf("want 1 queued note, got %d", n)
	}
}

func TestStoreRefusals(t *testing.T) {
	devInstall(t, true)
	secret := "sk-" + "ant-" + strings.Repeat("Ab3dE5gH9k", 3)
	cases := map[string][]string{
		"bad kind":   {"store", "--kind", "idea", "text"},
		"no text":    {"store", "--kind", "lesson"},
		"blank text": {"store", "--kind", "lesson", "  "},
		"secret":     {"store", "--kind", "lesson", "use", secret},
	}
	for name, args := range cases {
		code, out, errOut := run(t, args...)
		if code != output.Usage || strings.Contains(out+errOut, secret) {
			t.Errorf("%s: exit %d %q %q", name, code, out, errOut)
		}
	}
}

func newTestNote() retain.Note {
	return retain.NewNote("claude-memory", "lesson", "p", "Retry with backoff.", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
}

func TestStoreQueuesDuringMaintenance(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	_ = os.MkdirAll(filepath.Dir(d.MaintenanceLock()), 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":1}`), 0o600)
	before := retained.Load()
	_, out, _ := run(t, "store", "--kind", "lesson", "--project", "p", "Queued during maintenance.")
	if !strings.Contains(out, "queued") || retained.Load() != before {
		t.Errorf("%s", out)
	}
}

// A note the server refuses goes to quarantine: the command says so and
// exits 20, never "stored" (M1 review).
func TestStoreReportsARejectedNote(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	retainStatus.Store(400)
	code, out, _ := run(t, "store", "--kind", "decision", "--json", "We keep PostgreSQL 16.")
	if code != output.Blocked || strings.Contains(out, `"status": "stored"`) || !strings.Contains(out, "quarantine") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// hooks.outbox_max_mb is read by the command: a queue past it refuses the note.
func TestStoreHonoursTheConfiguredOutboxMax(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	appendConfig(t, home, "[hooks]\noutbox_warn_mb = 1\noutbox_max_mb = 1\n")
	d := pathsOf(home)
	_ = os.MkdirAll(d.OutboxDir(), 0o700)
	path := filepath.Join(d.OutboxDir(), "notes.jsonl")
	_ = os.WriteFile(path, []byte(strings.Repeat("x", 1<<20)), 0o600)
	before := retained.Load()
	code, out, errOut := run(t, "store", "--kind", "lesson", "--project", "p", "Refused: the queue is full.")
	if code == output.OK || retained.Load() != before || !strings.Contains(out+errOut, "outbox is full") {
		t.Errorf("exit %d, %q %q", code, out, errOut)
	}
	if raw, _ := os.ReadFile(path); len(raw) != 1<<20 {
		t.Errorf("the refused note was written: %d bytes", len(raw))
	}
}

// While a different server answers at the joined address (a jobs tick
// recorded it), a note stays queued, sent to no server: no extra request
// on the hot path, the warning names doctor.
func TestStoreKeepsTheNoteWhileAnotherServerAnswers(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recordMismatch(t, home)
	before := retained.Load()
	code, out, _ := run(t, "store", "--kind", "lesson", "--project", "p", "Kept while another server answers.", "--json")
	var env struct {
		Data storeResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || env.Data.Status != "queued" || !strings.Contains(env.Data.Warning, "backsight doctor") || retained.Load() != before {
		t.Errorf("exit %d, %d retained\n%s", code, retained.Load()-before, out)
	}
	if n, _ := notesOutbox(pathsOf(home)).Pending(); n != 1 {
		t.Errorf("want the note queued, got %d", n)
	}
}

// A mismatch record left after a switch back to local mode holds nothing:
// only a client checks the server it joined.
func TestStoreSendsInLocalModeWhateverTheRecord(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	setMismatch(t, home, "b")
	before := retained.Load()
	if code, out, _ := run(t, "store", "--kind", "lesson", "--project", "p", "Sent from a local install.", "--json"); code != output.OK || !strings.Contains(out, `"status": "stored"`) || retained.Load() != before+1 {
		t.Errorf("exit %d, %d retained\n%s", code, retained.Load()-before, out)
	}
}
