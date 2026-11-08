package retain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gateEnv is a session sent once at 10:00 (its first segment, one turn
// of 09:00), with the append gate on at its defaults (decision 0083 C1).
func gateEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.f.Append = AppendGate{MinChars: 20_000, MaxWait: 6 * time.Hour}
	e.clock = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	e.record("s1", "u1", "user", 0, "first question") // 09:00
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	return e
}

// longOpen adds chars characters of turns at 10:10 and a fresh turn at
// fresh minutes after 09:00: the session is still open, its 10:10 turns
// long-open once 2 h old.
func longOpen(e *env, chars, fresh int) {
	e.t.Helper()
	e.record("s1", "a1", "assistant", 70, strings.Repeat("x", chars)) // 10:10
	e.record("s1", "u2", "user", fresh, "still talking")
}

func TestAppendOfLittleTextWaitsForTheGate(t *testing.T) {
	e := gateEnv(t)
	longOpen(e, 5_000, 230)                                  // fresh turn 12:50
	e.clock = time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC) // 10:10 is 2 h 50 old, last send 3 h ago
	r := e.flush("s1", Options{})
	expect(t, r, Waiting, 0, 0)
	if !strings.Contains(r.Detail, "waiting for 20,000 characters or 6 h") {
		t.Errorf("detail %q does not name the rule", r.Detail)
	}
	if len(e.hs.retains) != 1 {
		t.Errorf("the append went out: %d retains", len(e.hs.retains))
	}
}

// A last send 1 h ago, the issue's own case.
func TestAppendWaitsAnHourAfterTheLastSend(t *testing.T) {
	e := newEnv(t)
	e.f.Append = AppendGate{MinChars: 20_000, MaxWait: 6 * time.Hour}
	e.clock = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	e.record("s1", "u1", "user", 0, "first question")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)                  // 12:00
	e.record("s1", "a1", "assistant", 5, strings.Repeat("x", 5_000)) // 09:05, 3 h 55 old at 13:00
	e.record("s1", "u2", "user", 235, "still talking")               // 12:55
	e.clock = e.clock.Add(time.Hour)
	expect(t, e.flush("s1", Options{}), Waiting, 0, 0)
	if len(e.hs.retains) != 1 {
		t.Errorf("the append went out: %d retains", len(e.hs.retains))
	}
}

func TestAppendOfEnoughTextGoesAtOnce(t *testing.T) {
	e := gateEnv(t)
	longOpen(e, 21_000, 230)
	e.clock = time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	if got := e.hs.retains[1]; got.UpdateMode != "append" || strings.Contains(got.Content, "still talking") {
		t.Errorf("not the long-open part as an append: %+v", got)
	}
}

func TestAppendGoesOnceTheLastSendIsOldEnough(t *testing.T) {
	e := gateEnv(t)
	longOpen(e, 5_000, 410)                                  // fresh turn 15:50
	e.clock = time.Date(2026, 10, 24, 16, 1, 0, 0, time.UTC) // last send 6 h 1 ago
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
}

// A settled session waits too: the lull alone no longer sends an append.
func TestSettledAppendWaitsForTheGate(t *testing.T) {
	e := gateEnv(t)
	e.record("s1", "a1", "assistant", 70, strings.Repeat("x", 5_000)) // 10:10
	e.clock = time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)          // settled 50 min, last send 1 h ago
	expect(t, e.flush("s1", Options{}), Waiting, 0, 0)
	e.clock = time.Date(2026, 10, 24, 16, 0, 0, 0, time.UTC) // last send exactly 6 h ago
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
}

// SessionEnd and retain flush send with Force: the gate never holds them.
func TestForcedAppendIgnoresTheGate(t *testing.T) {
	e := gateEnv(t)
	e.record("s1", "a1", "assistant", 70, strings.Repeat("x", 1_000))
	e.clock = time.Date(2026, 10, 24, 10, 15, 0, 0, time.UTC)
	expect(t, e.flush("s1", Options{Force: true}), Sent, 1, 2)
}

// The first send of a session is never held, however little it holds.
func TestFirstSendIgnoresTheGate(t *testing.T) {
	e := newEnv(t)
	e.f.Append = AppendGate{MinChars: 20_000, MaxWait: 6 * time.Hour}
	e.record("s1", "u1", "user", 0, strings.Repeat("x", 1_000))
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
}

