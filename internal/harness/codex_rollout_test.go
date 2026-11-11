package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/Barben360/backsight/internal/attribution"
	"github.com/Barben360/backsight/internal/testtool"
	"github.com/Barben360/backsight/internal/transcript"
)

// codex_rollout.go's ledger: block 6, https://github.com/Barben360/backsight/issues/281.
func init() {
	codexOwns("codex_rollout.go", codexPart{over: true,
		done: []string{"Conversations", "SessionOf", "Size", "Open", "ReadConversation", "Subagents", "AgentType"}})
}

// The recorded threads (internal/testdata/codex/0.167.0/rollouts) and the
// names Codex gave their files (the hooks' transcript_path).
const (
	rootID     = "01a1c72b-9533-7bb0-9002-52dca295a387"
	subagentID = "01a1c72b-ae08-75b3-a4db-8a06f93c0c83"
	forkID     = "01a1c72b-cd0a-7550-ae7a-76acc8d479c6"
	clearID    = "01a1c72b-dbea-7382-90b6-cfd1f7785604"
	packedID   = "01a1c72c-6968-7223-8b71-37becf81a899"
)

var codexFixtureNames = map[string]string{
	"root.jsonl":           "rollout-2026-11-10T20-34-44-" + rootID + ".jsonl",
	"subagent.jsonl":       "rollout-2026-11-10T20-34-51-" + subagentID + ".jsonl",
	"fork.jsonl":           "rollout-2026-11-10T20-34-58-" + forkID + ".jsonl",
	"clear-new.jsonl":      "rollout-2026-11-10T20-35-02-" + clearID + ".jsonl",
	"compressed.jsonl.zst": "rollout-2026-11-10T20-35-38-" + packedID + ".jsonl.zst",
}

func fixtureRollout(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "rollouts", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// codexHome is a Codex home holding the recorded rollouts where Codex
// keeps them; it returns the home and each rollout's path by fixture name.
func codexHome(t *testing.T, names ...string) (string, map[string]string) {
	t.Helper()
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "11", "10")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		for n := range codexFixtureNames {
			names = append(names, n)
		}
	}
	paths := map[string]string{}
	for _, n := range names {
		p := filepath.Join(day, codexFixtureNames[n])
		if err := os.WriteFile(p, fixtureRollout(t, n), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[n] = p
	}
	return home, paths
}

