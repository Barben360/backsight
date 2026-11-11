package retain

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/hindsight"
)

// Codex's recorded threads (internal/testdata/codex/0.167.0/rollouts),
// under the names Codex gave their files, sent through its adapter
// (decision 0085 D7).
const (
	codexRoot     = "01a1c72b-9533-7bb0-9002-52dca295a387"
	codexSubagent = "01a1c72b-ae08-75b3-a4db-8a06f93c0c83"
	codexFork     = "01a1c72b-cd0a-7550-ae7a-76acc8d479c6"
	codexPacked   = "01a1c72c-6968-7223-8b71-37becf81a899"
)

var codexNames = map[string]string{
	"root.jsonl":           "rollout-2026-11-10T20-34-44-" + codexRoot + ".jsonl",
	"subagent.jsonl":       "rollout-2026-11-10T20-34-51-" + codexSubagent + ".jsonl",
	"fork.jsonl":           "rollout-2026-11-10T20-34-58-" + codexFork + ".jsonl",
	"compressed.jsonl.zst": "rollout-2026-11-10T20-35-38-" + codexPacked + ".jsonl.zst",
}

// codexRollouts writes the recorded rollouts into a Codex home, their
// working directory moved to e.cwd (project billing); the compressed one
// compressed again after the move. It returns each rollout's path.
func (e *env) codexRollouts() map[string]string {
	e.t.Helper()
	day := filepath.Join(e.t.TempDir(), "sessions", "2026", "11", "10")
	if err := os.MkdirAll(day, 0o700); err != nil {
		e.t.Fatal(err)
	}
	paths := map[string]string{}
	for fixture, name := range codexNames {
		raw, err := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "rollouts", fixture))
		if err != nil {
			e.t.Fatal(err)
		}
		if strings.HasSuffix(fixture, ".zst") {
			d, _ := zstd.NewReader(nil)
			if raw, err = d.DecodeAll(raw, nil); err != nil {
				e.t.Fatal(err)
			}
			d.Close()
		}
		raw = bytes.ReplaceAll(raw, []byte("/home/tester/work"), jsonPath(e.cwd))
		if strings.HasSuffix(fixture, ".zst") {
			enc, _ := zstd.NewWriter(nil)
			raw = enc.EncodeAll(raw, nil)
			_ = enc.Close()
		}
		p := filepath.Join(day, name)
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			e.t.Fatal(err)
		}
		paths[fixture] = p
	}
	// The recorded threads settled long ago.
	e.clock = time.Date(2026, 11, 10, 22, 0, 0, 0, time.UTC)
	return paths
}

// jsonPath is p as it reads inside a JSON string (Windows' backslashes
// escaped).
func jsonPath(p string) []byte {
	raw, _ := json.Marshal(p)
	return raw[1 : len(raw)-1]
}

// segLines decodes a segment's content into role: content lines.
func segLines(t *testing.T, it hindsight.RetainItem) []string {
	t.Helper()
	var out []string
	for l := range strings.SplitSeq(it.Content, "\n") {
		var v line
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("segment line %q: %v", l, err)
		}
		out = append(out, v.Role+": "+v.Content)
	}
	return out
}