// The first send to another project's document is not an append either:
// the whole pending part goes out, the append with it.
func TestFirstSendToAnotherDocumentIgnoresTheGate(t *testing.T) {
	e := gateEnv(t)
	other := e.repo("ledger")
	e.record("s1", "a1", "assistant", 70, "a little more here")
	e.recordIn(other, "s1", "u2", "user", 75, "and a word about the ledger")
	e.clock = time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	r := e.flush("s1", Options{})
	if r.Status != Sent || len(e.hs.retains) != 3 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
}

// A resync rebuilds the cursor from the server (here: the cursor is lost),
// and with it forgets the last sends: what it finds to send goes out, as
// before the gate, never held on a time it cannot know.
func TestResyncIgnoresTheGate(t *testing.T) {
	e := gateEnv(t)
	if err := os.RemoveAll(e.f.Dirs.CursorsDir()); err != nil {
		t.Fatal(err)
	}
	e.record("s1", "a1", "assistant", 70, "a little more")
	e.clock = time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
}

// The last send survives a restart: a new process reads it from the
// cursor and still waits.
func TestLastSendSurvivesARestart(t *testing.T) {
	e := gateEnv(t)
	cur, ok, err := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if want := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC); !cur.SentAts["billing"].Equal(want) {
		t.Fatalf("sent_ats %v, want billing at %v", cur.SentAts, want)
	}
	idx, _ := LoadSentIndex(e.f.Dirs.SentIndexDir())
	f := *e.f
	f.Index = idx
	e.f = &f
	e.record("s1", "a1", "assistant", 70, strings.Repeat("x", 5_000))
	e.clock = time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	expect(t, e.flush("s1", Options{}), Waiting, 0, 0)
}

// A cursor of an older build has no sent_ats: its sent_at, the session's
// last send, stands in.
func TestOlderCursorFallsBackToItsLastSend(t *testing.T) {
	e := gateEnv(t)
	cur, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	cur.SentAts = nil
	if err := SaveCursor(e.f.Dirs.CursorsDir(), "s1", cur); err != nil {
		t.Fatal(err)
	}
	e.record("s1", "a1", "assistant", 70, strings.Repeat("x", 5_000))
	e.clock = time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	expect(t, e.flush("s1", Options{}), Waiting, 0, 0)
	e.clock = time.Date(2026, 10, 24, 16, 0, 0, 0, time.UTC)
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
}

// retain waiting names the gate as the reason, not the settle rule.
func TestWaitingNamesTheAppendGate(t *testing.T) {
	e := gateEnv(t)
	e.record("s1", "a1", "assistant", 70, strings.Repeat("x", 5_000))
	e.clock = time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	ws := e.f.Waiting([]string{filepath.Join(e.dir, "s1.jsonl")})
	if len(ws) != 1 || ws[0].Reason != WaitAppend || !strings.Contains(ws[0].Detail, "waiting for 20,000 characters or 6 h") {
		t.Fatalf("%+v", ws)
	}
	// Enough text: due at the next tick, as before.
	e.record("s1", "a2", "assistant", 80, strings.Repeat("y", 20_000))
	e.clock = time.Date(2026, 10, 24, 11, 30, 0, 0, time.UTC)
	if ws := e.f.Waiting([]string{filepath.Join(e.dir, "s1.jsonl")}); len(ws) != 1 || ws[0].Reason != WaitDue {
		t.Fatalf("%+v", ws)
	}
}

func TestAppendGateHolds(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	g := AppendGate{MinChars: 20_000, MaxWait: 6 * time.Hour}
	for _, c := range []struct {
		name  string
		g     AppendGate
		chars int
		last  time.Time
		want  bool
	}{
		{"little text, recent send", g, 19_999, now.Add(-time.Hour), true},
		{"enough text", g, 20_000, now.Add(-time.Hour), false},
		{"last send old enough", g, 10, now.Add(-6 * time.Hour), false},
		{"no last send known", g, 10, time.Time{}, false},
		{"min chars 0: off", AppendGate{MaxWait: 6 * time.Hour}, 10, now.Add(-time.Minute), false},
		{"max wait 0: off", AppendGate{MinChars: 20_000}, 10, now.Add(-time.Minute), false},
	} {
		if got := c.g.Holds(c.chars, c.last, now); got != c.want {
			t.Errorf("%s: holds %v, want %v", c.name, got, c.want)
		}
	}
	if got := (AppendGate{MinChars: 1_500, MaxWait: 90 * time.Minute}).Rule(); got != "waiting for 1,500 characters or 1h30m" {
		t.Errorf("rule %q", got)
	}
}