func readAll(t *testing.T, path string, from int64) ([]transcript.Record, transcript.Stats) {
	t.Helper()
	rc, err := Codex{}.Open(path, from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	recs, st, err := Codex{}.ReadConversation(rc, from)
	if err != nil {
		t.Fatal(err)
	}
	return recs, st
}

type turnLine struct{ role, text string }

func turnsOf(recs []transcript.Record) []turnLine {
	var out []turnLine
	for _, r := range recs {
		for _, tu := range r.Turns {
			out = append(out, turnLine{tu.Role, tu.Content})
		}
	}
	return out
}

// The root thread, as checked by hand against the recorded rollout:
// startup, tools (memory, shell, a patch inside the shell, a question, a
// store), a subagent, a compaction, then a resume appending to the same
// file. Only the user's typed prompts and the assistant's replies are
// text; Codex's environment context, its developer messages (skills,
// permissions, backsight's own hook context), the subagent's notification
// and the compaction's summary are not.
func TestCodexRootRolloutReadsAsCheckedByHand(t *testing.T) {
	_, paths := codexHome(t, "root.jsonl")
	recs, st := readAll(t, paths["root.jsonl"], 0)
	want := []turnLine{
		{"user", `first turn <<fake [{"call":"memory_search","args":{"queries":["codex spike"]}},{"call":"exec_command","args":{"cmd":"env | sort > /out/fixtures-env-shell.txt; echo ok"}},{"call":"exec_command","args":{"cmd":"apply_patch <<'EOF'\n*** Begin Patch\n*** Add File: hello.txt\n+hello from the fake model\n*** End Patch\nEOF\n"}},{"call":"request_user_input","args":{"questions":[{"id":"pick","header":"Pick","question":"Which one?","options":[{"label":"A (Recommended)","description":"a"},{"label":"B","description":"b"}]}]}},{"call":"memory_store","args":{"kind":"decision","text":"Decision (2026-11-10): the Codex harness records fixtures with a fake model."}},{"say":"T1-DONE"}]>>`},
		{"action", "mcp__memory__memory_search codex spike"},
		{"action", "exec_command env | sort > /out/fixtures-env-shell.txt; echo ok"},
		{"action", "exec_command apply_patch <<'EOF'"},
		{"action", "request_user_input"},
		{"action", "mcp__memory__memory_store"},
		{"assistant", "T1-DONE"},
		// a target is its first line, cut at 100 characters
		{"action", `spawn_agent sub task <<fake [{"call":"memory_search","args":{"queries":["subagent check"]}},{"call":"exec_comman`},
		{"action", "wait_agent"},
		{"assistant", "T2-DONE"},
		{"user", `third turn after compaction <<fake [{"say":"T3-DONE"}]>>`},
		{"assistant", "T3-DONE"},
		{"user", `resumed turn <<fake [{"say":"RESUMED"}]>>`},
		{"assistant", "RESUMED"},
	}
	got := turnsOf(recs)
	// the second prompt's text is long: compared by its start
	if len(got) > 8 && strings.HasPrefix(got[7].text, "second turn with a subagent <<fake") {
		got = slices.Delete(got, 7, 8)
	} else {
		t.Errorf("the second prompt is missing: %+v", got)
	}
	if !slices.Equal(got, want) {
		t.Errorf("turns:\n%s\nwant:\n%s", dumpTurns(got), dumpTurns(want))
	}
	raw := fixtureRollout(t, "root.jsonl")
	if st.Consumed != int64(len(raw)) || st.BadLines != 0 || len(st.UnknownLines) != 0 {
		t.Errorf("stats %+v, want all %d bytes consumed, no bad or unknown line", st, len(raw))
	}
	ids := map[string]bool{}
	for _, r := range recs {
		if r.CWD != "/home/tester/work" || r.Entrypoint != "codex-tui" || r.SessionID != rootID || r.Timestamp.IsZero() {
			t.Errorf("record %+v: want cwd /home/tester/work, entrypoint codex-tui, session %s, a time", r, rootID)
		}
		if ids[r.UUID] || len(r.UUID) != 36 {
			t.Errorf("record id %q repeated or malformed", r.UUID)
		}
		ids[r.UUID] = true
		if line := bytes.TrimSpace(raw[r.Start:r.End]); !json.Valid(line) {
			t.Errorf("record %s's range [%d,%d) is not one line", r.UUID, r.Start, r.End)
		}
	}
}

func dumpTurns(ts []turnLine) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString("  " + t.role + ": " + t.text + "\n")
	}
	return b.String()
}

