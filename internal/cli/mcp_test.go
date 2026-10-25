package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
)

// mcpSession starts the memory server on a fake bank and connects a client.
func mcpSession(t *testing.T) *mcp.ClientSession { t.Helper(); return mcpSessionWith(t, "") }

// mcpParentPID and mcpParentStart are the Claude Code pid the next
// mcpSession believes it has, and when that process started.
var (
	mcpParentPID   int
	mcpParentStart time.Time
)

// mcpSessionWith appends extra TOML to the config before the server starts.
func mcpSessionWith(t *testing.T, extra string) *mcp.ClientSession {
	t.Helper()
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	if extra != "" {
		cfgPath := filepath.Join(home, "backsight", "config", "config.toml")
		raw, _ := os.ReadFile(cfgPath)
		_ = os.WriteFile(cfgPath, append(raw, []byte(extra)...), 0o600)
	}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := pathsOf(home)
	eng, err := newEngine(cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	srv := newMCPServer(mcpDeps{dirs: d, home: home, cfg: cfg, sec: sec, engine: eng, current: "billing", tw: telemetry.New(d.State, telemetry.Common{}), parentPID: mcpParentPID, parentStart: mcpParentStart})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestMCPServesThreeToolsWithTheGuide(t *testing.T) {
	cs := mcpSession(t)
	if got := cs.InitializeResult().Instructions; got != instructions.ShortGuide {
		t.Errorf("instructions %q", got)
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, tl := range tools.Tools {
		names[tl.Name] = tl.Description
	}
	if len(names) != 3 || names["memory_search"] != instructions.SearchDescription || names["memory_store"] != instructions.StoreDescription || names["memory_read"] == "" {
		t.Errorf("tools %v", names)
	}
}

func TestMCPSearchReturnsHitsAndTheCreditFormat(t *testing.T) {
	cs := mcpSession(t)
	recallAnswer = `[{"id":"m1","text":"PostgreSQL 16 stays for billing.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.7}}]`
	out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres version"}})
	if isErr || !strings.Contains(out, "PostgreSQL 16 stays") || !strings.Contains(out, "[billing]") || !strings.Contains(out, instructions.Credit) {
		t.Errorf("%v\n%s", isErr, out)
	}
	if !strings.Contains(strings.Join(recallTags, "|"), "project:billing") {
		t.Errorf("project scope not searched: %q", recallTags)
	}
	recallAnswer = "[]"
	if out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"nothing"}}); !strings.Contains(out, "nothing in memory") {
		t.Errorf("empty: %s", out)
	}
	if out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"a", "b", "c", "d", "e"}}); !isErr || !strings.Contains(out, "1 to 4") {
		t.Errorf("five queries: %v %s", isErr, out)
	}
}

func TestMCPReadShowsSourcesAndUnknownIDs(t *testing.T) {
	cs := mcpSession(t)
	memories["m1"] = `{"id":"m1","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"],"source_memories":[{"id":"f1","text":"We keep 16.","type":"world","tags":["kind:decision"]}]}`
	if out, isErr := call(t, cs, "memory_read", map[string]any{"id": "m1"}); isErr || !strings.Contains(out, "We keep 16.") || !strings.Contains(out, "decision") {
		t.Errorf("%v %s", isErr, out)
	}
	if out, isErr := call(t, cs, "memory_read", map[string]any{"id": "nope"}); !isErr || !strings.Contains(out, "no memory") {
		t.Errorf("%v %s", isErr, out)
	}
}

func TestMCPStoreShowsSimilarMemoriesAndStores(t *testing.T) {
	cs := mcpSession(t)
	recallAnswer = `[{"id":"m9","text":"Billing uses PostgreSQL 15.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-01T09:00:00Z","scores":{"reranker":0.8}}]`
	before := retained.Load()
	out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "correction", "text": "Billing moved to PostgreSQL 16 on 2026-10-20."})
	if isErr || !strings.Contains(out, "stored (correction, project billing)") || !strings.Contains(out, "Billing uses PostgreSQL 15.") || retained.Load() != before+1 {
		t.Errorf("%v %d\n%s", isErr, retained.Load()-before, out)
	}
	if !strings.Contains(strings.Join(retainedTags, "|"), "kind:correction") {
		t.Errorf("tags %v", retainedTags)
	}
}

