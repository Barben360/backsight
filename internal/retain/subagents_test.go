package retain

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/state"
)

// subagent writes subagents/agent-<agent>.jsonl beside session sid's
// transcript, ending with reply, and its .meta.json naming agentType.
func (e *env) subagent(sid, agent, agentType, reply string) {
	e.t.Helper()
	dir := filepath.Join(e.dir, sid, "subagents")
	_ = os.MkdirAll(dir, 0o700)
	lines := `{"type":"user","isSidechain":true,"uuid":"` + agent + `-1","timestamp":"2026-10-24T09:40:00Z","cwd":"` + e.cwd + `","message":{"role":"user","content":"Look at the export."}}` + "\n" +
		`{"type":"assistant","isSidechain":true,"uuid":"` + agent + `-2","timestamp":"2026-10-24T09:45:00Z","cwd":"` + e.cwd + `","message":{"role":"assistant","content":[{"type":"text","text":` + quote(reply) + `}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "agent-"+agent+".jsonl"), []byte(lines), 0o600); err != nil {
		e.t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "agent-"+agent+".meta.json"), []byte(`{"agentType":"`+agentType+`"}`), 0o600)
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// The reconcile rebuilds the subagent reports a restore lost, from the
// transcripts of the sessions it re-sends (M5 review rules): queued in the
// notes outbox as SubagentStop queues them, redacted and capped. Not a
// report the server still holds, not one a purge deleted (its tombstone),
// not one of a paused session, not one of a session it does not re-send.
func TestReconcileRebuildsLostSubagentReports(t *testing.T) {
	e := restoredEnv(t)
	token := "ghp_" + strings.Repeat("aB3d", 9) // built here: no token-shaped literal in the repository
	e.subagent("s2", "ag1", "Explore", "The export pages by 200; the token was "+token+" "+strings.Repeat("x", 5000))
	e.subagent("s2", "ag2", "Explore", "a purged report")
	e.subagent("s2", "ag3", "Plan", "a report the server holds")
	e.subagent("s1", "ag4", "Explore", "a report typed while s1 was paused")
	e.subagent("s3", "ag9", "Explore", "s3 is not re-sent")
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.AddTombstones(state.Tombstone{Bank: "claude-memory", Document: "subagent:s2:ag2", Session: "s2", PurgedAt: clockAt(13, 10)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(e.f.Dirs.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.SessionsDir(), "s1.json"), []byte(`{"paused":true}`), 0o600)
	e.hs.mu.Lock()
	e.hs.docs["subagent:s2:ag3"] = map[string]string{"source": "subagent"}
	e.hs.mu.Unlock()

	rec, err := e.f.Reconcile(context.Background())
	if err != nil || !reflect.DeepEqual(rec.Resend, []string{"s1", "s2"}) {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	if !reflect.DeepEqual(rec.Reports, []string{"subagent:s2:ag1"}) {
		t.Errorf("reports %v", rec.Reports)
	}
	notes, err := NotesOutbox(e.f.Dirs).read()
	if err != nil || len(notes) != 1 {
		t.Fatalf("outbox %+v %v", notes, err)
	}
	n := notes[0]
	if n.DocumentID != "subagent:s2:ag1" || n.Source != "subagent" || n.AgentType != "Explore" || n.Project != "billing" ||
		!n.CreatedAt.Equal(clockAt(9, 45)) || len([]rune(n.Content)) != ReportChars || strings.Contains(n.Content, token) ||
		!strings.HasPrefix(n.Content, "The export pages by 200") {
		t.Errorf("note %+v", n)
	}
	// Queued once: a reconcile stopped half way and done again, or the
	// next epoch, does not queue it twice.
	if _, err := e.f.rebuildReports(context.Background(), "s2", filepath.Join(e.dir, "s2.jsonl"), nil); err != nil {
		t.Fatal(err)
	}
	if notes, _ := NotesOutbox(e.f.Dirs).read(); len(notes) != 1 {
		t.Errorf("queued twice: %+v", notes)
	}
}

// A rebuilt report follows the hook's rules (issue 285 review L3): none
// with components.write off, under a headless parent session, from an
// opted-out directory, nor one whose text a purge deleted under another
// id, nor one already quarantined; its project is the hook's attribution.
func TestRebuiltReportsFollowTheHooksRules(t *testing.T) {
	const reply = "The export pages by 200."
	for name, c := range map[string]struct {
		prep    func(e *env)
		queued  bool
		project string
	}{
		"write off":       {func(e *env) { e.f.NoReports = true }, false, ""},
		"headless parent": {func(e *env) { e.f.Headless = func(string) bool { return true } }, false, ""},
		"opted-out cwd":   {func(e *env) { e.f.Exclude = []string{e.cwd} }, false, ""},
		"text purged": {func(e *env) {
			e.tombstone(state.Tombstone{Document: "note:elsewhere", ContentSHA256: ContentHash(SubagentReport(reply))})
		}, false, ""},
		"quarantined": {func(e *env) {
			e.quarantine(NewSubagentNote("claude-memory", "s2", "ag1", "Explore", "billing", reply, clockAt(9, 45)))
		}, false, ""},
		"hook's attribution": {func(e *env) { e.f.ReportProject = func(string) string { return "transverse" } }, true, "transverse"},
	} {
		t.Run(name, func(t *testing.T) {
			e := restoredEnv(t)
			e.subagent("s2", "ag1", "Explore", reply)
			c.prep(e)
			if _, err := e.f.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			notes, _ := NotesOutbox(e.f.Dirs).read()
			if c.queued != (len(notes) == 1) || len(notes) > 1 || (c.queued && notes[0].Project != c.project) {
				t.Errorf("queued %+v", notes)
			}
		})
	}
}

func (e *env) tombstone(t state.Tombstone) {
	e.t.Helper()
	t.Bank = "claude-memory"
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error { s.AddTombstones(t); return nil }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) quarantine(n Note) {
	e.t.Helper()
	if err := appendJSON(NotesOutbox(e.f.Dirs).QuarantinePath, n); err != nil {
		e.t.Fatal(err)
	}
}