// Tool calls keep their call id and their input, an MCP tool under
// Codex's own name for it (mcp__<server>__<tool>).
func TestCodexToolCallsKeepTheirInput(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: the recorded rollout's POSIX paths")
	_, paths := codexHome(t, "root.jsonl")
	recs, _ := readAll(t, paths["root.jsonl"], 0)
	var tools []transcript.ToolUse
	for _, r := range recs {
		tools = append(tools, r.Tools...)
	}
	if len(tools) != 7 {
		t.Fatalf("%d tool calls, want 7: %+v", len(tools), tools)
	}
	search, shell, patch := tools[0], tools[1], tools[2]
	var queries []string
	if search.Name != "mcp__memory__memory_search" || search.ID != "call_fake_6_0" || json.Unmarshal(search.Input["queries"], &queries) != nil || !slices.Equal(queries, []string{"codex spike"}) {
		t.Errorf("memory search %+v", search)
	}
	var cmd string
	if shell.Name != "exec_command" || json.Unmarshal(shell.Input["cmd"], &cmd) != nil || cmd != "env | sort > /out/fixtures-env-shell.txt; echo ok" {
		t.Errorf("shell %+v", shell)
	}
	// The patch a model Codex has no metadata for runs inside the shell:
	// attribution reads it as a write of hello.txt in the session's cwd.
	c := attribution.CallOf(patch.Name, patch.Input, recs[0].CWD, "/home/tester")
	if !c.Write || !slices.Contains(c.Paths, "/home/tester/work/hello.txt") {
		t.Errorf("the patch's evidence %+v, want a write of /home/tester/work/hello.txt", c)
	}
	var notes int
	for _, tu := range tools {
		if strings.HasSuffix(tu.Name, "memory_store") && json.Unmarshal(tu.Input["text"], new(string)) == nil {
			notes++ // what backfill replays as a saved note
		}
	}
	if notes != 1 {
		t.Errorf("%d memory_store calls with a text, want 1", notes)
	}
}

// Conversations are the root threads only, each once: the subagent's
// thread is its session's (Subagents), a fork and the session /clear
// started are new threads, and a compressed one is found too.
func TestCodexConversationsAreRootThreads(t *testing.T) {
	home, paths := codexHome(t)
	got := Codex{}.Conversations(Env{Home: t.TempDir(), Profile: home})
	want := []string{paths["root.jsonl"], paths["fork.jsonl"], paths["clear-new.jsonl"], paths["compressed.jsonl.zst"]}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("conversations\n%v\nwant\n%v", got, want)
	}
	ids := map[string]string{"root.jsonl": rootID, "fork.jsonl": forkID, "clear-new.jsonl": clearID, "compressed.jsonl.zst": packedID, "subagent.jsonl": subagentID}
	for n, id := range ids {
		if got := (Codex{}).SessionOf(paths[n]); got != id {
			t.Errorf("SessionOf(%s) = %q, want %q", n, got, id)
		}
	}
	for _, p := range []string{"/x/" + rootID + ".jsonl", "/x/rollout-2026-11-10.jsonl", "/x/rollout-a-" + rootID + ".json", "/x/rollout-a-../../etc.jsonl"} {
		if got := (Codex{}).SessionOf(p); got != "" {
			t.Errorf("SessionOf(%s) = %q, want none", p, got)
		}
	}
	if subs := (Codex{}).Subagents(paths["root.jsonl"]); !slices.Equal(subs, []string{paths["subagent.jsonl"]}) {
		t.Errorf("subagents %v, want the subagent's thread", subs)
	}
	if subs := (Codex{}).Subagents(paths["fork.jsonl"]); len(subs) != 0 {
		t.Errorf("the fork has subagents %v", subs)
	}
	if at := (Codex{}).AgentType(paths["subagent.jsonl"]); at != "default" {
		t.Errorf("agent type %q, want default (the hooks' agent_type)", at)
	}
	if at := (Codex{}).AgentType(paths["root.jsonl"]); at != "" {
		t.Errorf("the root thread has agent type %q", at)
	}
	m, ok := ReadRolloutMeta(paths["fork.jsonl"])
	if !ok || m.ID != forkID || m.ForkedFromID != rootID || m.Subagent() {
		t.Errorf("fork meta %+v %v", m, ok)
	}
	// The default home is ~/.codex.
	user := t.TempDir()
	if err := os.Rename(home, filepath.Join(user, ".codex")); err != nil {
		t.Fatal(err)
	}
	if got := (Codex{}).Conversations(Env{Home: user}); len(got) != 4 {
		t.Errorf("the default home lists %v", got)
	}
	if got := (Codex{}).Conversations(Env{}); got != nil {
		t.Errorf("no home lists %v", got)
	}
}

