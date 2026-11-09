package ledgerdb

import (
	"os/exec"
	"strings"
	"testing"
)

// TestLedgerDBNeverImportsTheIndex: the ledger database shares the
// index's SQLite helpers through internal/sqlitedb, never the index
// itself.
func TestLedgerDBNeverImportsTheIndex(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	have := map[string]bool{}
	for _, d := range strings.Fields(string(out)) {
		have[d] = true
	}
	const mod = "github.com/Barben360/backsight/internal/"
	for _, want := range []string{"feedback/ledgerdb", "feedback", "sqlitedb"} {
		if !have[mod+want] {
			t.Fatalf("go list -deps does not list %s; the check below would prove nothing", mod+want)
		}
	}
	for d := range have {
		if d == mod+"index" || strings.HasPrefix(d, mod+"index/") {
			t.Errorf("internal/feedback/ledgerdb depends on %s", d)
		}
	}
}
