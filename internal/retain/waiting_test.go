package retain

import (
	"path/filepath"
	"testing"
	"time"
)

// Waiting names each session that has something left to send, with why it
// is not gone yet: still being written (it settles 30 minutes after its last
// turn), settled and due at the next tick, or set aside by the server.
func TestWaitingNamesEachSessionAndItsReason(t *testing.T) {
	e := newEnv(t)
	e.record("live", "u1", "user", 170, "still talking") // 11:50, 10 min before the clock
	e.record("due", "u2", "user", 0, "asked at nine")    // 3 h before the clock
	e.record("held", "u3", "user", 0, "refused once")
	e.record("done", "u4", "user", 0, "already sent")
	expect(t, e.flush("done", Options{}), Sent, 1, 1)
	cur := Cursor{Transcript: filepath.Join(e.dir, "held.jsonl"), Quarantined: true}
	if err := SaveCursor(e.f.Dirs.CursorsDir(), "held", cur); err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, s := range []string{"live", "due", "held", "done"} {
		files = append(files, filepath.Join(e.dir, s+".jsonl"))
	}

	got := map[string]Waiter{}
	for _, w := range e.f.Waiting(files) {
		got[w.Session] = w
	}
	if len(got) != 3 {
		t.Fatalf("waiting: %+v", got)
	}
	if w := got["live"]; w.Reason != WaitNotSettled || w.Idle != 10*time.Minute {
		t.Errorf("live: %+v", w)
	}
	if w := got["due"]; w.Reason != WaitDue || w.Idle != 3*time.Hour {
		t.Errorf("due: %+v", w)
	}
	if w := got["held"]; w.Reason != WaitQuarantined {
		t.Errorf("held: %+v", w)
	}
	// Unsent keeps counting what a tick can still send: the quarantined
	// session is counted apart.
	if n := e.f.Unsent(files); n != 2 {
		t.Errorf("unsent %d, want 2", n)
	}
}