// A Codex thread is one document, conversation:<thread id>, tagged
// harness:codex: its prompts, replies and actions, none of Codex's own
// injected text. Read again, nothing goes twice; resumed (Codex appends to
// the same file), it continues the same document; a fork is a document
// of its own, holding only its own turns; a compressed thread is sent as
// its content. The flusher picks Codex's reader from the path alone, as
// backfill hands it a mixed queue.
func TestCodexThreadsBecomeTheirDocuments(t *testing.T) {
	e := newEnv(t)
	e.f.Harnesses = []Harness{harness.Claude{}, harness.Codex{}}
	paths := e.codexRollouts()
	ctx := context.Background()

	expect(t, e.f.FlushFile(ctx, paths["root.jsonl"], Options{}), Sent, 15, 1)
	items := e.hs.itemsOf("conversation:" + codexRoot)
	if len(items) != 1 {
		t.Fatalf("%d items, want 1", len(items))
	}
	it := items[0]
	if !slices.Contains(it.Tags, "harness:codex") || !slices.Contains(it.Tags, "project:billing") || slices.Contains(it.Tags, "harness:claude-code") ||
		it.Metadata["harness"] != "codex" || it.Metadata["session_id"] != codexRoot || it.Metadata["entrypoint"] != "codex-tui" {
		t.Errorf("tags %v metadata %v", it.Tags, it.Metadata)
	}
	got := segLines(t, it)
	if len(got) != 16 || got[0] != "system: REF-ID: conversation:"+codexRoot || !strings.HasPrefix(got[1], "user: first turn <<fake") ||
		got[2] != "action: mcp__memory__memory_search codex spike" || got[len(got)-1] != "assistant: RESUMED" {
		t.Errorf("segment:\n%s", strings.Join(got, "\n"))
	}
	for _, injected := range []string{"environment_context", "Memory reminder", "Memory: an issue", "subagent_notification", "summary: the conversation so far", "skills_instructions"} {
		if strings.Contains(it.Content, injected) {
			t.Errorf("the segment holds Codex's injected %q", injected)
		}
	}
	retains := len(e.hs.retains)
	if r := e.f.FlushFile(ctx, paths["root.jsonl"], Options{}); r.Status != Nothing || len(e.hs.retains) != retains {
		t.Errorf("read again: %s (%s), %d new items", r.Status, r.Detail, len(e.hs.retains)-retains)
	}

	// Resumed: Codex appends the new turn to the same file.
	resume := `{"timestamp":"2026-11-10T21:00:00.000Z","type":"turn_context","payload":{"cwd":"` + string(jsonPath(e.cwd)) + `"}}` + "\n" +
		`{"timestamp":"2026-11-10T21:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"one more after the resume"}]}}` + "\n" +
		`{"timestamp":"2026-11-10T21:00:02.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"MORE"}]}}` + "\n"
	fh, _ := os.OpenFile(paths["root.jsonl"], os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = fh.WriteString(resume)
	_ = fh.Close()
	expect(t, e.f.FlushFile(ctx, paths["root.jsonl"], Options{}), Sent, 2, 2)
	items = e.hs.itemsOf("conversation:" + codexRoot)
	if last := items[len(items)-1]; len(items) != 2 || last.UpdateMode != "append" ||
		!slices.Equal(segLines(t, last), []string{"user: one more after the resume", "assistant: MORE"}) {
		t.Errorf("the resume's segment %+v", last)
	}

	expect(t, e.f.FlushFile(ctx, paths["fork.jsonl"], Options{}), Sent, 2, 1)
	fork := e.hs.itemsOf("conversation:" + codexFork)
	if len(fork) != 1 || !slices.Equal(segLines(t, fork[0])[1:], []string{`user: forked turn <<fake [{"say":"FORKED"}]>>`, "assistant: FORKED"}) {
		t.Errorf("the fork's document %+v", fork)
	}

	expect(t, e.f.FlushFile(ctx, paths["compressed.jsonl.zst"], Options{}), Sent, 2, 1)
	packed := e.hs.itemsOf("conversation:" + codexPacked)
	if len(packed) != 1 || packed[0].Metadata["entrypoint"] != "codex_exec" ||
		!slices.Equal(segLines(t, packed[0])[1:], []string{`user: to compress <<fake [{"say":"ok"}]>>`, "assistant: ok"}) {
		t.Errorf("the compressed thread's document %+v", packed)
	}
	gets := e.hs.gets
	if r := e.f.FlushFile(ctx, paths["compressed.jsonl.zst"], Options{}); r.Status != Nothing || e.hs.gets != gets {
		t.Errorf("an unchanged compressed thread: %s (%s), %d reads", r.Status, r.Detail, e.hs.gets-gets)
	}
}

// Lines Codex's format does not promise, or that are broken, never stop a
// flush: the thread's turns go out, the unknown type is counted for
// doctor, and the next flush has nothing left.
func TestCodexMalformedLinesNeverBlockAFlush(t *testing.T) {
	e := newEnv(t)
	paths := e.codexRollouts()
	raw, _ := os.ReadFile(paths["root.jsonl"])
	at := bytes.Index(raw, []byte(`"T1-DONE"}]`))
	at += bytes.IndexByte(raw[at:], '\n') + 1
	junk := "not json\n{\"type\":\"mystery\",\"payload\":{}}\n[\"x\"]\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":{}}}\n"
	raw = append(raw[:at:at], append([]byte(junk), raw[at:]...)...)
	_ = os.WriteFile(paths["root.jsonl"], raw, 0o600)
	e.f.Harness = harness.Codex{}
	r := e.f.FlushFile(context.Background(), paths["root.jsonl"], Options{})
	expect(t, r, Sent, 15, 1)
	if r.Unknown["mystery"] != 1 || len(r.Unknown) != 1 {
		t.Errorf("unknown %v, want the one mystery line", r.Unknown)
	}
	if r := e.f.FlushFile(context.Background(), paths["root.jsonl"], Options{}); r.Status != Nothing {
		t.Errorf("again: %s (%s)", r.Status, r.Detail)
	}
}