func TestMCPStoreRefusesSecretsAndUnknownKinds(t *testing.T) {
	cs := mcpSession(t)
	before := retained.Load()
	if out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Use DB_PASSWORD=" + "Zx9kLmQ2" + "wPq8 for prod."}); !isErr || !strings.Contains(out, "secret") {
		t.Errorf("secret: %v %s", isErr, out)
	}
	if out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "idea", "text": "A fine idea."}); !isErr || !strings.Contains(out, "kind") {
		t.Errorf("kind: %v %s", isErr, out)
	}
	if retained.Load() != before {
		t.Error("a refused note was sent")
	}
}

// A transverse alias other than the first is stored under the first: one
// transverse pool (SPEC §4).
func TestStoreUsesTheCanonicalTransverseAlias(t *testing.T) {
	cs := mcpSession(t)
	cfg, _, _ := loadServerConfig()
	if len(cfg.Projects.Transverse) < 2 {
		t.Skip("needs two aliases")
	}
	out, _ := call(t, cs, "memory_store", map[string]any{"kind": "convention", "text": "Answer in French.", "project": cfg.Projects.Transverse[1]})
	if !strings.Contains(out, "project "+cfg.Projects.Transverse[0]+")") {
		t.Errorf("%s", out)
	}
}

// A refused note sends nothing anywhere: not even the similar-memory
// recall (M2 block 1 review: a secret reached /memories/recall).
func TestMCPRefusedStoreSendsNothing(t *testing.T) {
	cs := mcpSession(t)
	for _, args := range []map[string]any{
		{"kind": "decision", "text": "Use DB_PASSWORD=" + "Zx9kLmQ2" + "wPq8 for prod."},
		{"kind": "idea", "text": "A fine idea."},
		{"kind": "decision", "text": "   "},
	} {
		recallTags = nil
		if _, isErr := call(t, cs, "memory_store", args); !isErr || len(recallTags) != 0 {
			t.Errorf("%v: isErr %v, %d recalls", args, isErr, len(recallTags))
		}
	}
}

// The MCP server learns its session from the record its Claude Code
// process's hooks keep (decision 0056): a paused session stores nothing.
func TestMCPStoreHonoursASessionPause(t *testing.T) {
	mcpParentPID = 4242
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	_ = sessions(home).Update("paused-s", func(st *session.State) error { st.Paused = true; return nil })
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4242, session.Live{SessionID: "paused-s", Cwd: home, Project: "billing"})
	before := retained.Load()
	recallTags = nil
	if out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Keep 16."}); !isErr || !strings.Contains(out, "paused") || retained.Load() != before || len(recallTags) != 0 {
		t.Errorf("%v %s", isErr, out)
	}
}

func TestMCPStoreTagsAndTheTransverseKeyword(t *testing.T) {
	cs := mcpSession(t)
	call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps PostgreSQL 16."})
	last := retainedTags[len(retainedTags)-1]
	if !strings.Contains(last, "project:billing") || !strings.Contains(last, "source:note") || !strings.Contains(last, "kind:decision") {
		t.Errorf("tags %q", last)
	}
	if !strings.Contains("|"+strings.Join(recallTags, "|"), "|project:billing,") { // scopes run in parallel: any order
		t.Errorf("similar memories not searched in the note's project first: %q", recallTags)
	}
	out, _ := call(t, cs, "memory_store", map[string]any{"kind": "convention", "text": "Answer in French.", "project": "transverse"})
	if !strings.Contains(out, "project .claude)") || !strings.Contains(retainedTags[len(retainedTags)-1], "project:.claude") {
		t.Errorf("transverse keyword: %s %q", out, retainedTags)
	}
}

