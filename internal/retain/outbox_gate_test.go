package retain

import (
	"context"
	"strings"
	"testing"
)

// A subagent report queued before the gate (or given another try from the
// quarantine) that fails it is dropped at flush, counted, never sent; a
// long report and a short note go as before.
func TestOutboxFlushDropsShortSubagentReports(t *testing.T) {
	e := newEnv(t)
	o := outbox(t)
	o.ReportMinChars = 400
	short := NewSubagentNote("claude-memory", "s1", "a1", "general-purpose", "billing", "Reading x.go", at)
	long := NewSubagentNote("claude-memory", "s1", "a2", "general-purpose", "billing", strings.Repeat("Found the leak in pool.go. ", 20), at)
	note := NewNote("claude-memory", "lesson", "billing", "ok", at) // a stored note is never gated
	for _, n := range []Note{short, long, note} {
		if _, err := o.Append(n); err != nil {
			t.Fatal(err)
		}
	}
	res, err := o.Flush(context.Background(), e.f.Client, "claude-memory", "m1")
	if err != nil || res.Sent != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	var sent []string
	for _, it := range e.hs.retains {
		sent = append(sent, it.DocumentID)
	}
	if strings.Join(sent, ",") != long.DocumentID+","+note.DocumentID {
		t.Errorf("sent %v", sent)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != (SkippedReport{Document: short.DocumentID, Length: "<50", AgentType: "general-purpose"}) {
		t.Errorf("skipped %+v", res.Skipped)
	}
	if n, _ := o.Pending(); n != 0 {
		t.Errorf("%d notes left: a dropped report must leave the queue", n)
	}
}
