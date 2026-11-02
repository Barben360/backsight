package session_test

import (
	"testing"

	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
)

func TestOfDocument(t *testing.T) {
	for _, c := range []struct {
		id, sid string
		ok      bool
		kind    string
	}{
		{"conversation:s1", "s1", true, "session"},
		{"conversation:s1@p", "s1", true, "session"},
		{"conversation:s1#2", "s1", true, "session"},
		{"conversation:s1@p#2", "s1", true, "session"},
		{"conversation:s1@my-proj/sub#3", "s1", true, "session"},
		{"subagent:s1:a1", "s1", true, "session"},
		{"subagent:s1:agent-0a1b2c3d", "s1", true, "session"},
		{"note:x", "", false, "note"},
		{"import:docs/a.md", "", false, "import"},
		{"import:", "", false, "import"},
		{"conversation:", "", false, "other"},
		{"conversation:@p", "", false, "other"},
		{"conversation:#2", "", false, "other"},
		{"conversation:../x", "", false, "other"},
		{"subagent:s1", "", false, "other"},
		{"subagent:s1:", "", false, "other"},
		{"subagent::a1", "", false, "other"},
		{"Conversation:s1", "", false, "other"},
		{"s1", "", false, "other"},
		{"tombstones", "", false, "other"},
		{"", "", false, "none"},
	} {
		sid, ok := session.OfDocument(c.id)
		if sid != c.sid || ok != c.ok {
			t.Errorf("OfDocument(%q) = %q, %v; want %q, %v", c.id, sid, ok, c.sid, c.ok)
		}
		if k := session.KindOfDocument(c.id); k != c.kind {
			t.Errorf("KindOfDocument(%q) = %q, want %q", c.id, k, c.kind)
		}
	}

	// the ids retain writes come back to their session
	const sid = "0e251a5b-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	tombs := []state.Tombstone{{Document: "conversation:" + sid + "@other"}, {Document: "conversation:" + sid + "@other#2"}}
	for _, id := range []string{
		retain.DocumentID(sid, "home", "home"),
		retain.DocumentID(sid, "home", "other"),
		retain.LiveDocumentID(sid, "home", "other", tombs),
	} {
		if got, ok := session.OfDocument(id); !ok || got != sid {
			t.Errorf("OfDocument(%q) = %q, %v; want %q, true", id, got, ok, sid)
		}
		if k := session.KindOfDocument(id); k != "session" {
			t.Errorf("KindOfDocument(%q) = %q, want session", id, k)
		}
	}
	if live := retain.LiveDocumentID(sid, "home", "other", tombs); live != "conversation:"+sid+"@other#3" {
		t.Fatalf("fixture: live id = %q, want a #3 successor", live)
	}
}
