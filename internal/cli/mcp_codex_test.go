package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/testtool"
)

// The memory server under Codex (0085 D4, D6): each call's session from
// its own _meta, never from the process; the approval hints.

const (
	codexRoot  = "01a1c72b-9533-7bb0-9002-52dca295a387" // the recorded root session
	codexChatA = "01a1c72c-0000-7000-8000-00000000000a"
	codexChatB = "01a1c72c-0000-7000-8000-00000000000b"
	codexPID   = 5151 // the Codex process the server believes started it
)

// codexMCP is the memory server of home as Codex starts it (`mcp --harness
// codex`), on the fake bank. Its parent pid holds a current by-pid record
// of another session, and its environment names others: none may ever
// stand in for a call's own session.
func codexMCP(t *testing.T, home string) *mcp.ClientSession {
	t.Helper()
	dirs := pathsOf(home)
	if err := session.WriteLive(dirs.SessionsDir(), codexPID, session.Live{SessionID: "pid-session", Cwd: filepath.Join(home, "shop"), Project: "shop", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := newEngine(cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		return map[string]string{"CODEX_SESSION_ID": "env-session", "CLAUDE_CODE_SESSION_ID": "env-claude"}[k]
	}
	deps := newMCPDeps(harness.Codex{}, env, dirs, home, cfg, sec, eng, 0)
	deps.current = "billing"
	deps.parentPID = codexPID
	// A session the process would hold (Codex's adapter gives none): a
	// call naming none must not fall back on it.
	if deps.sessionID != "" {
		t.Fatalf("a Codex server started with session %q", deps.sessionID)
	}
	deps.sessionID = "pid-session"
	srv := newMCPServer(deps)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// codexHome is a dev install on the fake bank with a current hook record
// for each session given, in directory dir under home (its project).
func codexHome(t *testing.T, records map[string]string) string {
	t.Helper()
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	for _, p := range []string{"billing", "shop", "secret"} {
		_ = os.MkdirAll(filepath.Join(home, p), 0o700)
	}
	for sid, project := range records {
		if err := session.WriteLiveSession(pathsOf(home).SessionsDir(), session.Live{SessionID: sid, Cwd: filepath.Join(home, project), Project: project, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// codexMeta is the _meta of a Codex call: the session, the thread, and the
// turn's metadata (both ids, the workspaces).
func codexMeta(sid, thread string, workspaces ...string) map[string]any {
	ws := map[string]any{}
	for _, w := range workspaces {
		ws[w] = map[string]any{"has_changes": false}
	}
	return map[string]any{"sessionId": sid, "threadId": thread, "callId": "call_x",
		"x-codex-turn-metadata": map[string]any{"session_id": sid, "thread_id": thread, "turn_id": "t-1", "workspaces": ws}}
}

// recordedMeta is the _meta of the first tools/call of a recorded traffic
// file (internal/testdata/codex/0.167.0/mcp/).
func recordedMeta(t *testing.T, name string) map[string]any {
	t.Helper()
	fh, err := os.Open(filepath.Join("..", "testdata", "codex", "0.167.0", "mcp", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var msg struct {
			Method string
			Params struct {
				Meta map[string]any `json:"_meta"`
			}
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.Method == "tools/call" {
			return msg.Params.Meta
		}
	}
	t.Fatalf("%s: no tools/call", name)
	return nil
}

// callMeta calls a memory tool with _meta meta (none when nil).
func callMeta(t *testing.T, cs *mcp.ClientSession, meta map[string]any, name string, args map[string]any) (string, bool) {
	t.Helper()
	p := &mcp.CallToolParams{Name: name, Arguments: args}
	if meta != nil {
		p.Meta = mcp.Meta(maps.Clone(meta)) // the client writes its progress token in it
	}
	res, err := cs.CallTool(t.Context(), p)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return "", true
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// queuedNotes are the notes waiting in the outbox (the fake bank refuses
// them, so they stay).
func queuedNotes(t *testing.T, home string) []retain.Note {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl"))
	var out []retain.Note
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var n retain.Note
		if json.Unmarshal([]byte(l), &n) == nil {
			out = append(out, n)
		}
	}
	return out
}

// queuedNote is the queued note holding text.
func queuedNote(t *testing.T, home, text string) retain.Note {
	t.Helper()
	for _, n := range queuedNotes(t, home) {
		if strings.Contains(n.Content, text) {
			return n
		}
	}
	t.Errorf("no note queued with %q", text)
	return retain.Note{}
}

// The recorded calls of a root session and of its subagent (each on its
// own server process, as Codex runs them by default) pair with the root
// session: its shows, its short ids, its record's project.
func TestCodexRootAndSubagentCallsPairWithTheRootSession(t *testing.T) {
	home := codexHome(t, map[string]string{codexRoot: "billing"})
	root, sub := recordedMeta(t, "root.in.jsonl"), recordedMeta(t, "subagent.in.jsonl")
	if sub["threadId"] == codexRoot || sub["sessionId"] != codexRoot {
		t.Fatalf("the subagent fixture: %v", sub)
	}
	showIn(t, home, codexRoot, creditA)
	memories[creditA] = `{"id":"` + creditA + `","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"]}`
	retainStatus.Store(http.StatusServiceUnavailable) // notes stay queued, where the test reads them
	for name, meta := range map[string]map[string]any{"root": root, "subagent": sub} {
		cs := codexMCP(t, home)
		if out, isErr := callMeta(t, cs, meta, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}}); isErr || !strings.HasPrefix(out, creditUsedPrefix) {
			t.Errorf("%s: credit of the root's show: %v %s", name, isErr, out)
		}
		if out, isErr := callMeta(t, cs, meta, "memory_read", map[string]any{"id": "aaaaaaaa"}); isErr || !strings.Contains(out, "PostgreSQL 16 stays.") {
			t.Errorf("%s: the root's short id: %v %s", name, isErr, out)
		}
		text := "Decision (2026-11-11): the " + name + " keeps PostgreSQL 16 for partial indexes."
		if out, isErr := callMeta(t, cs, meta, "memory_store", map[string]any{"kind": "decision", "text": text}); isErr {
			t.Fatalf("%s: store: %s", name, out)
		}
		if n := queuedNote(t, home, text); n.Session != codexRoot || n.Project != "billing" {
			t.Errorf("%s: note filed under %q/%q, want the root session in billing", name, n.Session, n.Project)
		}
	}
	if tel := telemetryText(t, home); !strings.Contains(tel, `"live":"call"`) || strings.Contains(tel, `"live":"record"`) {
		t.Errorf("telemetry:\n%s", tel)
	}
}

// Two chats on one server process (Codex's MCP sharing): each call sees
// its own session's shows, short ids, turn, project and pause, never the
// other's.
func TestTwoCodexChatsOnOneServerNeverSeeEachOthersShows(t *testing.T) {
	home := codexHome(t, map[string]string{codexChatA: "billing", codexChatB: "shop"})
	cs := codexMCP(t, home)
	a, b := codexMeta(codexChatA, codexChatA), codexMeta(codexChatB, codexChatB)
	showIn(t, home, codexChatA, creditA)
	showIn(t, home, codexChatB, creditB)
	memories[creditA] = `{"id":"` + creditA + `","text":"A's fact.","type":"observation","tags":["project:billing"]}`
	memories[creditB] = `{"id":"` + creditB + `","text":"B's fact.","type":"observation","tags":["project:shop"]}`
	for _, c := range []struct {
		meta        map[string]any
		own, others string
	}{{a, "aaaaaaaa", "bbbbbbbb"}, {b, "bbbbbbbb", "aaaaaaaa"}} {
		if out, isErr := callMeta(t, cs, c.meta, "memory_credit", map[string]any{"ids": []string{c.others}}); !isErr || !strings.Contains(out, "not shown in this session") {
			t.Errorf("%s credited the other chat's %s: %v %s", c.meta["sessionId"], c.others, isErr, out)
		}
		if out, isErr := callMeta(t, cs, c.meta, "memory_credit", map[string]any{"ids": []string{c.own}}); isErr || !strings.HasPrefix(out, creditUsedPrefix) {
			t.Errorf("%s: its own %s: %v %s", c.meta["sessionId"], c.own, isErr, out)
		}
		if out, isErr := callMeta(t, cs, c.meta, "memory_read", map[string]any{"id": c.others}); !isErr || !strings.Contains(out, "no memory "+c.others) {
			t.Errorf("%s read the other chat's short id %s: %v %s", c.meta["sessionId"], c.others, isErr, out)
		}
	}
	// The recall line: under the call's session, its turn, its project.
	dir := pathsOf(home).FeedbackDir()
	_ = feedback.SetTurn(dir, codexChatA, feedback.Turn{ID: "p-a", N: 1, At: time.Now()})
	_ = feedback.SetTurn(dir, codexChatB, feedback.Turn{ID: "p-b", N: 1, At: time.Now()})
	recallAnswer = `[{"id":"m1","text":"fact","type":"observation","tags":["project:billing"],"scores":{"reranker":0.9}}]`
	callMeta(t, cs, a, "memory_search", map[string]any{"queries": []string{"one"}})
	callMeta(t, cs, b, "memory_search", map[string]any{"queries": []string{"two"}})
	got := map[string]string{}
	for _, e := range recallEvents(t, home) {
		got[e.Session] = e.Turn + "/" + e.Project + "/" + e.Client
	}
	if want := map[string]string{codexChatA: "p-a/billing/" + harnessid.Codex, codexChatB: "p-b/shop/" + harnessid.Codex}; !reflect.DeepEqual(got, want) {
		t.Errorf("recall lines %v, want %v", got, want)
	}
	// B paused: B's store is refused, A's goes on (the server knows which).
	_ = sessions(home).Update(codexChatB, func(st *session.State) error { st.SetPaused(true, time.Now()); return nil })
	retainStatus.Store(http.StatusServiceUnavailable)
	if out, isErr := callMeta(t, cs, b, "memory_store", map[string]any{"kind": "decision", "text": "Decision (2026-11-11): B keeps Redis 7."}); !isErr || !strings.Contains(out, "paused for this session") {
		t.Errorf("paused chat stored: %v %s", isErr, out)
	}
	if out, isErr := callMeta(t, cs, a, "memory_store", map[string]any{"kind": "decision", "text": "Decision (2026-11-11): A keeps Redis 7."}); isErr {
		t.Errorf("the other chat's pause refused A: %s", out)
	}
	callMeta(t, cs, b, "memory_search", map[string]any{"queries": []string{"two"}})
	if n := len(recallEvents(t, home)); n != 2 {
		t.Errorf("a paused chat's search journaled: %d recall lines", n)
	}
}

// A Codex call that names no session fails closed where the session
// matters, whatever else the server could find (its parent's by-pid
// record, its environment, the only session recorded).
func TestACodexCallWithoutItsSessionFailsClosed(t *testing.T) {
	home := codexHome(t, map[string]string{codexRoot: "billing"})
	cs := codexMCP(t, home)
	showIn(t, home, codexRoot, creditA)
	showIn(t, home, "pid-session", creditA)
	_ = feedback.SetTurn(pathsOf(home).FeedbackDir(), codexRoot, feedback.Turn{ID: "p-1", N: 1, At: time.Now()})
	memories[creditA] = `{"id":"` + creditA + `","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"]}`
	recallAnswer = `[{"id":"m1","text":"fact","type":"observation","tags":["project:billing"],"scores":{"reranker":0.9}}]`
	for name, meta := range map[string]map[string]any{
		"no _meta":            nil,
		"threadId only":       {"threadId": codexRoot, "x-codex-turn-metadata": map[string]any{"session_id": codexRoot, "thread_id": codexRoot}},
		"two different ids":   {"sessionId": codexRoot, "x-codex-turn-metadata": map[string]any{"session_id": codexChatB}},
		"not a session id":    {"sessionId": "../" + codexRoot},
		"a subagent's thread": {"threadId": "01a1c72b-ae08-75b3-a4db-8a06f93c0c83"},
	} {
		for _, args := range []map[string]any{{"ids": []string{"aaaaaaaa"}}, {"verdict": "none"}, verdict("wrong", "aaaaaaaa")} {
			if out, isErr := callMeta(t, cs, meta, "memory_credit", args); !isErr || out != noCallSession {
				t.Errorf("%s: credit %v: %v %q", name, args, isErr, out)
			}
		}
		if out, isErr := callMeta(t, cs, meta, "memory_read", map[string]any{"id": "aaaaaaaa"}); !isErr || !strings.Contains(out, "no memory aaaaaaaa") {
			t.Errorf("%s: a short id resolved without a session: %v %s", name, isErr, out)
		}
		if out, isErr := callMeta(t, cs, meta, "memory_search", map[string]any{"queries": []string{"x"}}); isErr || !strings.Contains(out, "m1") {
			t.Errorf("%s: search: %v %s", name, isErr, out)
		}
	}
	if evs := recallEvents(t, home); len(evs) != 0 {
		t.Errorf("recall journaled without a session: %+v", evs)
	}
	// No store without the call's session, paused or not, the turn's
	// directory allowed or excluded: neither its pause nor its directory
	// can be checked.
	retainStatus.Store(http.StatusServiceUnavailable)
	secret := filepath.Join(home, "secret")
	appendConfig(t, home, "[privacy]\nexclude_paths = ["+testtool.TOMLStr(secret)+"]")
	bad := map[string]any{"sessionId": "../x", "x-codex-turn-metadata": map[string]any{"workspaces": map[string]any{filepath.Join(home, "shop"): map[string]any{}}}}
	excluded := map[string]any{"sessionId": codexRoot, "x-codex-turn-metadata": map[string]any{"session_id": codexChatB, "workspaces": map[string]any{filepath.Join(home, "billing"): map[string]any{}, secret: map[string]any{}}}}
	for _, paused := range []bool{true, false} {
		_ = sessions(home).Update("other", func(st *session.State) error { st.SetPaused(paused, time.Now()); return nil })
		for name, meta := range map[string]map[string]any{"no _meta": nil, "a bad id": bad, "an excluded workspace": excluded} {
			out, isErr := callMeta(t, cs, meta, "memory_store", map[string]any{"kind": "decision", "text": "Decision (2026-11-11): keep 16."})
			if !isErr || !strings.Contains(out, "not stored") && !strings.Contains(out, "nothing stored") {
				t.Errorf("%s, paused %v: %v %s", name, paused, isErr, out)
			}
			if name == "an excluded workspace" && !strings.Contains(out, "excluded from memory") {
				t.Errorf("an excluded workspace: %s", out)
			}
		}
	}
	if n := len(queuedNotes(t, home)); n != 0 {
		t.Errorf("%d notes queued without a session", n)
	}
	if tel := telemetryText(t, home); !strings.Contains(tel, `"live":"none"`) || strings.Contains(tel, `"live":"record"`) || strings.Contains(tel, `"live":"session"`) {
		t.Errorf("telemetry:\n%s", tel)
	}
	// A search without its session is scoped to the turn's workspace, not
	// to the server's start project.
	recallTags = nil
	callMeta(t, cs, bad, "memory_search", map[string]any{"queries": []string{"x"}})
	if j := "|" + strings.Join(recallTags, "|"); !strings.Contains(j, "|project:shop,") || strings.Contains(j, "project:billing") {
		t.Errorf("search scoped to %q", recallTags)
	}
}

// A call naming its session with no hook record for it (hooks not trusted
// yet): the session is still the call's own (its pause, its shows), the
// directory the turn's workspace, which must be allowed.
func TestACodexCallWithoutAHookRecordKeepsItsOwnSession(t *testing.T) {
	home := codexHome(t, nil)
	cs := codexMCP(t, home)
	secret := filepath.Join(home, "secret")
	appendConfig(t, home, "[privacy]\nexclude_paths = ["+testtool.TOMLStr(secret)+"]")
	retainStatus.Store(http.StatusServiceUnavailable)
	m := codexMeta(codexChatA, codexChatA, filepath.Join(home, "shop"))
	showIn(t, home, codexChatA, creditA)
	if out, isErr := callMeta(t, cs, m, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}}); isErr || !strings.HasPrefix(out, creditUsedPrefix) {
		t.Errorf("credit: %v %s", isErr, out)
	}
	// Another session's pause does not stop it: the server knows its session.
	_ = sessions(home).Update(codexChatB, func(st *session.State) error { st.SetPaused(true, time.Now()); return nil })
	// Into the project of the turn's workspace, not the server's start one.
	text := "Decision (2026-11-11): shop keeps Redis 7."
	if out, isErr := callMeta(t, cs, m, "memory_store", map[string]any{"kind": "decision", "text": text}); isErr {
		t.Fatalf("store: %s", out)
	}
	if n := queuedNote(t, home, text); n.Session != codexChatA || n.Project != "shop" {
		t.Errorf("note under %q/%q", n.Session, n.Project)
	}
	// A turn working in the server's own directory keeps its start project.
	cwd, _ := os.Getwd()
	text = "Decision (2026-11-11): the start project keeps Redis 7."
	if out, isErr := callMeta(t, cs, codexMeta(codexChatA, codexChatA, cwd, filepath.Join(home, "shop")), "memory_store", map[string]any{"kind": "decision", "text": text}); isErr {
		t.Fatalf("store: %s", out)
	}
	if n := queuedNote(t, home, text); n.Project != "billing" {
		t.Errorf("in the server's directory: project %q", n.Project)
	}
	if !strings.Contains(telemetryText(t, home), `"live":"unrecorded"`) {
		t.Errorf("telemetry:\n%s", telemetryText(t, home))
	}
	// A turn working in an excluded directory: refused, nothing queued.
	before := len(queuedNotes(t, home))
	for _, ws := range [][]string{{secret}, {filepath.Join(home, "billing"), filepath.Join(secret, "sub")}} {
		mx := codexMeta(codexChatA, codexChatA, ws...)
		if out, isErr := callMeta(t, cs, mx, "memory_store", map[string]any{"kind": "decision", "text": "Decision (2026-11-11): secret keeps Redis 7."}); !isErr || !strings.Contains(out, "excluded from memory") {
			t.Errorf("%v: stored: %v %s", ws, isErr, out)
		}
		if out, isErr := callMeta(t, cs, mx, "memory_credit", verdict("wrong", "aaaaaaaa")); !isErr || !strings.Contains(out, "excluded from memory") {
			t.Errorf("%v: correction stored: %v %s", ws, isErr, out)
		}
	}
	if len(queuedNotes(t, home)) != before {
		t.Error("a note was queued from an excluded directory")
	}
	// Its own pause: refused.
	_ = sessions(home).Update(codexChatA, func(st *session.State) error { st.SetPaused(true, time.Now()); return nil })
	if out, isErr := callMeta(t, cs, m, "memory_store", map[string]any{"kind": "decision", "text": "Decision (2026-11-11): paused."}); !isErr || !strings.Contains(out, "paused for this session") {
		t.Errorf("paused: %v %s", isErr, out)
	}
	// No hook record: no recall line (no turn, no project of its own).
	recallAnswer = `[{"id":"m1","text":"fact","type":"observation","tags":["project:billing"],"scores":{"reranker":0.9}}]`
	_ = sessions(home).Update(codexChatA, func(st *session.State) error { st.SetPaused(false, time.Now()); return nil })
	callMeta(t, cs, m, "memory_search", map[string]any{"queries": []string{"x"}})
	if evs := recallEvents(t, home); len(evs) != 0 {
		t.Errorf("recall without a hook record: %+v", evs)
	}
}

// Two searches of one turn run side by side on one server, and both
// succeed, each journaled; calls of two chats interleaved never cross.
func TestParallelCodexSearchesInOneTurnBothSucceed(t *testing.T) {
	home := codexHome(t, map[string]string{codexChatA: "billing", codexChatB: "shop"})
	cs := codexMCP(t, home)
	dir := pathsOf(home).FeedbackDir()
	_ = feedback.SetTurn(dir, codexChatA, feedback.Turn{ID: "p-a", N: 1, At: time.Now()})
	_ = feedback.SetTurn(dir, codexChatB, feedback.Turn{ID: "p-b", N: 1, At: time.Now()})
	recallAnswer = `[{"id":"m1","text":"fact","type":"observation","tags":["project:billing"],"scores":{"reranker":0.9}}]`
	// Each search's recall waits until the other's has reached the bank:
	// they overlap, or both wait forever (go test's -timeout ends it).
	var mu sync.Mutex
	reached := map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})}
	active := true
	fakeHook = func(r *http.Request) {
		mu.Lock()
		on := active
		mu.Unlock()
		if !on || !strings.HasSuffix(r.URL.Path, "/memories/recall") {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		me, other := "first", "second"
		if bytes.Contains(raw, []byte("second")) {
			me, other = other, me
		}
		mu.Lock()
		select {
		case <-reached[me]:
		default:
			close(reached[me])
		}
		mu.Unlock()
		<-reached[other]
	}
	a := codexMeta(codexChatA, codexChatA)
	var wg sync.WaitGroup
	outs := make([]string, 2)
	errs := make([]bool, 2)
	for i, q := range []string{"first", "second"} {
		wg.Go(func() { outs[i], errs[i] = callMeta(t, cs, a, "memory_search", map[string]any{"queries": []string{q}}) })
	}
	wg.Wait()
	mu.Lock()
	active = false
	mu.Unlock()
	for i := range outs {
		if errs[i] || !strings.Contains(outs[i], "m1") {
			t.Errorf("search %d: %v %s", i, errs[i], outs[i])
		}
	}
	if evs := recallEvents(t, home); len(evs) != 2 || evs[0].Session != codexChatA || evs[1].Session != codexChatA || evs[0].Turn != "p-a" || evs[1].Turn != "p-a" {
		t.Errorf("recall lines %+v", evs)
	}
	// Two chats at once, many calls: each answer and line is its own chat's.
	showIn(t, home, codexChatA, creditA)
	showIn(t, home, codexChatB, creditB)
	b := codexMeta(codexChatB, codexChatB)
	for range 8 {
		wg.Go(func() {
			if out, isErr := callMeta(t, cs, a, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}}); isErr || !strings.HasPrefix(out, creditUsedPrefix) {
				t.Errorf("A: %v %s", isErr, out)
			}
		})
		wg.Go(func() {
			if out, isErr := callMeta(t, cs, b, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}}); !isErr || !strings.Contains(out, "not shown in this session") {
				t.Errorf("B credited A's show: %v %s", isErr, out)
			}
		})
		wg.Go(func() { callMeta(t, cs, b, "memory_search", map[string]any{"queries": []string{"x"}}) })
	}
	wg.Wait()
	n := map[string]int{}
	for _, e := range recallEvents(t, home) {
		n[e.Session+"/"+e.Turn+"/"+e.Project]++
	}
	if want := map[string]int{codexChatA + "/p-a/billing": 2, codexChatB + "/p-b/shop": 8}; !reflect.DeepEqual(n, want) {
		t.Errorf("recall lines %v, want %v", n, want)
	}
}

