package index

import (
	"testing"

	"github.com/Barben360/backsight/internal/feedback"
)

// A memory or session id never holds whitespace; a project, a document
// or a tool name may.
func TestIDsRejectWhitespaceNamesKeepIt(t *testing.T) {
	e := newEnv(t)
	e.append("2026-10", tline("tool_use", ts0, withCommon(map[string]any{
		"name": "Read tool", "ids": 2, "memory_ids": []string{"a b", "ok"}, "n": 1,
		"session": "s p", "project": "my project"})))
	e.append("2026-10", tline("retain", ts0, withCommon(map[string]any{
		"document": "import:src/some file.md", "status": "ok", "items": 1})))
	e.ingest()

	ids := e.rowsOf(`SELECT memory_id FROM tel_ids ORDER BY pos`)
	if len(ids) != 1 || ids[0]["memory_id"] != "ok" {
		t.Errorf("tel_ids %v, want only ok", ids)
	}
	use := e.rowsOf(`SELECT session, project, name FROM tel_tool_use`)
	if len(use) != 1 || use[0]["session"] != nil || use[0]["project"] != "my project" || use[0]["name"] != "Read tool" {
		t.Errorf("tool_use %v: want session NULL, project and tool name kept", use)
	}
	ret := e.rowsOf(`SELECT document FROM tel_retain`)
	if len(ret) != 1 || ret[0]["document"] != "import:src/some file.md" {
		t.Errorf("retain %v: the document id with a space is kept", ret)
	}
}

func TestJournalIDsRejectWhitespace(t *testing.T) {
	e := newEnv(t)
	e.appendJ(feedback.Event{TS: jT0, Kind: "credit", Session: "s p", Project: "my project", IDs: []string{"a b", "ok"}, Verdict: "used"})
	e.ingest()
	rows := e.rowsOf(`SELECT session, project, memory_id FROM credits ORDER BY pos`)
	if len(rows) != 2 || rows[0]["memory_id"] != nil || rows[1]["memory_id"] != "ok" {
		t.Fatalf("credits %v: want memory_id NULL then ok", rows)
	}
	for _, r := range rows {
		if r["session"] != nil || r["project"] != "my project" {
			t.Errorf("credit %v: want session NULL, project kept", r)
		}
	}
}

func TestLedgerSkipsIDsWithWhitespace(t *testing.T) {
	fsOf(t, FSLocal)
	r := &stampReader{l: ledgerOf(map[string]feedback.Entry{"a b": {Shown: 1}, "m1": {Shown: 1}}), st: stampAt(clock.Add(-1), 1)}
	e := envWith(t, func(o *Options) { o.Ledger = r })
	e.catchUp(60e9)
	if ids := e.ledgerIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("ledger %v, want only m1", ids)
	}
}
