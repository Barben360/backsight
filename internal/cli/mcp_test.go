package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
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

func TestMCPServesFourTools(t *testing.T) {
	cs := mcpSession(t)
	if got := cs.InitializeResult().Instructions; got != instructions.ShortGuide {
		t.Errorf("instructions %q", got)
	}
	names := toolNames(t, cs)
	if len(names) != 4 || names["memory_search"] != instructions.SearchDescription || names["memory_store"] != instructions.StoreDescription ||
		names["memory_read"] == "" || names["memory_credit"] != instructions.CreditDescription {
		t.Errorf("tools %v", names)
	}
	// feedback off: the tool is not offered
	if names := toolNames(t, mcpSessionWith(t, "\n[feedback]\nenabled = false\n")); len(names) != 3 || names["memory_credit"] != "" {
		t.Errorf("feedback off: tools %v", names)
	}
}

const searchTrailer = "If a memory informs your answer, call memory_credit with its id before your final message.\n"

// v7: the search result asks for a declaration, not a citation format;
// an empty search asks for nothing.
func TestSearchTrailerAsksForACredit(t *testing.T) {
	out := mcpSearchText(recall.Result{Hits: []recall.Hit{{ID: "0b4c2a52-0000-4000-8000-000000000001", Text: "fact", Project: "billing", Age: "2d", Score: 0.9}}}, true)
	if !strings.HasSuffix(out, "\n"+searchTrailer) || strings.Contains(out, instructions.Credit) || strings.Contains(out, "Cite what you use") {
		t.Errorf("trailer:\n%s", out)
	}
	if out := mcpSearchText(recall.Result{}, true); out != "nothing in memory\n" {
		t.Errorf("empty search: %q", out)
	}
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]string {
	t.Helper()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, tl := range tools.Tools {
		names[tl.Name] = tl.Description
	}
	return names
}

func TestMCPSearchReturnsHitsAndTheCreditFormat(t *testing.T) {
	cs := mcpSession(t)
	recallAnswer = `[{"id":"m1","text":"PostgreSQL 16 stays for billing.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.7}}]`
	out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres version"}})
	if isErr || !strings.Contains(out, "PostgreSQL 16 stays") || !strings.Contains(out, "[billing]") || !strings.HasSuffix(out, searchTrailer) {
		t.Errorf("%v\n%s", isErr, out)
	}
	if !strings.Contains(strings.Join(recallTags, "|"), "project:billing") {
		t.Errorf("project scope not searched: %q", recallTags)
	}
	recallAnswer = `[{"id":"m1","text":"PostgreSQL 16 stays for billing.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","source_fact_ids":["f1"],"scores":{"reranker":0.7}}]`
	recallSourceFacts = `{"f1":{"tags":["kind:decision","kind:pitfall"]}}`
	if out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres version"}}); !strings.Contains(out, "(2026-10-20 [decision, pitfall],") {
		t.Errorf("kinds missing from the tool's answer: %s", out)
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

// A memory server started before connect holds the old server's config:
// its store queues the note and sends nothing, the next tick sends it.
func TestMCPStoreAfterConnectStartedOverKeepsTheNote(t *testing.T) {
	cs := mcpSession(t)
	d := pathsOf(os.Getenv("HOME"))
	if err := state.Update(d, "connect", func(s *state.State) error { s.SendsGeneration++; return nil }); err != nil {
		t.Fatal(err)
	}
	before := retained.Load()
	out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps PostgreSQL 16 for partial indexes."})
	if isErr || !strings.Contains(out, "queued (decision, project billing)") || retained.Load() != before {
		t.Errorf("%v, %d retained\n%s", isErr, retained.Load()-before, out)
	}
	if n, _ := notesOutbox(d).Pending(); n != 1 {
		t.Errorf("want the note queued, got %d", n)
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
	_ = os.WriteFile(filepath.Join(home, "billing", ".backsight.toml"), []byte(`related = ["atlas-spec"]`+"\n"), 0o600)
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4646, session.Live{SessionID: "s1", Cwd: filepath.Join(home, "billing"), Project: "billing"})
	recallTags = nil
	call(t, cs, "memory_search", map[string]any{"queries": []string{"spec"}})
	if j := "|" + strings.Join(recallTags, "|"); !strings.Contains(j, "|project:billing,") || !strings.Contains(j, ",project:atlas-spec") {
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

// rotateTo gives secrets.toml a new API key, the only one the fake server
// accepts from now on (a server token --rotate on this host).
func rotateTo(t *testing.T, home string) {
	t.Helper()
	var fresh config.Secrets
	config.GenerateMissing(&fresh)
	if err := config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { s.APIKey = fresh.APIKey; return nil }); err != nil {
		t.Fatal(err)
	}
	requireKey.Store(true)
	acceptedKey.Store(fresh.APIKey)
}

// A server started on the old key follows secrets.toml once the server
// refuses that key: each tool works on its first call after the rotation.
func TestMCPFollowsARotatedKey(t *testing.T) {
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	memories["m1"] = `{"id":"m1","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"]}`
	recallAnswer = `[{"id":"m1","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.7}}]`
	rotateTo(t, home)
	if out, isErr := call(t, cs, "memory_read", map[string]any{"id": "m1"}); isErr || !strings.Contains(out, "PostgreSQL 16 stays.") {
		t.Errorf("read: %v %s", isErr, out)
	}
	rotateTo(t, home)
	if out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}}); isErr || !strings.Contains(out, "PostgreSQL 16 stays.") {
		t.Errorf("search: %v %s", isErr, out)
	}
	rotateTo(t, home)
	before := retained.Load()
	if out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps PostgreSQL 16 on 2026-10-20."}); isErr || !strings.Contains(out, "stored (decision") || retained.Load() != before+1 {
		t.Errorf("store: %v %d %s", isErr, retained.Load()-before, out)
	}
}

