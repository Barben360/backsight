package feedback_test

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/session"
)

// The turn pointer repeats session.ValidID's pattern (an import cycle
// forbids the call): both accept and refuse the same ids.
func TestSessionPatternsAgree(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{
		"a", "Z", "0", "s1", "a-b_c", "0f3c9e2a-1b4d-4c8e-9f00-123456789abc", strings.Repeat("a", 128),
		"", "-a", "_a", "a b", "a/b", "../x", ".", "a.b", "a\n", "é", "a:b", strings.Repeat("a", 129),
	} {
		_, read := feedback.CurrentTurn(dir, id)
		if set := feedback.SetTurn(dir, id, feedback.Turn{ID: "t"}) == nil; set != session.ValidID(id) {
			t.Errorf("%q: the turn pointer accepts it %v, session.ValidID %v", id, set, session.ValidID(id))
		}
		if _, ok := feedback.CurrentTurn(dir, id); ok != session.ValidID(id) || read {
			t.Errorf("%q: the turn pointer reads it %v, session.ValidID %v", id, ok, session.ValidID(id))
		}
	}
}