// While Codex compresses a thread, the plain file and its .zst copy are
// both there: the thread is listed once, plain. A cursor naming the plain
// file reads the compressed one once the plain one is gone.
func TestCodexCompressionInPlace(t *testing.T) {
	home, paths := codexHome(t, "compressed.jsonl.zst")
	packed := paths["compressed.jsonl.zst"]
	plain := strings.TrimSuffix(packed, ".zst")
	content := decompress(t, packed)
	if err := os.WriteFile(plain, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (Codex{}).Conversations(Env{Profile: home}); !slices.Equal(got, []string{plain}) {
		t.Errorf("both forms list %v, want the plain one once", got)
	}
	if err := os.Remove(plain); err != nil {
		t.Fatal(err)
	}
	if got := (Codex{}).Conversations(Env{Profile: home}); !slices.Equal(got, []string{packed}) {
		t.Errorf("compressed alone lists %v", got)
	}
	n, err := Codex{}.Size(plain)
	if err != nil || n != int64(len(content)) {
		t.Errorf("Size of the moved file %d %v, want %d", n, err, len(content))
	}
	recs, _ := readAll(t, plain, 0)
	if len(recs) != 2 {
		t.Errorf("the moved file reads %d records", len(recs))
	}
}

func decompress(t *testing.T, path string) []byte {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	d, err := zstd.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	raw, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A compressed rollout reads as its decompressed bytes do: the same
// records, ids and offsets (offsets into the content stream), its size
// the content's, from any offset.
func TestCodexCompressedRolloutReadsAsItsContent(t *testing.T) {
	dir := t.TempDir()
	_, paths := codexHome(t, "compressed.jsonl.zst")
	packed := paths["compressed.jsonl.zst"]
	content := decompress(t, packed)
	plain := filepath.Join(dir, "rollout-2026-11-10T20-35-38-"+packedID+".jsonl")
	if err := os.WriteFile(plain, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := (Codex{}).Size(packed); err != nil || n != int64(len(content)) {
		t.Fatalf("Size %d %v, want %d", n, err, len(content))
	}
	if fi, _ := os.Stat(packed); fi.Size() >= int64(len(content)) {
		t.Fatal("the fixture is not smaller than its content")
	}
	a, sa := readAll(t, packed, 0)
	b, sb := readAll(t, plain, 0)
	if len(a) != 2 || !slices.EqualFunc(a, b, sameRecord) || sa.Consumed != sb.Consumed || sa.Consumed != int64(len(content)) {
		t.Fatalf("compressed %+v (%d) and plain %+v (%d) differ", a, sa.Consumed, b, sb.Consumed)
	}
	if got := turnsOf(a); !slices.Equal(got, []turnLine{{"user", `to compress <<fake [{"say":"ok"}]>>`}, {"assistant", "ok"}}) {
		t.Errorf("turns %+v", got)
	}
	if a[0].Entrypoint != "codex_exec" {
		t.Errorf("entrypoint %q, want codex_exec (codex exec's originator)", a[0].Entrypoint)
	}
	for _, from := range []int64{a[0].End, a[1].Start} {
		ta, _ := readAll(t, packed, from)
		tb, _ := readAll(t, plain, from)
		if len(ta) != 1 || !slices.EqualFunc(ta, tb, sameRecord) || !sameRecord(ta[0], a[1]) {
			t.Errorf("from %d: compressed %+v, plain %+v, want the last record", from, ta, tb)
		}
	}
	// At its end a compressed rollout reads nothing.
	if recs, st := readAll(t, packed, int64(len(content))); len(recs) != 0 || st.Consumed != int64(len(content)) {
		t.Errorf("at the end: %+v %+v", recs, st)
	}
}

func sameRecord(a, b transcript.Record) bool {
	return a.UUID == b.UUID && a.Start == b.Start && a.End == b.End && a.CWD == b.CWD && a.SessionID == b.SessionID &&
		a.Entrypoint == b.Entrypoint && a.Timestamp.Equal(b.Timestamp) && slices.Equal(turnsOf([]transcript.Record{a}), turnsOf([]transcript.Record{b}))
}

// A .zst without declared sizes (a streaming encoder's) has its size
// counted by decompressing it.
func TestCodexCompressedSizeWithoutDeclaredSize(t *testing.T) {
	_, paths := codexHome(t, "compressed.jsonl.zst")
	content := decompress(t, paths["compressed.jsonl.zst"])
	// One frame by hand: no single segment, no declared size, a 128 KiB
	// window, raw blocks of at most 16 KiB.
	b := bytes.NewBuffer([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 7 << 3})
	for rest := content; len(rest) > 0; {
		n := min(len(rest), 16<<10)
		h := uint32(n) << 3 // raw block
		if n == len(rest) {
			h |= 1 // the last one
		}
		b.Write([]byte{byte(h), byte(h >> 8), byte(h >> 16)})
		b.Write(rest[:n])
		rest = rest[n:]
	}
	p := filepath.Join(t.TempDir(), "rollout-x-"+packedID+".jsonl.zst")
	_ = os.WriteFile(p, b.Bytes(), 0o600)
	if _, ok := zstdDeclaredSize(p); ok {
		t.Fatal("the test needs a frame without a declared size")
	}
	if n, err := (Codex{}).Size(p); err != nil || n != int64(len(content)) {
		t.Errorf("Size %d %v, want %d", n, err, len(content))
	}
	if recs, _ := readAll(t, p, 0); len(recs) != 2 {
		t.Errorf("%d records", len(recs))
	}
}

// A compressed rollout that decompresses past what backsight reads, or is
// not zstd at all, is an error for that file: nothing hangs or panics.
func TestCodexCompressedRolloutRefusals(t *testing.T) {
	_, paths := codexHome(t, "compressed.jsonl.zst")
	packed := paths["compressed.jsonl.zst"]
	old := rolloutMax
	rolloutMax = 1000
	defer func() { rolloutMax = old }()
	if _, err := (Codex{}).Size(packed); !errors.Is(err, errRolloutTooLarge) {
		t.Errorf("Size past the cap: %v", err)
	}
	rc, err := Codex{}.Open(packed, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Codex{}.ReadConversation(rc, 0)
	_ = rc.Close()
	if !errors.Is(err, errRolloutTooLarge) {
		t.Errorf("a read past the cap: %v", err)
	}
	rolloutMax = old
	junk := filepath.Join(t.TempDir(), "rollout-x-"+packedID+".jsonl.zst")
	_ = os.WriteFile(junk, []byte("\x28\xb5\x2f\xfd this is no frame at all"), 0o600)
	if _, err := (Codex{}).Size(junk); err == nil {
		t.Error("a corrupt .zst has a size")
	}
	if rc, err := (Codex{}).Open(junk, 0); err == nil {
		if _, _, err := (Codex{}).ReadConversation(rc, 0); err == nil {
			t.Error("a corrupt .zst reads without error")
		}
		_ = rc.Close()
	}
	if _, ok := ReadRolloutMeta(junk); ok {
		t.Error("a corrupt .zst has a session_meta")
	}
}

// A read that starts mid-file (a cursor) takes the thread's identity and
// the cwd in force from the lines before it, when it reads through Open;
// another reader's records before any turn_context have no cwd (the flush
// keeps such records out: privacy fails closed).
func TestCodexMidFileReadTakesItsContext(t *testing.T) {
	_, paths := codexHome(t, "root.jsonl")
	all, _ := readAll(t, paths["root.jsonl"], 0)
	from := all[0].End // after the first prompt, before its tool calls
	tail, st := readAll(t, paths["root.jsonl"], from)
	if !slices.EqualFunc(tail, all[1:], sameRecord) {
		t.Fatalf("the tail read %d records unlike the whole read's", len(tail))
	}
	raw := fixtureRollout(t, "root.jsonl")
	if st.Consumed != int64(len(raw)) {
		t.Errorf("consumed %d, want %d", st.Consumed, len(raw))
	}
	bare, _, err := Codex{}.ReadConversation(bytes.NewReader(raw[from:]), from)
	if err != nil || len(bare) != len(tail) {
		t.Fatalf("%d records %v", len(bare), err)
	}
	if bare[0].CWD != "" || bare[0].Entrypoint != "" {
		t.Errorf("a reader that is not Open's names a context: %+v", bare[0])
	}
	if last := bare[len(bare)-1]; last.CWD != "/home/tester/work" {
		t.Errorf("after the next turn_context the cwd is %q", last.CWD)
	}
}

// A changed turn_context moves the records after it to its cwd.
func TestCodexTurnContextMovesTheCwd(t *testing.T) {
	lines := strings.Join([]string{
		`{"timestamp":"2026-11-10T10:00:00Z","type":"session_meta","payload":{"id":"` + rootID + `","cwd":"/w/a","originator":"codex-tui"}}`,
		`{"timestamp":"2026-11-10T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"in a"}]}}`,
		`{"timestamp":"2026-11-10T10:00:02Z","type":"turn_context","payload":{"cwd":"/w/b"}}`,
		`{"timestamp":"2026-11-10T10:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"in b"}]}}`,
	}, "\n") + "\n"
	recs, _, err := Codex{}.ReadConversation(strings.NewReader(lines), 0)
	if err != nil || len(recs) != 2 || recs[0].CWD != "/w/a" || recs[1].CWD != "/w/b" {
		t.Errorf("%+v %v", recs, err)
	}
}

// Untrusted lines never stop a read: a line that is not JSON, not an
// object, too long, of an unknown type or with fields of the wrong
// shape is skipped (counted bad, or unknown for doctor) and the lines
// around it are read; a last line without its newline waits.
func TestCodexHostileLinesNeverStopARead(t *testing.T) {
	old := rolloutLineMax
	rolloutLineMax = 4096
	defer func() { rolloutLineMax = old }()
	user := func(text string) string {
		return `{"timestamp":"2026-11-10T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}`
	}
	lines := []string{
		`{"timestamp":"2026-11-10T10:00:00Z","type":"session_meta","payload":{"id":"` + rootID + `","cwd":"/w"}}`,
		user("before"),
		`not json at all`,
		`[1,2,3]`,
		`{"type":5}`,
		`{"type":"mystery","payload":{}}`,
		`{"type":"response_item","payload":{"type":"hologram"}}`,
		`{"type":"response_item","payload":"a string"}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":42}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":7}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{not json"}}`,
		`{"type":"response_item","payload":{"type":"function_call","arguments":"{}"}}`,
		`{"type":"response_item","payload":{"type":"local_shell_call","action":"nope"}}`,
		`{"type":"session_meta","payload":{"source":{"subagent":[1]}}}`,
		`{"type":"turn_context","payload":{"cwd":["x"]}}`,
		strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("x", 5000) + `"}]}}`,
		"\xff\xfe\x00 invalid utf-8",
		user("after"),
	}
	in := strings.Join(lines, "\n") + "\n" + user("unfinished")
	recs, st, err := Codex{}.ReadConversation(strings.NewReader(in), 0)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, tl := range turnsOf(recs) {
		texts = append(texts, tl.role+":"+tl.text)
	}
	// the function call with arguments that are no JSON object keeps them
	// as its raw input
	if !slices.Equal(texts, []string{"user:before", "action:exec_command {not json", "user:after"}) {
		t.Errorf("turns %v", texts)
	}
	if want := int64(len(in) - len(user("unfinished"))); st.Consumed != want {
		t.Errorf("consumed %d, want %d (the unfinished line waits)", st.Consumed, want)
	}
	var unknown []string
	for _, u := range st.UnknownLines {
		unknown = append(unknown, u.Type)
	}
	if !slices.Equal(unknown, []string{"mystery", "response_item/hologram"}) {
		t.Errorf("unknown %v", unknown)
	}
	if st.BadLines != 7 {
		t.Errorf("%d bad lines, want 7 (not JSON, an array, a numeric type, a string payload, the deep nesting, the long line, invalid UTF-8)", st.BadLines)
	}
}

// Without the provenance Codex 0.167 writes, its own blocks are still
// told from the user's text by their tag.
func TestCodexHarnessBlocksWithoutProvenance(t *testing.T) {
	msg := func(text string) string {
		raw, _ := json.Marshal(text)
		return `{"timestamp":"2026-11-10T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(raw) + `}]}}`
	}
	in := strings.Join([]string{
		msg("<environment_context>\n  <cwd>/w</cwd>\n</environment_context>"),
		msg("# AGENTS.md instructions for /w\n\nbe nice"),
		msg("<subagent_notification>{}</subagent_notification>"),
		msg("<user_instructions>x</user_instructions>"),
		msg("a real prompt"),
		`{"timestamp":"2026-11-10T10:00:00Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"Memory: guide"}]}}`,
		`{"timestamp":"2026-11-10T10:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]},"metadata":{"compaction_output":true}}`,
		`{"timestamp":"2026-11-10T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context/>"},{"type":"input_text","text":"typed"}],"internal_chat_message_metadata_passthrough":{"content_item_metadata":[{"provenance":{"type":"harness"}},{"provenance":{"type":"user"}}]}}}`,
	}, "\n") + "\n"
	recs, _, err := Codex{}.ReadConversation(strings.NewReader(in), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := turnsOf(recs); !slices.Equal(got, []turnLine{{"user", "a real prompt"}, {"user", "typed"}}) {
		t.Errorf("turns %+v", got)
	}
}

// An action names its target redacted, at most 100 characters.
func TestCodexActionsAreRedacted(t *testing.T) {
	secret := "sk-ant-" + strings.Repeat("a1B2", 10)
	args, _ := json.Marshal(`{"cmd":"curl -H 'x-api-key: ` + secret + `' https://example.invalid/` + strings.Repeat("p", 200) + `"}`)
	in := `{"timestamp":"2026-11-10T10:00:00Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":` + string(args) + `,"call_id":"c1"}}` + "\n" +
		`{"timestamp":"2026-11-10T10:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n*** Update File: src/a.go\n@@\n-x\n+y\n*** End Patch","call_id":"c2"}}` + "\n"
	recs, _, err := Codex{}.ReadConversation(strings.NewReader(in), 0)
	if err != nil || len(recs) != 2 {
		t.Fatalf("%+v %v", recs, err)
	}
	a := recs[0].Turns[0].Content
	if strings.Contains(a, secret) || !strings.HasPrefix(a, "exec_command curl") || len([]rune(strings.TrimPrefix(a, "exec_command "))) > 100 {
		t.Errorf("action %q", a)
	}
	if p := recs[1].Turns[0].Content; p != "apply_patch src/a.go" {
		t.Errorf("patch action %q", p)
	}
}

// Any input never panics the reader, and what it returns stays within
// what it read.
func FuzzCodexReadConversation(f *testing.F) {
	f.Add([]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n"))
	f.Add([]byte(`{"type":"session_meta","payload":{"id":"x","source":{"subagent":{"thread_spawn":{}}}}}` + "\n"))
	f.Add([]byte(`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":[\"a\"]}"}}` + "\n"))
	f.Add([]byte("\n\n{\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		recs, st, err := Codex{}.ReadConversation(bytes.NewReader(in), 0)
		if err != nil {
			t.Fatal(err)
		}
		if st.Consumed > int64(len(in)) {
			t.Fatalf("consumed %d of %d", st.Consumed, len(in))
		}
		for _, r := range recs {
			if r.Start < 0 || r.End > st.Consumed || r.Start >= r.End || len(r.Turns) == 0 {
				t.Fatalf("record %+v", r)
			}
		}
	})
}