// A refused key that secrets.toml still holds is not tried again, and a
// new key the server refuses too is tried once: never a loop.
func TestMCPRefusedKeyIsTriedOnce(t *testing.T) {
	cs := mcpSession(t)
	requireKey.Store(true)
	acceptedKey.Store("a-key-nobody-has")
	if out, isErr := call(t, cs, "memory_read", map[string]any{"id": "m1"}); !isErr || !strings.Contains(out, "unauthorized") || refusedCalls.Load() != 1 {
		t.Errorf("unchanged key: %d calls, %v %s", refusedCalls.Load(), isErr, out)
	}
	rotateTo(t, os.Getenv("HOME"))
	acceptedKey.Store("a-key-nobody-has")
	refusedCalls.Store(0)
	if _, isErr := call(t, cs, "memory_read", map[string]any{"id": "m1"}); !isErr || refusedCalls.Load() != 2 {
		t.Errorf("new key refused too: %d calls, %v", refusedCalls.Load(), isErr)
	}
}

const (
	creditA = "aaaaaaaa-0000-4000-8000-000000000001"
	creditB = "bbbbbbbb-0000-4000-8000-000000000002"
)

// creditSession is a memory server whose Claude Code process has a
// current record for session S, project proj.
func creditSession(t *testing.T) (*mcp.ClientSession, string) {
	t.Helper()
	mcpParentPID = 4848
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4848, session.Live{SessionID: "S", Cwd: filepath.Join(home, "proj"), Project: "proj", At: time.Now()})
	return cs, home
}

// showIn journals what a hook shows: ids of project proj shown in session
// sess, in project proj.
func showIn(t *testing.T, home, sess string, ids ...string) {
	t.Helper()
	items := make([]feedback.Item, 0, len(ids))
	for i, id := range ids {
		items = append(items, feedback.Item{ID: id, Rank: i + 1, Project: "proj"})
	}
	showItems(t, home, sess, "search", "proj", items...)
}

// showItems journals a shows event of path in session sess, whose
// session project is sessProject.
func showItems(t *testing.T, home, sess, path, sessProject string, items ...feedback.Item) {
	t.Helper()
	j := feedback.NewJournal(pathsOf(home).FeedbackDir(), feedback.Common{Session: sess, Project: sessProject})
	if err := j.Append(feedback.Event{Kind: "shows", Turn: "p-1", Path: path, Items: items}); err != nil {
		t.Fatal(err)
	}
}

// creditEvent: one telemetry credit event holds every field given.
func creditEvent(t *testing.T, home string, fields ...string) bool {
	t.Helper()
	for _, line := range strings.Split(telemetryText(t, home), "\n") {
		ok := strings.Contains(line, `"event":"credit"`)
		for _, f := range fields {
			ok = ok && strings.Contains(line, f)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestCreditUsedReturnsTheLineToPrint(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	before := len(journalEvents(t, home))
	out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}})
	if isErr || !strings.HasPrefix(out, "Credited.") || !strings.Contains(out, "> 🧠 **From memory (proj · aaaaaaaa)** — ") || !strings.Contains(out, "final message") {
		t.Errorf("%v\n%s", isErr, out)
	}
	if n := len(journalEvents(t, home)); n != before {
		t.Errorf("the server journaled %d events: only the hook does", n-before)
	}
	if !creditEvent(t, home, `"verdict":"used"`, `"ids_n":1`, `"ok":true`) {
		t.Errorf("telemetry:\n%s", telemetryText(t, home))
	}
}

