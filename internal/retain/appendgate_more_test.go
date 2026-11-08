package retain

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A last send later than now (the clock was set back since) never holds an
// append: the wait would outlast append_max_wait.
func TestAppendAfterTheClockMovedBackGoes(t *testing.T) {
	e := gateEnv(t)                                                  // sent at 10:00
	e.record("s1", "a1", "assistant", 5, strings.Repeat("x", 5_000)) // 09:05
	e.clock = time.Date(2026, 10, 24, 9, 40, 0, 0, time.UTC)         // set back 20 min before the last send
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
}

// minuteAt is minutes after 09:00 on the test day.
func minuteAt(minute int) time.Time {
	return time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute)
}

// twoProjects sends a session with turns in alpha (its home) and beta at
// 10:00, then 31,000 characters of beta's at 13:00: alpha's document was
// last sent at 10:00, beta's at 13:00.
func twoProjects(t *testing.T) (*env, string, string) {
	t.Helper()
	e := newEnv(t)
	e.f.Append = AppendGate{MinChars: 30_000, MaxWait: 6 * time.Hour}
	alpha, beta := e.repo("alpha"), e.repo("beta")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha question")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta question")
	e.clock = minuteAt(60)
	if r := e.flush("s1", Options{}); r.Status != Sent || len(e.hs.retains) != 2 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
	e.recordIn(beta, "s1", "a2", "assistant", 70, strings.Repeat("b", 31_000))
	e.clock = minuteAt(240)
	if r := e.flush("s1", Options{}); r.Status != Sent || len(e.hs.retains) != 3 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
	return e, alpha, beta
}

// Two documents both appended to: their pending characters add up. Each
// part alone is under the threshold, both together over it.
func TestAppendsToTwoDocumentsSumTheirText(t *testing.T) {
	e, alpha, beta := twoProjects(t)
	e.recordIn(alpha, "s1", "u3", "user", 250, strings.Repeat("a", 16_000))
	e.recordIn(beta, "s1", "u4", "user", 251, strings.Repeat("b", 16_000))
	e.clock = minuteAt(300) // settled; last sends 4 h and 1 h ago
	if r := e.flush("s1", Options{}); r.Status != Sent || len(e.hs.retains) != 5 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
}

// Two documents both appended to: the oldest last send counts, so neither
// waits past append_max_wait for the other's later send.
func TestAppendsToTwoDocumentsWaitForTheOldestLastSend(t *testing.T) {
	e, alpha, beta := twoProjects(t)
	e.recordIn(alpha, "s1", "u3", "user", 250, "a little more for alpha")
	e.recordIn(beta, "s1", "u4", "user", 251, "a little more for beta")
	e.clock = minuteAt(300) // alpha last sent 4 h ago, beta 1 h ago
	expect(t, e.flush("s1", Options{}), Waiting, 0, 0)
	e.clock = minuteAt(420) // alpha 6 h ago, beta 3 h ago
	if r := e.flush("s1", Options{}); r.Status != Sent || len(e.hs.retains) != 5 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
}

// The send at a session's end failed (server down): the tick holds the
// small tail like any append, and sends it once the last send is 6 h old.
func TestFailedSessionEndSendGoesByTheWait(t *testing.T) {
	e := gateEnv(t) // sent at 10:00
	e.record("s1", "a1", "assistant", 70, "the last words")
	e.clock = minuteAt(75)
	e.hs.status = 503
	if r := e.flush("s1", Options{Force: true}); r.Status != Retry {
		t.Fatalf("session end with the server down: %+v", r)
	}
	e.hs.status = 0
	e.clock = minuteAt(120) // settled, last send 1 h ago
	expect(t, e.flush("s1", Options{}), Waiting, 0, 0)
	e.clock = minuteAt(420) // 16:00
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
}

// retain waiting follows the flush: a cursor the flush would rebuild
// (here its fingerprint no longer matches the transcript) is never named
// held by the gate, nor is a turn the settle rule still keeps.
func TestWaitingFollowsTheFlushRules(t *testing.T) {
	e := gateEnv(t)
	e.record("s1", "a1", "assistant", 70, "a little more")
	file := filepath.Join(e.dir, "s1.jsonl")
	e.clock = minuteAt(75) // 5 min old: not settled
	if ws := e.f.Waiting([]string{file}); len(ws) != 1 || ws[0].Reason != WaitNotSettled {
		t.Fatalf("unsettled: %+v", ws)
	}
	e.clock = minuteAt(120)
	if ws := e.f.Waiting([]string{file}); len(ws) != 1 || ws[0].Reason != WaitAppend {
		t.Fatalf("settled: %+v", ws)
	}
	cur, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	cur.Fingerprint = "not this transcript"
	if err := SaveCursor(e.f.Dirs.CursorsDir(), "s1", cur); err != nil {
		t.Fatal(err)
	}
	if ws := e.f.Waiting([]string{file}); len(ws) != 1 || ws[0].Reason != WaitDue {
		t.Fatalf("stale cursor: %+v", ws)
	}
}