// A Codex thread whose session_meta names a parent is a subagent's: it is
// no conversation of its own, and it is its session's report, rebuilt as
// SubagentStop builds it (its last reply, its agent type, harness:codex)
// when the server lost it; its attribution reads the subagent's thread.
func TestCodexSubagentThreadsAreReports(t *testing.T) {
	e := newEnv(t)
	e.f.Harnesses = []Harness{harness.Codex{}}
	e.f.Attribution = Attributor{Resolver: e.f.Resolver, Dir: filepath.Join(e.t.TempDir(), "attribution"), Harnesses: e.f.Harnesses}
	paths := e.codexRollouts()
	if got := (harness.Codex{}).Conversations(harness.Env{Profile: filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(paths["root.jsonl"])))))}); slices.Contains(got, paths["subagent.jsonl"]) {
		t.Errorf("the subagent's thread is listed as a conversation: %v", got)
	}
	queued, err := e.f.rebuildReports(context.Background(), codexRoot, paths["root.jsonl"], nil, time.Time{})
	if err != nil || !slices.Equal(queued, []string{"subagent:" + codexRoot + ":" + codexSubagent}) {
		t.Fatalf("queued %v %v", queued, err)
	}
	notes, err := NotesOutbox(e.f.Dirs).read()
	if err != nil || len(notes) != 1 {
		t.Fatalf("outbox %+v %v", notes, err)
	}
	n := notes[0]
	if n.Content != "SUB-DONE" || n.AgentType != "default" || n.HarnessTag != "harness:codex" || n.Project != "billing" ||
		!n.CreatedAt.Equal(time.Date(2026, 11, 10, 20, 34, 51, 748000000, time.UTC)) {
		t.Errorf("report %+v", n)
	}
	// What SubagentStop got as the report matches the thread: decided on
	// its evidence, not left provisional.
	d, err := e.f.Attribution.Subagent(codexRoot, codexSubagent, paths["root.jsonl"], paths["subagent.jsonl"], "billing", "SUB-DONE", e.clock)
	if err != nil || d.Provisional || d.Source == SourceIncomplete || d.Source == SourceUnread {
		t.Errorf("decision %+v %v", d, err)
	}
}

// The main turns of a Codex thread are recorded for the path rule as a
// Claude Code session's are: one per prompt.
func TestCodexTurnsAreRecorded(t *testing.T) {
	e := newEnv(t)
	e.f.Harness = harness.Codex{}
	e.f.Attribution = Attributor{Resolver: e.f.Resolver, Dir: filepath.Join(e.t.TempDir(), "attribution")}
	paths := e.codexRollouts()
	r := e.f.FlushFile(context.Background(), paths["root.jsonl"], Options{})
	if len(r.Turns) != 4 || r.Turns[0].Base != "billing" || r.Turns[0].Session != codexRoot {
		t.Errorf("turns %+v, want the 4 prompts filed under billing", r.Turns)
	}
}

// The other readers (backfill's survey, the sessions waiting) take a
// Codex thread's session id and size from its adapter.
func TestCodexSurveyAndWaiting(t *testing.T) {
	e := newEnv(t)
	e.f.Harnesses = []Harness{harness.Codex{}}
	paths := e.codexRollouts()
	c, recs := e.f.Survey(paths["compressed.jsonl.zst"])
	if c.Session != codexPacked || c.Status != "send" || len(recs) != 2 || c.Project != "billing" {
		t.Errorf("survey %+v (%d records)", c, len(recs))
	}
	if sids := e.f.SessionOf(paths["fork.jsonl"]); sids != codexFork {
		t.Errorf("SessionOf %q", sids)
	}
	w := e.f.Waiting([]string{paths["root.jsonl"], paths["compressed.jsonl.zst"]})
	if len(w) != 2 || w[0].Session != codexRoot || w[1].Session != codexPacked {
		t.Errorf("waiting %+v", w)
	}
	e.f.Harness = harness.Codex{}
	expect(t, e.f.FlushFile(context.Background(), paths["compressed.jsonl.zst"], Options{}), Sent, 2, 1)
	if w := e.f.Waiting([]string{paths["compressed.jsonl.zst"]}); len(w) != 0 {
		t.Errorf("a compressed thread sent whole still waits: %+v", w)
	}
}

// A Codex thread's entrypoint is its originator, read from its rollout
// (privacy.skip_entrypoints matches it).
func TestCodexEntrypointIsTheOriginator(t *testing.T) {
	e := newEnv(t)
	paths := e.codexRollouts()
	if got := EntrypointOf(harness.Codex{}, paths["compressed.jsonl.zst"]); got != "codex_exec" {
		t.Errorf("entrypoint %q, want codex_exec", got)
	}
	if got := EntrypointOf(harness.Codex{}, paths["root.jsonl"]); got != "codex-tui" {
		t.Errorf("entrypoint %q, want codex-tui", got)
	}
}