// toolAnnotations are the served tools/list's annotations, as JSON, by tool.
func toolAnnotations(t *testing.T, cs *mcp.ClientSession) map[string]map[string]any {
	t.Helper()
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, tl := range tools.Tools {
		raw, _ := json.Marshal(tl.Annotations)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out[tl.Name] = m
	}
	return out
}

// The served tools/list carries the hints Codex runs memory_store and
// memory_credit on without asking (destructive and open-world false, as
// recorded with them in checks/4-approvals-with-hints), the two others
// read-only; Claude Code's server serves the same list.
func TestServedToolsCarryTheApprovalHints(t *testing.T) {
	home := codexHome(t, nil)
	codex := toolAnnotations(t, codexMCP(t, home))
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "checks", "4-approvals-with-hints", "tools-list-annotations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded []struct {
		Name        string
		Annotations map[string]any
	}
	if err := json.Unmarshal(raw, &recorded); err != nil || len(recorded) != 4 {
		t.Fatalf("%v %d", err, len(recorded))
	}
	for _, r := range recorded {
		for k, v := range r.Annotations {
			if got, ok := codex[r.Name][k]; !ok || got != v {
				t.Errorf("%s: %s = %v (set %v), recorded %v", r.Name, k, got, ok, v)
			}
		}
	}
	for _, name := range []string{"memory_store", "memory_credit"} {
		if codex[name]["readOnlyHint"] == true {
			t.Errorf("%s read-only", name)
		}
	}
	for _, name := range []string{"memory_search", "memory_read"} {
		if codex[name]["readOnlyHint"] != true {
			t.Errorf("%s not read-only: %v", name, codex[name])
		}
	}
	if claude := toolAnnotations(t, mcpConnect(t, home)); !reflect.DeepEqual(claude, codex) {
		t.Errorf("Claude Code's tools/list differs:\n%v\n%v", claude, codex)
	}
}