// The line names each memory's own project, not the session's: a session
// in billing shown an infra memory credits it to infra.
func TestCreditNamesEachMemorysOwnProject(t *testing.T) {
	cs, home := creditSession(t)
	showItems(t, home, "S", "search", "billing", feedback.Item{ID: creditB, Rank: 1, Project: "infra"}, feedback.Item{ID: creditA, Rank: 2, Project: "billing"})
	showItems(t, home, "S", "read", "billing", feedback.Item{ID: creditB}) // a read names no project: the search's stays
	out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"bbbbbbbb"}})
	if isErr || !strings.Contains(out, "From memory (infra · bbbbbbbb)**") {
		t.Errorf("%v\n%s", isErr, out)
	}
	// ids of two projects: one line each, full ids and prefixes alike
	out, isErr = call(t, cs, "memory_credit", map[string]any{"ids": []string{creditB, "aaaaaaaa"}, "verdict": "used"})
	want := "Credited. Print these lines in your final message:\n" +
		"> 🧠 **From memory (infra · bbbbbbbb)** — <the facts you used, in your words>\n" +
		"> 🧠 **From memory (billing · aaaaaaaa)** — <the facts you used, in your words>"
	if isErr || out != want {
		t.Errorf("%v\n%s", isErr, out)
	}
}

// Another session's shows cannot be credited here.
func TestCreditSeesOnlyItsSessionsShows(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S2", creditA)
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}}); !isErr || !strings.Contains(out, "aaaaaaaa: not shown in this session") {
		t.Errorf("%v %s", isErr, out)
	}
}

func TestCreditIsAllOrNothing(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA, "aaaaaaaa-9999-4000-8000-000000000009")
	out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{creditA, "cccccccc"}})
	if !isErr || !strings.Contains(out, "cccccccc: not shown in this session") || strings.Contains(out, "aaaaaaaa: ") || strings.Contains(out, "From memory") {
		t.Errorf("%v %s", isErr, out)
	}
	if !creditEvent(t, home, `"verdict":"used"`, `"ids_n":2`, `"ok":false`) {
		t.Errorf("telemetry:\n%s", telemetryText(t, home))
	}
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}}); !isErr || !strings.Contains(out, "aaaaaaaa: matches several memories") {
		t.Errorf("ambiguous: %v %s", isErr, out)
	}
}

// A credit sent with the search that shows the id (parallel calls) comes
// before the hook journaled the show: refused, then accepted once it is.
func TestCreditBeforeTheSearchResultIsRefused(t *testing.T) {
	cs, home := creditSession(t)
	out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}})
	if !isErr || !strings.Contains(out, "aaaaaaaa: not shown in this session") || !strings.Contains(out, "call again after the result") {
		t.Errorf("%v %s", isErr, out)
	}
	showIn(t, home, "S", creditA)
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}}); isErr || !strings.HasPrefix(out, "Credited.") {
		t.Errorf("after the show: %v %s", isErr, out)
	}
}

func TestCreditNoneNeedsNoIDs(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}, "verdict": "none"}); !isErr || !strings.Contains(out, "none") {
		t.Errorf("none with ids: %v %s", isErr, out)
	}
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"verdict": "none"}); isErr || out != "Noted: no memory used." {
		t.Errorf("none: %v %q", isErr, out)
	}
	if out, isErr := call(t, cs, "memory_credit", map[string]any{}); !isErr || !strings.Contains(out, "ids") {
		t.Errorf("used without ids: %v %s", isErr, out)
	}
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = creditA
	}
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": eleven}); !isErr || !strings.Contains(out, "1 to 10") {
		t.Errorf("eleven ids: %v %s", isErr, out)
	}
	for _, v := range []string{"wrong", "outdated"} {
		if out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}, "verdict": v}); !isErr || !strings.Contains(out, "correction") {
			t.Errorf("%s without a correction: %v %s", v, isErr, out)
		}
	}
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"verdict": "maybe"}); !isErr || !strings.Contains(out, `"used", "none", "wrong" or "outdated"`) {
		t.Errorf("unknown verdict: %v %s", isErr, out)
	}
}