func TestMCPSearchParameters(t *testing.T) {
	cs := mcpSession(t)
	recallAnswer = `[{"id":"a","text":"weak","type":"observation","tags":["project:x"],"scores":{"reranker":0.1}},
		{"id":"b","text":"strong","type":"observation","tags":["project:x"],"scores":{"reranker":0.9}}]`
	if out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}}); strings.Contains(out, "weak") || !strings.Contains(out, "strong") {
		t.Errorf("default floor 0.4: %s", out)
	}
	if out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}, "min_score": 0}); !strings.Contains(out, "weak") {
		t.Errorf("min_score 0 must widen: %s", out)
	}
	if out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}, "min_score": 0, "limit": 1}); strings.Contains(out, "weak") {
		t.Errorf("limit 1: %s", out)
	}
	recallTags = nil
	call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}, "scope": "all"})
	if len(recallTags) != 1 || recallTags[0] != "" {
		t.Errorf("scope all: %q", recallTags)
	}
	recallTags = nil
	call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}, "projects": []string{"garage"}})
	if !strings.Contains(strings.Join(recallTags, "|"), "project:garage") {
		t.Errorf("projects: %q", recallTags)
	}
	if out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}, "scope": "everything"}); !isErr || !strings.Contains(out, "scope") {
		t.Errorf("bad scope: %v %s", isErr, out)
	}
	recallAnswer = "[]"
	if out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}}); isErr || !strings.Contains(out, "nothing in memory") {
		t.Errorf("empty: %v %s", isErr, out)
	}
	recallStatus.Store(500)
	if out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}}); !isErr || !strings.Contains(out, "memory unavailable") {
		t.Errorf("server failing: %v %s", isErr, out)
	}
}

func TestMCPToolEventsAreLogged(t *testing.T) {
	cs := mcpSession(t)
	recallAnswer = `[{"id":"b","text":"strong","type":"observation","tags":["project:x"],"scores":{"reranker":0.9}}]`
	call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}})
	call(t, cs, "memory_read", map[string]any{"id": "nope"})
	files, _ := filepath.Glob(filepath.Join(pathsOf(os.Getenv("HOME")).TelemetryDir(), "*.jsonl"))
	var all string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all += string(b)
	}
	if !strings.Contains(all, `"name":"memory_search"`) || !strings.Contains(all, `"hits":1`) || !strings.Contains(all, `"name":"memory_read"`) || !strings.Contains(all, `"ok":false`) {
		t.Errorf("telemetry:\n%s", all)
	}
}

func TestMCPReadRefusesIDsThatAreNotIDs(t *testing.T) {
	cs := mcpSession(t)
	for _, id := range []string{"", "..", "../banks", "a/b"} {
		if out, isErr := call(t, cs, "memory_read", map[string]any{"id": id}); !isErr || !strings.Contains(out, "id") {
			t.Errorf("%q: %v %s", id, isErr, out)
		}
	}
}

// Concurrent searches each keep their own context (M2 block 1 review: a
// shared one was cancelled by the faster call; go test -race flagged it).
func TestMCPConcurrentSearches(t *testing.T) {
	cs := mcpSession(t)
	recallAnswer = `[{"id":"b","text":"strong","type":"observation","tags":["project:x"],"scores":{"reranker":0.9}}]`
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		go func() {
			out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}})
			if isErr || !strings.Contains(out, "strong") {
				errs <- out
				return
			}
			errs <- ""
		}()
	}
	for i := 0; i < 8; i++ {
		if e := <-errs; e != "" {
			t.Errorf("a concurrent search failed: %s", e)
		}
	}
}

// After a cd, the hooks' record gives the MCP server the live project.
func TestMCPFollowsTheLiveProject(t *testing.T) {
	mcpParentPID = 4343
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4343, session.Live{SessionID: "s1", Cwd: filepath.Join(home, "infra"), Project: "infra"})
	out, _ := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Infra keeps Traefik."})
	if !strings.Contains(out, "project infra)") {
		t.Errorf("%s", out)
	}
	// a later cd is followed: the record is read again at every call
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4343, session.Live{SessionID: "s1", Cwd: filepath.Join(home, "billing"), Project: "billing"})
	recallTags = nil
	call(t, cs, "memory_search", map[string]any{"queries": []string{"traefik"}})
	if j := "|" + strings.Join(recallTags, "|"); !strings.Contains(j, "|project:billing,") || strings.Contains(j, "project:infra") {
		t.Errorf("search not scoped to the live project: %q", recallTags)
	}
}

