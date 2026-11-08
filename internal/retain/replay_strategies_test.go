package retain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/transcript"
)

// TestRenderStrategyReplay renders past sessions as backsight sends them,
// for a replay of the retain strategies' instructions into a throwaway
// bank (decision 0083 C6): per session, its conversation document (one
// segment holding every turn, the text a document ends up with), the notes
// it stored (memory_store calls, the document strategy) and its subagents'
// final reports (the subagent strategy). It reads only, and runs only when
// asked:
//
//	BACKSIGHT_REPLAY_SESSIONS="~/.claude/projects/<dir>/<sid>.jsonl …" \
//	BACKSIGHT_REPLAY_OUT=<dir> \
//	go test ./internal/retain -run TestRenderStrategyReplay -v
//
// Each session becomes <dir>/<sid>.json, a retain request body. Without
// BACKSIGHT_REPLAY_OUT it only logs the sizes. The project is the base
// name of the first turn's directory (the tags only).
func TestRenderStrategyReplay(t *testing.T) {
	list := strings.Fields(os.Getenv("BACKSIGHT_REPLAY_SESSIONS"))
	if len(list) == 0 {
		t.Skip("BACKSIGHT_REPLAY_SESSIONS not set")
	}
	out := os.Getenv("BACKSIGHT_REPLAY_OUT")
	const bank = "replay"
	total := 0
	for _, file := range list {
		fh, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		recs, _, err := transcript.Read(fh, 0)
		_ = fh.Close()
		if err != nil {
			t.Fatal(err)
		}
		var kept []transcript.Record
		for _, r := range recs {
			if !strings.HasPrefix(r.Entrypoint, "sdk-") && !r.Timestamp.IsZero() && (len(r.Turns) > 0 || len(r.Tools) > 0) {
				kept = append(kept, r)
			}
		}
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].Timestamp.Before(kept[j].Timestamp) })
		var turns []transcript.Record
		for _, r := range kept {
			if len(r.Turns) > 0 {
				turns = append(turns, r)
			}
		}
		if len(turns) == 0 {
			t.Logf("%s: no turns", file)
			continue
		}
		sid := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		project := filepath.Base(turns[0].CWD)
		seg := BuildSegment(bank, sid, DocumentID(sid, project, project), turns, 1, project, "replay", "replay")
		items := []hindsight.RetainItem{seg.Item}
		notes := NotesFromRecords(bank, sid, kept, nil, func(transcript.Record) (string, bool) { return project, true })
		for _, n := range notes {
			items = append(items, n.Item("replay"))
		}
		reports, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(file, ".jsonl"), "subagents", "agent-*.jsonl"))
		nReports := 0
		for _, p := range reports {
			rh, err := os.Open(p)
			if err != nil {
				continue
			}
			reply := transcript.LastReply(rh)
			_ = rh.Close()
			if text := SubagentReport(reply.Text); text != "" {
				agent := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "agent-"), ".jsonl")
				items = append(items, NewSubagentNote(bank, sid, agent, agentTypeOf(p), project, text, reply.At).Item("replay"))
				nReports++
			}
		}
		chars := 0
		for _, it := range items {
			chars += len(it.Content)
		}
		total += chars
		t.Logf("%s %s: conversation %d chars, %d notes, %d reports, %d chars in all", sid[:8], project, seg.Chars, len(notes), nReports, chars)
		if out == "" {
			continue
		}
		raw, err := json.Marshal(map[string]any{"items": items, "async": true})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, sid+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d sessions, %d chars", len(list), total)
}
