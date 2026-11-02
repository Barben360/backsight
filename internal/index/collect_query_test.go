package index

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metricsfile"
)

// TestListedDays: the days of the memory_day view, ascending, each once
// however many lines list it; complete when any day's latest line is.
func TestListedDays(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	days, complete, err := e.ix.ListedDays(ctx)
	if err != nil || len(days) != 0 || complete {
		t.Fatalf("empty record: %v %v %v, want no day, not complete", days, complete, err)
	}
	md := metricsfile.FamilyMemoryDocs
	listed := func(at time.Duration, day string, n int, complete bool) string {
		f := map[string]any{"day": day, "n": n}
		if complete {
			f["complete"] = true
		}
		return mline("listed", tsAt(at), f)
	}
	e.appendM(md, "2026-10", listed(0, "2026-10-27", 4, false)+listed(time.Second, "2026-10-25", 2, false)+
		listed(2*time.Second, "2026-10-27", 5, false)+
		mline("memory", tsAt(3*time.Second), map[string]any{"memory": "m1", "outcome": "found", "doc_kind": "none", "created_day": "2026-10-26", "fact_type": "world"}))
	e.ingest()
	days, complete, err = e.ix.ListedDays(ctx)
	if err != nil || !reflect.DeepEqual(days, []string{"2026-10-25", "2026-10-27"}) || complete {
		t.Errorf("days %v complete %v err %v, want 25 and 27 (a memory line lists no day), not complete", days, complete, err)
	}
	e.appendM(md, "2026-10", listed(4*time.Second, "2026-10-24", 0, true))
	e.ingest()
	days, complete, err = e.ix.ListedDays(ctx)
	if err != nil || !reflect.DeepEqual(days, []string{"2026-10-24", "2026-10-25", "2026-10-27"}) || !complete {
		t.Errorf("days %v complete %v err %v, want 24, 25, 27, complete", days, complete, err)
	}
}

// TestSentDocuments: the session documents this PC sent ok since the
// time asked, once each with the latest send; notes, imports and other
// ids are never returned (R47), nor sends that were not ok.
func TestSentDocuments(t *testing.T) {
	e := newEnv(t)
	at := tsAt
	retain := func(ts, doc, status string) string {
		return tline("retain", ts, map[string]any{"session": "s1", "document": doc, "items": 1, "status": status, "ms": 5})
	}
	since := time.UnixMilli(ms0).UTC().Add(-7 * 24 * time.Hour)
	e.append("2026-10", retain(at(-10*24*time.Hour), "conversation:old", "ok")+
		retain(at(-3*24*time.Hour), "conversation:s1@backsight", "ok")+
		retain(at(-2*24*time.Hour), "conversation:s1@backsight", "ok")+
		retain(at(-24*time.Hour), "subagent:s2:a1", "ok")+
		retain(at(-24*time.Hour), "conversation:s3", "queued")+
		retain(at(-24*time.Hour), "conversation:s4", "failed")+
		retain(at(-time.Hour), "note:abc123", "ok")+
		retain(at(-time.Hour), "import:docs/x.md", "ok")+
		retain(at(-time.Hour), "backsight:ledger", "ok"))
	e.ingest()
	got, err := e.ix.SentDocuments(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	want := []SentDocument{
		{Document: "conversation:s1@backsight", Session: "s1", SentAt: time.UnixMilli(ms0).UTC().Add(-2 * 24 * time.Hour)},
		{Document: "subagent:s2:a1", Session: "s2", SentAt: time.UnixMilli(ms0).UTC().Add(-24 * time.Hour)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sent documents\n got %v\nwant %v", got, want)
	}
	for _, s := range got {
		if strings.HasPrefix(s.Document, "import:") || strings.HasPrefix(s.Document, "note:") {
			t.Errorf("a non-session document was returned: %v", s)
		}
	}
}

// TestRecordedOperations: per operation its latest status, whether any
// of its lines holds the document's unit count, and whether one says the
// server gave none.
func TestRecordedOperations(t *testing.T) {
	e := newEnv(t)
	md := metricsfile.FamilyMemoryDocs
	op := func(at time.Duration, id, status string, units any) string {
		f := map[string]any{"operation": id, "document": "conversation:s1", "session": "s1", "status": status,
			"created_at": "2026-10-29T10:00:00Z", "updated_at": "2026-10-29T10:01:00Z", "retries": 0}
		if units != nil {
			f["units"] = units
		}
		return mline("op", tsAt(at), f)
	}
	e.appendM(md, "2026-10", op(0, "o1", "pending", nil)+op(time.Second, "o1", "completed", nil)+
		op(0, "o2", "completed", nil)+op(time.Second, "o2", "completed", 4)+
		op(0, "o3", "failed", nil)+op(0, "o4", "completed", 0)+
		op(0, "o5", "completed", nil)+mline("op", tsAt(time.Second), map[string]any{"operation": "o5", "document": "conversation:s1", "session": "s1",
		"status": "completed", "created_at": "2026-10-29T10:00:00Z", "updated_at": "2026-10-29T10:01:00Z", "retries": 0, "no_units": true}))
	e.ingest()
	got, err := e.ix.RecordedOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]RecordedOp{
		"o1": {Status: "completed"},
		"o2": {Status: "completed", HasUnits: true},
		"o3": {Status: "failed"},
		"o4": {Status: "completed", HasUnits: true}, // 0 units is a count read
		"o5": {Status: "completed", NoUnits: true},  // read, the server gave no count
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recorded\n got %v\nwant %v", got, want)
	}
}
