package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/transcript"
)

// fakeTool is a second harness, as a test registers one: its own hook
// format and answers, its own conversation store, file format and tag.
// What these tests never reach (install, drift) is Claude Code's.
type fakeTool struct{ harness.Claude }

// fakeToolEnv makes a test binary started as a hook child register
// fakeTool too, as the real binary registers its adapters (TestMain).
const fakeToolEnv = "BACKSIGHT_TEST_FAKE_HARNESS"

func (fakeTool) ID() string         { return "fake-tool" }
func (fakeTool) Name() string       { return "Fake Tool" }
func (fakeTool) Tag() string        { return "harness:fake-tool" }
func (fakeTool) ProfileVar() string { return "" }
func (fakeTool) ToolPrefix() string { return "fake__backsight__" }

func (fakeTool) PairsBySession(string) bool { return true }

func (fakeTool) Locate(env harness.Env) harness.Location {
	dir := filepath.Join(env.Home, ".fake-tool")
	return harness.Location{Dir: dir, Settings: filepath.Join(dir, "config"), Conversations: filepath.Join(dir, "sessions")}
}

// DecodeHook reads the fake tool's own event shape.
func (fakeTool) DecodeHook(_ string, raw []byte) (harness.HookInput, error) {
	var in struct {
		Conversation, Folder, Said, Agent, AgentLog, Report, Tool, Failure string
		ToolArgs                                                           json.RawMessage
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return harness.HookInput{}, err
	}
	return harness.HookInput{SessionID: in.Conversation, Cwd: in.Folder, Prompt: in.Said,
		AgentID: in.Agent, AgentTranscriptPath: in.AgentLog, LastAssistantMessage: in.Report,
		ToolName: in.Tool, ToolInput: in.ToolArgs, Error: in.Failure}, nil
}

// MCPCallSession is the call's own _meta.sessionId: one server may serve
// several sessions.
func (fakeTool) MCPCallSession(meta map[string]any, _ func(string) string) string {
	s, _ := meta["sessionId"].(string)
	return s
}

// EncodeHook answers in the fake tool's own shape.
func (fakeTool) EncodeHook(o harness.HookOutput) []byte {
	if o.Context == "" && o.Banner == "" && o.Block == "" && o.Deny == "" {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"fake": o})
	return raw
}

// The fake store keeps each conversation gzipped, named
// rollout-<time>-<session uuid>.fz: neither its name nor its bytes are
// Claude Code's.
func (f fakeTool) Conversations(env harness.Env) []string {
	files, _ := filepath.Glob(filepath.Join(f.Locate(env).Conversations, "rollout-*.fz"))
	slices.Sort(files)
	return files
}

// SessionOf is the uuid ending the file name.
func (fakeTool) SessionOf(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".fz")
	if !strings.HasPrefix(name, "rollout-") || len(name) < 36 {
		return ""
	}
	return name[len(name)-36:]
}

