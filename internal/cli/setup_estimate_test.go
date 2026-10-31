package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/testtool"
	"github.com/Barben360/backsight/internal/transcript"
)

// Issue 33: the backfill estimate counts the notes a replay would send, a
// save naming the transverse pool going to the canonical alias.
func TestBackfillEstimateFlusherCanonicalisesNotes(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	cwd := filepath.Join(home, "billing")
	p := filepath.Join(home, "s1.jsonl")
	line := `{"type":"assistant","uuid":"a1","sessionId":"s1","cwd":"` + testtool.JSONPath(cwd) + `","timestamp":"2026-09-20T09:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__plugin_backsight_memory__memory_store","input":{"kind":"convention","text":"Tabs in Makefiles.","project":"transverse"}}]}}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	fh, _ := os.Open(p)
	recs, _, err := transcript.Read(fh, 0)
	_ = fh.Close()
	if err != nil || len(recs) == 0 {
		t.Fatalf("%v %d", err, len(recs))
	}
	idx, err := retain.LoadSentIndex(d.SentIndexDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(d, home)
	notes := estimateFlusher(d, home, cfg, time.Now(), idx).Notes("s1", recs)
	if len(notes) != 1 || notes[0].Project != cfg.Projects.Transverse[0] {
		t.Errorf("notes %+v, want project %q", notes, cfg.Projects.Transverse[0])
	}
}