// Without its session the server cannot tell which shows count.
func TestCreditWithoutSessionAsksToRetry(t *testing.T) {
	mcpParentPID = 4949
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSession(t)
	showIn(t, os.Getenv("HOME"), "S", creditA)
	for _, args := range []map[string]any{{"ids": []string{"aaaaaaaa"}}, {"verdict": "none"}} {
		if out, isErr := call(t, cs, "memory_credit", args); !isErr || out != "memory session not ready: call memory_credit again" {
			t.Errorf("%v: %v %q", args, isErr, out)
		}
	}
}

// An unreadable journal does not fail the call: the session state's seen
// ids stand in for the shows.
func TestCreditFallsBackToTheSessionStateWhenTheJournalIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without permissions")
	}
	cs, home := creditSession(t)
	showIn(t, home, "S", creditB)
	files, _ := filepath.Glob(filepath.Join(pathsOf(home).FeedbackDir(), "journal-*.jsonl"))
	if len(files) != 1 || os.Chmod(files[0], 0) != nil {
		t.Fatalf("journal %v", files)
	}
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o600) })
	_ = sessions(home).Update("S", func(st *session.State) error { st.Seen = []string{creditA}; return nil })
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}}); isErr || !strings.Contains(out, "From memory (proj · aaaaaaaa)") {
		t.Errorf("%v %s", isErr, out)
	}
}

// memory_read takes the short id of a memory shown in this session, as
// memory_credit does; a full id needs no show.
func TestMCPReadTakesAShortIDShownInTheSession(t *testing.T) {
	cs, home := creditSession(t)
	memories[creditA] = `{"id":"` + creditA + `","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"]}`
	memories[creditB] = `{"id":"` + creditB + `","text":"Deploys go through staging.","type":"observation","tags":["project:billing"]}`
	showIn(t, home, "S", creditA)
	if out, isErr := call(t, cs, "memory_read", map[string]any{"id": "aaaaaaaa"}); isErr || !strings.Contains(out, "PostgreSQL 16 stays.") {
		t.Errorf("short id: %v %s", isErr, out)
	}
	if out, isErr := call(t, cs, "memory_read", map[string]any{"id": creditB}); isErr || !strings.Contains(out, "Deploys go through staging.") {
		t.Errorf("full id without a show: %v %s", isErr, out)
	}
	// shown in another session only: not this session's short id
	showIn(t, home, "S2", creditB)
	if out, isErr := call(t, cs, "memory_read", map[string]any{"id": "bbbbbbbb"}); !isErr || !strings.Contains(out, "no memory bbbbbbbb") {
		t.Errorf("another session's short id: %v %s", isErr, out)
	}
	showIn(t, home, "S", "aaaaaaaa-9999-4000-8000-000000000009")
	if out, isErr := call(t, cs, "memory_read", map[string]any{"id": "aaaaaaaa"}); !isErr || !strings.Contains(out, "aaaaaaaa: matches several memories") {
		t.Errorf("ambiguous short id: %v %s", isErr, out)
	}
}

// verdict is a memory_credit call of verdict on ids, with a correction
// and evidence that pass every check.
func verdict(v string, ids ...string) map[string]any {
	return map[string]any{"ids": ids, "verdict": v, "correction": "Billing runs PostgreSQL 16 since 2026-10-20.", "evidence": "psql --version printed 16.4"}
}

// retireIn journals what Task 2's retirements write: one event of kind
// per id, in session sess, in an earlier turn.
func retireIn(t *testing.T, home, sess, kind string, ids ...string) {
	t.Helper()
	j := feedback.NewJournal(pathsOf(home).FeedbackDir(), feedback.Common{Session: sess, Project: "proj"})
	for _, id := range ids {
		if err := j.Append(feedback.Event{Kind: kind, Turn: "p-0", IDs: []string{id}, Verdict: "wrong", Outcome: "invalidated"}); err != nil {
			t.Fatal(err)
		}
	}
}

// nothingStored: no note reached the server since before, none waits in
// the outbox.
func nothingStored(t *testing.T, home string, before int64) bool {
	t.Helper()
	n, _ := notesOutbox(pathsOf(home)).Pending()
	return retained.Load() == before && n == 0
}

var correctionStoredRE = regexp.MustCompile(`^Correction stored: note:[0-9a-f-]{36}\.$`)

