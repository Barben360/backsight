package ledgerdb

import (
	"os"
	"runtime"
	"testing"

	"github.com/Barben360/backsight/internal/feedback"
)

// The ledger and its -wal and -shm are their owner's alone (0600), as every
// other state file: created so, and tightened when an older backsight left
// them 0644.
func TestLedgerFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	check := func(when string) {
		t.Helper()
		n := 0
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			st, err := os.Stat(mainPath(o) + suffix)
			if err != nil {
				continue
			}
			n++
			if m := st.Mode().Perm(); m != 0o600 {
				t.Errorf("%s: %s%s mode %v, want 0600", when, FileName, suffix, m)
			}
		}
		if n < 3 {
			t.Errorf("%s: %d files, want the ledger, its -wal and -shm", when, n)
		}
	}
	if _, err := w.Fold(t.Context(), feedback.Params{}); err != nil {
		t.Fatal(err)
	}
	check("created")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(mainPath(o)+suffix, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustCloseWriter(t, w)
	w = mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	check("reopened") // the writer holds the WAL open: -wal and -shm exist
}