// unzip is the content stream of a fake conversation.
func unzip(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

func (fakeTool) Size(path string) (int64, error) {
	raw, err := unzip(path)
	return int64(len(raw)), err
}

func (fakeTool) Open(path string, from int64) (io.ReadCloser, error) {
	raw, err := unzip(path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(raw[min(from, int64(len(raw))):])), nil
}

// writeFakeConversation writes lines, gzipped, as the fake store does.
func writeFakeConversation(t *testing.T, path, lines string) {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = zw.Write([]byte(lines))
	_ = zw.Close()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ReadConversation reads lines role|uuid|time|cwd|text.
func (fakeTool) ReadConversation(r io.Reader, from int64) ([]transcript.Record, transcript.Stats, error) {
	raw, err := io.ReadAll(r)
	st := transcript.Stats{Consumed: from}
	var recs []transcript.Record
	for off := from; ; {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		line, start := string(raw[:i]), off
		raw, off = raw[i+1:], off+int64(i+1)
		st.Consumed = off
		f := strings.SplitN(line, "|", 5)
		if len(f) != 5 {
			st.BadLines++
			continue
		}
		at, _ := time.Parse(time.RFC3339, f[2])
		recs = append(recs, transcript.Record{UUID: f[1], CWD: f[3], Timestamp: at, Start: start, End: off,
			Turns: []transcript.Turn{{Role: f[0], Content: f[4], Timestamp: at}}})
	}
	return recs, st, err
}

// withFakeTool registers fakeTool here and in hook children.
func withFakeTool(t *testing.T) {
	t.Helper()
	t.Cleanup(harness.Register(fakeTool{}))
	t.Setenv(fakeToolEnv, "1")
}

// A second adapter, registered next to Claude Code's, is all it takes for
// `backsight hook --harness <id>` to read that tool's events and answer in
// its shape; Claude Code's hooks still read and answer Claude Code's; an
// unknown harness gets nothing.
func TestASecondHarnessGetsItsHooksDecodedAndAnswered(t *testing.T) {
	home := hookHome(t)
	withFakeTool(t)
	childFromTestBinary(t, "")
	// The harness's pid is known: a record by session id is written only
	// where the adapter says the pid may not pair.
	t.Setenv(claudePIDEnv, "31337")
	billing := filepath.Join(home, "billing")
	in, _ := json.Marshal(map[string]string{"conversation": "fk1", "folder": billing})
	code, out := runHookCmd(t, string(in), "--harness", "fake-tool", "session-start")
	var fake struct {
		Fake harness.HookOutput `json:"fake"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &fake) != nil || fake.Fake.Event != harness.EventSessionStart || !strings.HasPrefix(fake.Fake.Context, instructions.Guide) {
		t.Fatalf("exit %d, printed %q", code, out)
	}
	// Decoded with the fake's field names: its session and directory, kept
	// by session id because the fake pairs by session.
	if l, ok := session.ReadLiveSession(pathsOf(home).SessionsDir(), "fk1"); !ok || l.Project != "billing" || l.Cwd != billing {
		t.Errorf("live record %+v %v", l, ok)
	}

	code, out = runHookCmd(t, `{"session_id":"cc1","source":"startup","cwd":"`+jsonPathOf(billing)+`"}`, "session-start")
	var claude hookOut
	if code != 0 || json.Unmarshal([]byte(out), &claude) != nil || claude.Specific.Event != "SessionStart" || !strings.HasPrefix(claude.Specific.Context, instructions.Guide) {
		t.Errorf("Claude Code's hook: exit %d, printed %q", code, out)
	}
	// Claude Code pairs by pid but on Windows: no record by session id.
	if _, ok := session.ReadLiveSession(pathsOf(home).SessionsDir(), "cc1"); ok != (runtime.GOOS == "windows") {
		t.Errorf("Claude Code's session record by id: %v on %s", ok, runtime.GOOS)
	}
	if _, ok := session.ReadLive(pathsOf(home).SessionsDir(), 31337); !ok {
		t.Error("no record under the harness's pid")
	}

	if code, out := runHookCmd(t, `{"session_id":"cc2","source":"startup"}`, "--harness", "nope", "session-start"); code != 0 || out != "" {
		t.Errorf("unknown harness: exit %d, printed %q", code, out)
	}
	if _, ok := session.ReadLiveSession(pathsOf(home).SessionsDir(), "cc2"); ok {
		t.Error("an unknown harness's hook ran")
	}
}

func jsonPathOf(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }

// A hook child started for an id no longer known prints nothing, and one
// started without an id is Claude Code's.
func TestHookChildHarnessFromItsParent(t *testing.T) {
	home := hookHome(t)
	// runHookChild marks this process a hook child from now on: later
	// in-process handlers would count their budget from this test.
	old := hookChildStart
	t.Cleanup(func() { hookChildStart = old })
	in := `{"session_id":"cc3","source":"startup","cwd":"` + jsonPathOf(filepath.Join(home, "billing")) + `"}`
	t.Setenv(hookHarnessEnv, "gone")
	var out bytes.Buffer
	if code := runHookChild(t.Context(), []string{"session-start"}, strings.NewReader(in), &out); code != 0 || out.Len() != 0 {
		t.Errorf("unknown harness: exit %d, printed %q", code, out.String())
	}
	t.Setenv(hookHarnessEnv, "")
	if code := runHookChild(t.Context(), []string{"session-start"}, strings.NewReader(in), &out); code != 0 || !strings.Contains(out.String(), `"hookEventName":"SessionStart"`) {
		t.Errorf("no harness named: exit %d, printed %q", code, out.String())
	}
}

// A second adapter's conversations are discovered in its own store, read
// with its own parser and sent with its own tag, in the same flush as
// Claude Code's, whose sessions keep theirs. The store's files are
// compressed and named after more than the session: the session comes
// from the adapter, and cursors count in the decompressed stream.
func TestASecondHarnessConversationsAreSentWithItsTag(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)) // retain_since before the sessions
	run(t, "jobs", "tick")
	withFakeTool(t)
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	writeTranscript(t, home, "live", at)
	const sid = "0193f0aa-1111-4222-8333-444455556666"
	fk := filepath.Join(home, ".fake-tool", "sessions", "rollout-2026-10-24T09-00-00-"+sid+".fz")
	line := "user|fk1-u1|" + at.Format(time.RFC3339) + "|" + filepath.Join(home, "billing") + "|The fake tool keeps billing on PostgreSQL 17.\n"
	writeFakeConversation(t, fk, line)
	code, out, errOut := run(t, "retain", "flush")
	if code != output.OK || !strings.Contains(out, "live (billing") || !strings.Contains(out, sid+" (billing") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if cur, ok, err := retain.LoadCursor(pathsOf(home).CursorsDir(), sid); err != nil || !ok || cur.Offset != int64(len(line)) {
		t.Errorf("cursor under the session %+v %v %v, want offset %d", cur, ok, err, len(line))
	}
	byTag := map[string]string{}
	for i, tags := range retainedTags {
		for _, tag := range strings.Split(tags, ",") {
			if strings.HasPrefix(tag, "harness:") {
				byTag[tag] = retainedContents[i]
			}
		}
	}
	if !strings.Contains(byTag["harness:fake-tool"], "PostgreSQL 17") || !strings.Contains(byTag["harness:claude-code"], "PostgreSQL 16") || len(byTag) != 2 {
		t.Errorf("sent by tag: %q", byTag)
	}
	// One session of the second harness, named by its id: only what was
	// appended goes out.
	before := len(retainedTags)
	writeFakeConversation(t, fk, line+"assistant|fk1-a1|"+at.Add(time.Minute).Format(time.RFC3339)+"|"+filepath.Join(home, "billing")+"|Noted.\n")
	if code, out, _ := run(t, "retain", "flush", "--session", sid); code != output.OK || len(retainedTags) != before+1 || !strings.Contains(retainedTags[before], "harness:fake-tool") ||
		!strings.Contains(retainedContents[before], "Noted.") || strings.Contains(retainedContents[before], "PostgreSQL 17") {
		t.Errorf("exit %d\n%s\n%v %q", code, out, retainedTags[before:], retainedContents[before:])
	}
}

// The handlers run for the harness of their context: a subagent's report
// is queued with that harness's tag, a session end flushes the session it
// decoded.
func TestHandlersRunForTheirContextsHarness(t *testing.T) {
	home := hookHome(t)
	ctx := harness.WithAdapter(context.Background(), fakeTool{})
	agentLog := filepath.Join(t.TempDir(), "agent.log")
	_ = os.WriteFile(agentLog, []byte("ran\n"), 0o600)
	raw, _ := json.Marshal(map[string]string{"conversation": "fk1", "folder": filepath.Join(home, "billing"), "agent": "agent-7",
		"agentLog": agentLog, "report": padTo("Found the leak in pool.go.", 450)})
	if _, err := handleHook(ctx, harness.EventSubagentStop, raw); err != nil {
		t.Fatal(err)
	}
	notes, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl"))
	if !strings.Contains(string(notes), `"document_id":"subagent:fk1:agent-7"`) || !strings.Contains(string(notes), `"harness_tag":"harness:fake-tool"`) {
		t.Errorf("outbox:\n%.600s", notes)
	}

	var got []string
	old := spawnDetached
	spawnDetached = func(args ...string) error { got = args; return nil }
	t.Cleanup(func() { spawnDetached = old })
	raw, _ = json.Marshal(map[string]string{"conversation": "fk1"})
	if _, err := handleHook(ctx, harness.EventSessionEnd, raw); err != nil || strings.Join(got, " ") != "retain flush --session fk1" {
		t.Errorf("%v: spawned %v", err, got)
	}
}

// What names no harness (a flusher, an older note) is sent as Claude
// Code's, by the adapter's own id and tag.
func TestRetainsDefaultHarnessIsClaudeCodes(t *testing.T) {
	var a retain.Harness = harness.Default()
	if d := retain.DefaultHarness(); d.ID() != a.ID() || d.Tag() != a.Tag() {
		t.Errorf("retain's default %s %s, the adapter's %s %s", d.ID(), d.Tag(), a.ID(), a.Tag())
	}
}