func TestVerdictNeedsCorrectionAndEvidence(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	before := retained.Load()
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"no correction":    {map[string]any{"ids": []string{"aaaaaaaa"}, "verdict": "wrong", "evidence": "the user said so"}, "correction"},
		"blank correction": {map[string]any{"ids": []string{"aaaaaaaa"}, "verdict": "wrong", "correction": "  ", "evidence": "the user said so"}, "correction"},
		"no evidence":      {map[string]any{"ids": []string{"aaaaaaaa"}, "verdict": "outdated", "correction": "Billing runs PostgreSQL 16."}, "evidence"},
		"long evidence":    {map[string]any{"ids": []string{"aaaaaaaa"}, "verdict": "outdated", "correction": "Billing runs PostgreSQL 16.", "evidence": strings.Repeat("é", 301)}, "300"},
		"no ids":           {map[string]any{"verdict": "wrong", "correction": "Billing runs PostgreSQL 16.", "evidence": "the user said so"}, "ids"},
	} {
		if out, isErr := call(t, cs, "memory_credit", c.args); !isErr || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v %s", name, isErr, out)
		}
	}
	if !nothingStored(t, home, before) {
		t.Errorf("a refused verdict stored a note: %d sent", retained.Load()-before)
	}
	// 300 characters is the limit, not over it
	args := verdict("outdated", "aaaaaaaa")
	args["evidence"] = strings.Repeat("é", 300)
	if out, isErr := call(t, cs, "memory_credit", args); isErr || !correctionStoredRE.MatchString(out) {
		t.Errorf("300 characters: %v %s", isErr, out)
	}
}

func TestVerdictRefusesUnshownAndForeignMemories(t *testing.T) {
	cs, home := creditSession(t)
	cfg, _, _ := loadServerConfig()
	alias := cfg.Projects.Transverse[0]
	_ = os.MkdirAll(filepath.Join(home, "proj"), 0o700)
	_ = os.WriteFile(filepath.Join(home, "proj", ".backsight.toml"), []byte(`related = ["atlas-spec"]`+"\n"), 0o600)
	const (
		foreign = "cccccccc-0000-4000-8000-000000000003"
		linked  = "dddddddd-0000-4000-8000-000000000004"
		pool    = "eeeeeeee-0000-4000-8000-000000000005"
	)
	showItems(t, home, "S", "search", "proj", feedback.Item{ID: creditA, Rank: 1, Project: "proj"}, feedback.Item{ID: foreign, Rank: 2, Project: "other"},
		feedback.Item{ID: linked, Rank: 3, Project: "atlas-spec"}, feedback.Item{ID: pool, Rank: 4, Project: alias})
	before := retained.Load()
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "ffffffff")); !isErr || !strings.Contains(out, "ffffffff: not shown in this session") {
		t.Errorf("unshown: %v %s", isErr, out)
	}
	// all or nothing: a foreign id refuses the call, the allowed one with it
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "cccccccc"))
	if !isErr || !strings.Contains(out, "cccccccc") || !strings.Contains(out, "other") || !strings.Contains(out, "backsight forget "+foreign) {
		t.Errorf("foreign: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Fatalf("a refused verdict stored a note: %d sent", retained.Load()-before)
	}
	// a linked project's memory and the session's own: the session's project
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "dddddddd", "aaaaaaaa")); isErr || !correctionStoredRE.MatchString(out) {
		t.Errorf("linked: %v %s", isErr, out)
	} else if last := retainedTags[len(retainedTags)-1]; !strings.Contains(last, "project:proj,") {
		t.Errorf("linked: note tags %q", last)
	}
	// only transverse memories: the note goes to the pool
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "eeeeeeee")); isErr || !correctionStoredRE.MatchString(out) {
		t.Errorf("transverse: %v %s", isErr, out)
	} else if last := retainedTags[len(retainedTags)-1]; !strings.Contains(last, "project:"+alias+",") {
		t.Errorf("transverse: note tags %q", last)
	}
	// mixed projects: the session's
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "eeeeeeee", "aaaaaaaa")); isErr || !correctionStoredRE.MatchString(out) {
		t.Errorf("mixed: %v %s", isErr, out)
	} else if last := retainedTags[len(retainedTags)-1]; !strings.Contains(last, "project:proj,") {
		t.Errorf("mixed: note tags %q", last)
	}
}