// 0071 C3: memory_search adds what the live directory's .backsight.toml
// relates to the project call.
func TestMCPSearchAddsTheRelatedProjects(t *testing.T) {
	mcpParentPID = 4646
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	_ = os.MkdirAll(filepath.Join(home, "billing"), 0o700)
	_ = os.WriteFile(filepath.Join(home, "billing", ".backsight.toml"), []byte(`related = ["ref-stack"]`+"\n"), 0o600)
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4646, session.Live{SessionID: "s1", Cwd: filepath.Join(home, "billing"), Project: "billing"})
	recallTags = nil
	call(t, cs, "memory_search", map[string]any{"queries": []string{"spec"}})
	if j := "|" + strings.Join(recallTags, "|"); !strings.Contains(j, "|project:billing,") || !strings.Contains(j, ",project:ref-stack") {
		t.Errorf("project call %q", recallTags)
	}
}

// Without a record (an older Claude Code, a manual run) the server keeps its
// start project and has no session.
func TestMCPWithoutALiveRecordKeepsItsStartProject(t *testing.T) {
	mcpParentPID = 4444
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSession(t)
	out, _ := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps PostgreSQL 16."})
	if !strings.Contains(out, "project billing)") {
		t.Errorf("%s", out)
	}
}

// A record written before the parent process started belongs to an older
// process that had the same pid: ignored.
func TestMCPIgnoresARecordOlderThanItsParent(t *testing.T) {
	mcpParentPID, mcpParentStart = 4545, time.Now()
	t.Cleanup(func() { mcpParentPID, mcpParentStart = 0, time.Time{} })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4545, session.Live{SessionID: "old", Cwd: filepath.Join(home, "infra"), Project: "infra", At: time.Now().Add(-time.Hour)})
	out, _ := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps PostgreSQL 16."})
	if !strings.Contains(out, "project billing)") || !strings.Contains(telemetryText(t, os.Getenv("HOME")), `"live":"stale"`) {
		t.Errorf("%s\n%s", out, telemetryText(t, os.Getenv("HOME")))
	}
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4545, session.Live{SessionID: "new", Cwd: filepath.Join(home, "infra"), Project: "infra", At: time.Now()})
	if out, _ := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Infra keeps Traefik."}); !strings.Contains(out, "project infra)") {
		t.Errorf("a current record is used: %s", out)
	}
}

// Without its session the server cannot honour a pause: while any session
// is paused, memory_store refuses; otherwise it stores.
func TestMCPStoreFailsClosedWithoutItsSessionWhileOneIsPaused(t *testing.T) {
	mcpParentPID = 4646
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	_ = sessions(home).Update("other", func(st *session.State) error { st.SetPaused(true, time.Now()); return nil })
	before := retained.Load()
	if out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Keep 16."}); !isErr || !strings.Contains(out, "a session is paused") || retained.Load() != before {
		t.Errorf("%v %s", isErr, out)
	}
	if !strings.Contains(telemetryText(t, os.Getenv("HOME")), `"live":"none"`) {
		t.Errorf("telemetry:\n%s", telemetryText(t, os.Getenv("HOME")))
	}
	_ = sessions(home).Update("other", func(st *session.State) error { st.SetPaused(false, time.Now()); return nil })
	if out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Keep 16."}); isErr || retained.Load() == before {
		t.Errorf("no pause on: stored normally: %v %s", isErr, out)
	}
}

// A pause --project taken during the session applies to the next store:
// the server reads the privacy settings again on each call.
func TestMCPStoreFollowsAProjectPauseTakenDuringTheSession(t *testing.T) {
	mcpParentPID = 4747
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	dir := filepath.Join(home, "billing")
	_ = os.MkdirAll(dir, 0o700)
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4747, session.Live{SessionID: "s1", Cwd: dir, Project: "billing", At: time.Now()})
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[privacy]\nexclude_paths = [\""+dir+"\"]\n")...), 0o600)
	before := retained.Load()
	if out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps PostgreSQL 16."}); !isErr || !strings.Contains(out, "excluded") || retained.Load() != before {
		t.Errorf("%v %s", isErr, out)
	}
}
