package metrics

import (
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/telemetry"
)

// TestBacklogBoundByWhatWaitsNow: a flush that failed before sending logged
// a retry on conversation:<sid> while the session had nothing new, and no
// later line settles it. The local status is the truth of what waits: once
// a jobs tick that counted the waiting sessions ended after the retry and
// counted none, the document is settled: sent, no backlog, nothing bad.
// A real backlog after it keeps it waiting (never hidden); a retry after the
// last tick, or a tick that did not count (skipped), settles nothing.
func TestBacklogBoundByWhatWaitsNow(t *testing.T) {
	tick := func(x *fx, at time.Time, status string) {
		x.tel(at, telemetry.Job{Name: "tick", Status: status, MS: 40}, "", "")
	}
	stale := func(x *fx) {
		x.retained(ago(6*day+20*time.Hour), "conversation:s1", "ok", 100)
		x.retained(ago(6*day+19*time.Hour), "conversation:s1", "retry", 0)
		x.retained(ago(3*time.Hour), "note:n1", "ok", 100)
	}
	t.Run("a stale retry followed by nothing", func(t *testing.T) {
		x := pipeFx(t)
		x.local(index.LocalStatus{})
		stale(x)
		tick(x, ago(10*time.Minute), "done")
		ms := pipeOf(x, x.query(7))
		flow := ms["pipeline.flow"]
		if flow.Num == nil || *flow.Num != 2 || *flow.Den != 2 || extraInt(t, flow, "waiting") != 0 || extraInt(t, flow, "backlog_oldest_ms") != 0 {
			t.Errorf("flow %v/%v extra %v, want 2/2, nothing waiting, no backlog", flow.Num, flow.Den, flow.Extra)
		}
		wantStatus(t, "pipeline.flow", flow, StatusOK)
		bl := ms["local.backlog"]
		wantStatus(t, "local.backlog", bl, StatusOK)
		if got := extraInt(t, bl, "oldest_ms"); got != 0 {
			t.Errorf("local.backlog oldest_ms = %d, want 0", got)
		}
	})
	t.Run("a stale retry beside a session still being written", func(t *testing.T) {
		// The common case: someone works in Claude Code, so the tick counts
		// a session that waits (s3, not settled yet); the stale s1 is not
		// among the sessions it named: sent, nothing bad.
		x := pipeFx(t)
		x.local(index.LocalStatus{SessionsWaiting: 1, WaitingSessions: []string{"s3"}})
		stale(x)
		tick(x, ago(10*time.Minute), "partial")
		ms := pipeOf(x, x.query(7))
		flow := ms["pipeline.flow"]
		if *flow.Num != 2 || *flow.Den != 2 || extraInt(t, flow, "waiting") != 0 || extraInt(t, flow, "backlog_oldest_ms") != 0 {
			t.Errorf("flow %v/%v extra %v, want 2/2, nothing waiting, no backlog", *flow.Num, *flow.Den, flow.Extra)
		}
		wantStatus(t, "pipeline.flow", flow, StatusOK)
		wantStatus(t, "local.backlog", ms["local.backlog"], StatusOK)
	})
	t.Run("a stale retry beside a real backlog", func(t *testing.T) {
		// s2 failed 2 days ago and the tick still names it: it waits, and
		// its age (not the stale s1's) is the backlog's: still bad.
		x := pipeFx(t)
		x.local(index.LocalStatus{SessionsWaiting: 2, WaitingSessions: []string{"s2", "s3"}})
		stale(x)
		x.retained(ago(2*day), "conversation:s2@beta", "retry", 0)
		tick(x, ago(10*time.Minute), "done")
		ms := pipeOf(x, x.query(7))
		flow := ms["pipeline.flow"]
		if *flow.Num != 2 || *flow.Den != 3 || extraInt(t, flow, "waiting") != 1 || extraInt(t, flow, "backlog_oldest_ms") != (2*day).Milliseconds() {
			t.Errorf("flow %v/%v extra %v, want 2/3, s2 waiting for 2 days", *flow.Num, *flow.Den, flow.Extra)
		}
		wantStatus(t, "pipeline.flow", flow, StatusBad)
		wantStatus(t, "local.backlog", ms["local.backlog"], StatusBad)
	})
	t.Run("a retry during the last tick still waits", func(t *testing.T) {
		// The tick counted its sessions before s2's retry (logged during the
		// run, after the tick began): the count says nothing of it.
		x := pipeFx(t)
		x.local(index.LocalStatus{WaitingSessions: []string{}})
		x.retained(ago(5*time.Minute), "conversation:s2", "retry", 0)
		x.tel(ago(time.Minute), telemetry.Job{Name: "tick", Status: "done", MS: int((10 * time.Minute).Milliseconds())}, "", "")
		flow := pipeOf(x, x.query(7))["pipeline.flow"]
		if extraInt(t, flow, "waiting") != 1 || extraInt(t, flow, "backlog_oldest_ms") != (5*time.Minute).Milliseconds() {
			t.Errorf("flow extra %v, want s2 waiting for 5 min", flow.Extra)
		}
	})
	t.Run("without the list: a session waits, so nothing settles", func(t *testing.T) {
		// A reach.json from an earlier version names no session: the count
		// alone decides, and a session waits.
		x := pipeFx(t)
		x.local(index.LocalStatus{SessionsWaiting: 1})
		stale(x)
		tick(x, ago(10*time.Minute), "partial")
		ms := pipeOf(x, x.query(7))
		flow := ms["pipeline.flow"]
		if extraInt(t, flow, "waiting") != 1 || extraInt(t, flow, "backlog_oldest_ms") != (6*day+19*time.Hour).Milliseconds() {
			t.Errorf("flow extra %v, want 1 waiting since the retry", flow.Extra)
		}
		wantStatus(t, "pipeline.flow", flow, StatusBad)
		wantStatus(t, "local.backlog", ms["local.backlog"], StatusBad)
	})
	t.Run("a retry after the last tick still waits", func(t *testing.T) {
		x := pipeFx(t)
		x.local(index.LocalStatus{})
		tick(x, ago(2*time.Hour), "done")
		x.retained(ago(time.Hour), "conversation:s2", "retry", 0)
		flow := pipeOf(x, x.query(7))["pipeline.flow"]
		if extraInt(t, flow, "waiting") != 1 || extraInt(t, flow, "backlog_oldest_ms") != time.Hour.Milliseconds() {
			t.Errorf("flow extra %v, want 1 waiting for 1 h", flow.Extra)
		}
	})
	t.Run("a skipped tick counts nothing", func(t *testing.T) {
		x := pipeFx(t)
		x.local(index.LocalStatus{})
		stale(x)
		tick(x, ago(10*time.Minute), "skipped")
		if got := extraInt(t, pipeOf(x, x.query(7))["pipeline.flow"], "waiting"); got != 1 {
			t.Errorf("waiting = %d, want 1", got)
		}
	})
	t.Run("queued notes do not keep a settled session waiting", func(t *testing.T) {
		x := pipeFx(t)
		x.local(index.LocalStatus{NotesQueued: 1, NotesOldestAt: ago(2 * time.Hour)})
		stale(x)
		x.retained(ago(2*time.Hour), "note:n2", "queued", 0)
		tick(x, ago(10*time.Minute), "done")
		flow := pipeOf(x, x.query(7))["pipeline.flow"]
		if extraInt(t, flow, "waiting") != 1 || extraInt(t, flow, "backlog_oldest_ms") != (2*time.Hour).Milliseconds() {
			t.Errorf("flow extra %v, want the queued note alone, 2 h", flow.Extra)
		}
	})
}

// TestBacklogItemNamesTheAge: the attention item of a backlog says how long
// the oldest item has waited, never a bare count.
func TestBacklogItemNamesTheAge(t *testing.T) {
	d, _ := DefOf("local.backlog")
	m := Metric{Value: ptr(1.0), Extra: map[string]any{"oldest_ms": (6*day + 20*time.Hour).Milliseconds()}}
	if got, want := itemText(d, m, 7, 0, false), "The oldest item has waited on this PC for 6 days."; got != want {
		t.Errorf("local.backlog item %q, want %q", got, want)
	}
	f, _ := DefOf("pipeline.flow")
	fm := Metric{Value: ptr(0.99), Extra: map[string]any{"failed": int64(0), "set_aside": int64(0), "backlog_oldest_ms": (30 * time.Hour).Milliseconds()}}
	if got, want := itemText(f, fm, 7, 0, false), "An item has waited 30 h to be sent."; got != want {
		t.Errorf("pipeline.flow item %q, want %q", got, want)
	}
	fm.Extra["failed"] = int64(3)
	if got, want := itemText(f, fm, 7, 0, false), "3 items could not be sent; 0 set aside after an error."; got != want {
		t.Errorf("pipeline.flow failed item %q, want %q", got, want)
	}
}
