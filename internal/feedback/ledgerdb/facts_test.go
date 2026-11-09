package ledgerdb

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/state"
)

// mustFacts is Facts of o, failing the test on an error.
func mustFacts(t *testing.T, o Options) LedgerFacts {
	t.Helper()
	f, err := Facts(o)
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	return f
}

// TestFactsReadWithoutWriting: Facts says whether the file this binary
// uses was built, its fold's time, generation, watermark and export, and
// how many memories it counts (a restore's entry alone left out); a
// missing file stays missing, a newer one names the side file, garbage is
// its class, and an update not committed is said.
func TestFactsReadWithoutWriting(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		o := tempOpts(t)
		appendLines(t, o, showsOf(oct(1), "m1"))
		f := mustFacts(t, o)
		if f.Built || f.Newer || f.File != FileName || f.Head != 1 || f.Schema != 0 || f.OpenError != "" || f.UpdateInProgress {
			t.Errorf("%+v", f)
		}
		absent(t, mainPath(o))
		if ents, _ := os.ReadDir(feedbackDir(o)); len(ents) != 1 {
			t.Errorf("the feedback folder holds %d entries, want the month file alone", len(ents))
		}
	})
	t.Run("built", func(t *testing.T) {
		o := tempOpts(t)
		appendLines(t, o, hideOf(oct(1), "m1"), unhideOf(oct(2), "restored-only"))
		w := mustOpenWriter(t, o)
		f := mustFacts(t, o)
		if !f.Built || f.Generation == 0 || !f.FoldedAt.Equal(clock) || !f.Watermark.Equal(oct(2)) || !f.ExportBehind || f.Memories != 1 || f.Params == "" || f.OpenError != "" {
			t.Errorf("before the export: %+v", f)
		}
		if err := w.Export(); err != nil {
			t.Fatal(err)
		}
		mustCloseWriter(t, w)
		before := readBytes(t, mainPath(o))
		if f := mustFacts(t, o); f.ExportBehind || f.Schema != 1 {
			t.Errorf("after the export: %+v", f)
		}
		if !bytes.Equal(readBytes(t, mainPath(o)), before) {
			t.Error("Facts changed ledger.db")
		}
	})
	t.Run("newer", func(t *testing.T) {
		o := tempOpts(t)
		mustCloseWriter(t, mustOpenWriter(t, o))
		db, err := sql.Open(driverName, "file:"+mainPath(o))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		f := mustFacts(t, o)
		if !f.Newer || f.File != SideFileName(1) || f.Built || f.OpenError != "" {
			t.Errorf("%+v", f)
		}
		absent(t, filepath.Join(feedbackDir(o), SideFileName(1)))
	})
	t.Run("garbage", func(t *testing.T) {
		o := tempOpts(t)
		writeFile(t, mainPath(o), []byte(strings.Repeat("not a database ", 100)))
		if f := mustFacts(t, o); f.Built || f.OpenError != "notadb" {
			t.Errorf("%+v", f)
		}
	})
	t.Run("an update not committed", func(t *testing.T) {
		o := tempOpts(t)
		if err := state.WriteUpdateMarker(o.Dirs, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
			t.Fatal(err)
		}
		if f := mustFacts(t, o); !f.UpdateInProgress {
			t.Errorf("%+v", f)
		}
	})
	t.Run("a read-only folder", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("no folder permission to deny")
		}
		o := tempOpts(t)
		appendLines(t, o, showsOf(oct(1), "m1"))
		if err := os.Chmod(feedbackDir(o), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(feedbackDir(o), 0o700) })
		if f := mustFacts(t, o); f.Built || f.OpenError != "readonly" {
			t.Errorf("%+v", f)
		}
	})
}