func TestRetirementsAreCappedPerSession(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA, creditB)
	retireIn(t, home, "S2", feedback.KindRetire, "11111111-0000-4000-8000-000000000001", "22222222-0000-4000-8000-000000000002", "33333333-0000-4000-8000-000000000003")
	retireIn(t, home, "S", feedback.KindRetire, "44444444-0000-4000-8000-000000000004")
	retireIn(t, home, "S", feedback.KindHide, "55555555-0000-4000-8000-000000000005")
	before := retained.Load()
	// 2 earlier + 2 asked > 3: refused, whatever another session retired
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "bbbbbbbb")); !isErr || !strings.Contains(out, "backsight forget") || !strings.Contains(out, "3") {
		t.Errorf("over the cap: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Fatalf("a refused verdict stored a note")
	}
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa")); isErr || !correctionStoredRE.MatchString(out) {
		t.Errorf("the third: %v %s", isErr, out)
	}
	retireIn(t, home, "S", feedback.KindRetire, creditA)
	before = retained.Load()
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "bbbbbbbb")); !isErr || !strings.Contains(out, "backsight forget "+creditB) {
		t.Errorf("the fourth: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Error("a refused verdict stored a note")
	}
}

// max_retirements = 0: no verdict retires anything in a session.
func TestRetirementsCapOfZeroRefusesEveryVerdict(t *testing.T) {
	mcpParentPID = 4848
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSessionWith(t, "\n[feedback]\nmax_retirements = 0\n")
	home := os.Getenv("HOME")
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4848, session.Live{SessionID: "S", Cwd: filepath.Join(home, "proj"), Project: "proj", At: time.Now()})
	showIn(t, home, "S", creditA)
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa")); !isErr || !strings.Contains(out, "backsight forget") {
		t.Errorf("%v %s", isErr, out)
	}
}

func TestCorrectionNoteCarriesReplacesAndEvidence(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA, creditB)
	args := verdict("wrong", "aaaaaaaa", creditB)
	out, isErr := call(t, cs, "memory_credit", args)
	if isErr || !correctionStoredRE.MatchString(out) {
		t.Fatalf("%v %s", isErr, out)
	}
	doc := strings.TrimSuffix(strings.TrimPrefix(out, "Correction stored: "), ".")
	cfg, sec, _ := loadServerConfig()
	cl, _ := hindsight.New(cfg.Server.URL, sec.APIKey, 5*time.Second)
	d, err := cl.GetDocument(context.Background(), cfg.Server.Bank, doc)
	if err != nil {
		t.Fatal(err)
	}
	if d.Metadata["replaces"] != creditA+","+creditB || d.Metadata["evidence"] != args["evidence"] || d.Metadata["kind"] != "correction" || d.Metadata["session"] != "S" {
		t.Errorf("metadata %v", d.Metadata)
	}
	if d.OriginalText != args["correction"] || !slices.Contains(d.Tags, "kind:correction") || !slices.Contains(d.Tags, "project:proj") {
		t.Errorf("document %+v", d)
	}
	// the correction and the evidence live in the note only
	journal := ""
	files, _ := filepath.Glob(filepath.Join(pathsOf(home).FeedbackDir(), "*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		journal += string(b)
	}
	for _, text := range []string{"PostgreSQL 16", "psql"} {
		if strings.Contains(journal, text) || strings.Contains(telemetryText(t, home), text) {
			t.Errorf("%q left the note", text)
		}
	}
	if !creditEvent(t, home, `"verdict":"wrong"`, `"ids_n":2`, `"ok":true`) {
		t.Errorf("telemetry:\n%s", telemetryText(t, home))
	}
}

func TestVerdictCorrectionHonoursSecretsAndPause(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	before := retained.Load()
	secret := "DB_PASSWORD=" + "Zx9kLmQ2" + "wPq8"
	args := verdict("wrong", "aaaaaaaa")
	args["correction"] = "Use " + secret + " for prod."
	if out, isErr := call(t, cs, "memory_credit", args); !isErr || !strings.Contains(out, "secret") {
		t.Errorf("secret in the correction: %v %s", isErr, out)
	}
	args = verdict("wrong", "aaaaaaaa")
	args["evidence"] = "the .env holds " + secret
	if out, isErr := call(t, cs, "memory_credit", args); !isErr || !strings.Contains(out, "secret") {
		t.Errorf("secret in the evidence: %v %s", isErr, out)
	}
	_ = sessions(home).Update("S", func(st *session.State) error { st.Paused = true; return nil })
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "aaaaaaaa")); !isErr || !strings.Contains(out, "paused") {
		t.Errorf("paused: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Errorf("a refused verdict stored a note: %d sent", retained.Load()-before)
	}
}
