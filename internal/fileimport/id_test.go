package fileimport

import (
	"regexp"
	"testing"

	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

func TestSourceIDStable(t *testing.T) {
	a := SourceID("m1", "/home/u/.claude/LIFEOS/MEMORY")
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(a) {
		t.Fatalf("SourceID = %q, want 12 hex characters", a)
	}
	if b := SourceID("m1", "/home/u/.claude/LIFEOS/MEMORY"); b != a {
		t.Errorf("not stable: %q vs %q", a, b)
	}
	if SourceID("m2", "/home/u/.claude/LIFEOS/MEMORY") == a || SourceID("m1", "/home/u/.claude/LIFEOS/USER") == a {
		t.Error("another machine or root gave the same id")
	}
}

func TestSourceIDIsTheHashPrefix(t *testing.T) {
	if got, want := SourceID("m1", "/r"), hashOf([]byte("m1\n/r"))[:12]; got != want {
		t.Errorf("SourceID = %q, want %q", got, want)
	}
}

func TestDocumentIDSuccessor(t *testing.T) {
	base := "import:abc/a/b.md"
	if got := DocumentID("abc", "a/b.md", nil); got != base {
		t.Errorf("DocumentID = %q, want %q", got, base)
	}
	one := []state.Tombstone{{Bank: "b", Document: base}}
	if got := DocumentID("abc", "a/b.md", one); got != base+"#2" {
		t.Errorf("after one purge: %q", got)
	}
	two := []state.Tombstone{one[0], {Bank: "b", Document: base + "#2"}}
	if got := DocumentID("abc", "a/b.md", two); got != base+"#3" {
		t.Errorf("after two purges: %q", got)
	}
}

func TestDocumentIDWithHashInName(t *testing.T) {
	id := DocumentID("abc", "x#2.md", nil)
	if id != "import:abc/x#2.md" {
		t.Fatalf("DocumentID = %q", id)
	}
	if retain.BaseDocumentID(id) != id {
		t.Errorf("BaseDocumentID(%q) = %q: a name with # reads as a successor", id, retain.BaseDocumentID(id))
	}
}
