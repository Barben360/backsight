package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
	"github.com/Barben360/backsight/internal/testtool"
)

// mcpSession starts the memory server on a fake bank and connects a client.
func mcpSession(t *testing.T) *mcp.ClientSession { t.Helper(); return mcpSessionWith(t, "") }

// mcpParentPID and mcpParentStart are the Claude Code pid the next
// mcpSession believes it has, and when that process started.
var (
	mcpParentPID   int
	mcpParentStart time.Time
	mcpSessionID   string // what Claude Code gives it as CLAUDE_CODE_SESSION_ID
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
	return mcpConnect(t, home)
}

// mcpConnect starts the memory server of home, the machine the
// environment points at, and connects a client.
func mcpConnect(t *testing.T, home string) *mcp.ClientSession {
	t.Helper()
	cfg, sec, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := pathsOf(home)
	eng, err := newEngine(cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	srv := newMCPServer(mcpDeps{dirs: d, home: home, cfg: cfg, sec: sec, engine: eng, current: "billing", tw: telemetry.New(d.State, telemetry.Common{}), parentPID: mcpParentPID, parentStart: mcpParentStart, sessionID: mcpSessionID})
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
	// the lines to print: Updated, since a similar memory is listed, or Saved
	for _, want := range []string{"in your final message", "\n> 🧠💾 **Saved (billing · correction)** — <the fact, in a few words>\n",
		"\n> 🧠🔄 **Updated (billing · correction · replaces <id8 of the memory it updates>)** — <the fact, in a few words>\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("store answer lacks %q:\n%s", want, out)
		}
	}
}

// No similar memory: only the Saved line, in the project the fact went to.
func TestMCPStoreWithoutSimilarMemoriesHandsBackTheSavedLine(t *testing.T) {
	cs := mcpSession(t)
	recallAnswer = `[]`
	out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "convention", "text": "Commits are squash-merged.", "project": "transverse"})
	if isErr || !strings.Contains(out, "> 🧠💾 **Saved (") || !strings.Contains(out, " · convention)** — ") || strings.Contains(out, "Updated") || strings.Contains(out, "(billing · ") {
		t.Errorf("%v\n%s", isErr, out)
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
	_ = os.WriteFile(cfg, append(raw, []byte("\n[privacy]\nexclude_paths = ["+testtool.TOMLStr(dir)+"]\n")...), 0o600)
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
	return creditSessionWith(t, "")
}

// creditSessionWith is creditSession with extra TOML in the config.
func creditSessionWith(t *testing.T, extra string) (*mcp.ClientSession, string) {
	t.Helper()
	mcpParentPID = 4848
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSessionWith(t, extra)
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
	if len(files) != 1 {
		t.Fatalf("journal %v", files)
	}
	testtool.DenyRead(t, files[0])
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

// retiredRE: a verdict that retired everything it named, its correction
// stored.
var retiredRE = regexp.MustCompile(`(?s)^Retired\. .*note:[0-9a-f-]{36}`)

func TestVerdictNeedsCorrectionAndEvidence(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	rawFact(creditA)
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
	if out, isErr := call(t, cs, "memory_credit", args); isErr || !retiredRE.MatchString(out) {
		t.Errorf("300 characters: %v %s", isErr, out)
	}
}

func TestVerdictRefusesUnshownAndForeignMemories(t *testing.T) {
	cs, home := creditSessionWith(t, "\n[feedback]\nmax_retirements = 10\n") // five retirements below
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
	rawFact(creditA, foreign, linked, pool)
	before := retained.Load()
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "ffffffff")); !isErr || !strings.Contains(out, "ffffffff: not shown in this session") {
		t.Errorf("unshown: %v %s", isErr, out)
	}
	// all or nothing: a foreign id refuses the call, the allowed one with it
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "cccccccc"))
	if !isErr || !strings.Contains(out, "> 🧠⚠️ **Not retired (other · cccccccc)** — ") || !strings.Contains(out, "retire it: `backsight forget "+foreign+"`") ||
		strings.Contains(out, "(proj · aaaaaaaa)") {
		t.Errorf("foreign: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Fatalf("a refused verdict stored a note: %d sent", retained.Load()-before)
	}
	// a linked project's memory and the session's own: the session's project
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "dddddddd", "aaaaaaaa")); isErr || !retiredRE.MatchString(out) {
		t.Errorf("linked: %v %s", isErr, out)
	} else if last := retainedTags[len(retainedTags)-1]; !strings.Contains(last, "project:proj,") {
		t.Errorf("linked: note tags %q", last)
	}
	// only transverse memories: the note goes to the pool
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "eeeeeeee")); isErr || !retiredRE.MatchString(out) {
		t.Errorf("transverse: %v %s", isErr, out)
	} else if last := retainedTags[len(retainedTags)-1]; !strings.Contains(last, "project:"+alias+",") {
		t.Errorf("transverse: note tags %q", last)
	}
	// mixed projects: the session's
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "eeeeeeee", "aaaaaaaa")); isErr || !retiredRE.MatchString(out) {
		t.Errorf("mixed: %v %s", isErr, out)
	} else if last := retainedTags[len(retainedTags)-1]; !strings.Contains(last, "project:proj,") {
		t.Errorf("mixed: note tags %q", last)
	}
}

func TestRetirementsAreCappedPerSession(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA, creditB)
	rawFact(creditA, creditB)
	retireIn(t, home, "S2", feedback.KindRetire, "11111111-0000-4000-8000-000000000001", "22222222-0000-4000-8000-000000000002", "33333333-0000-4000-8000-000000000003")
	retireIn(t, home, "S", feedback.KindRetire, "44444444-0000-4000-8000-000000000004")
	retireIn(t, home, "S", feedback.KindHide, "55555555-0000-4000-8000-000000000005")
	before := retained.Load()
	// 2 earlier + 2 asked > 3: refused, whatever another session retired
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "bbbbbbbb")); !isErr || !strings.Contains(out, "at most 3 per session (feedback.max_retirements)") ||
		!strings.Contains(out, "\n> 🧠⚠️ **Not retired (proj · aaaaaaaa)** — ") || !strings.Contains(out, "\n> 🧠⚠️ **Not retired (proj · bbbbbbbb)** — ") ||
		!strings.Contains(out, "retire it: `backsight forget "+creditB+"`") {
		t.Errorf("over the cap: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Fatalf("a refused verdict stored a note")
	}
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa")); isErr || !retiredRE.MatchString(out) {
		t.Errorf("the third: %v %s", isErr, out)
	}
	// the server journaled the third itself
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
	rawFact(creditA, creditB)
	args := verdict("wrong", "aaaaaaaa", creditB)
	out, isErr := call(t, cs, "memory_credit", args)
	if isErr || !retiredRE.MatchString(out) {
		t.Fatalf("%v %s", isErr, out)
	}
	doc := noteIDRE.FindString(out)
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
	// an opt-out taken during the session: the directory excluded
	appendConfig(t, home, "[privacy]\nexclude_paths = ["+testtool.TOMLStr(filepath.Join(home, "proj"))+"]")
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "aaaaaaaa")); !isErr || !strings.Contains(out, "excluded") {
		t.Errorf("opted out: %v %s", isErr, out)
	}
	_ = sessions(home).Update("S", func(st *session.State) error { st.Paused = true; return nil })
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "aaaaaaaa")); !isErr || !strings.Contains(out, "paused") {
		t.Errorf("paused: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Errorf("a refused verdict stored a note: %d sent", retained.Load()-before)
	}
}

// A verdict that passes every check runs the note checks once: before
// the server is asked, not again when the correction is stored.
func TestVerdictChecksNoteOnce(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	rawFact(creditA)
	checks := 0
	noteChecked = func() { checks++ }
	t.Cleanup(func() { noteChecked = nil })
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa")); isErr || !retiredRE.MatchString(out) {
		t.Fatalf("%v %s", isErr, out)
	}
	if checks != 1 {
		t.Errorf("the note checks ran %d times, want 1", checks)
	}
}

// While another server answers at the joined address, a verdict is
// refused as forget is: no note stored or queued, nothing invalidated or
// hidden, nothing journaled. Once the joined server answers again, the
// same verdict goes through.
func TestVerdictRefusedOnAnotherServer(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recordMismatch(t, home)
	mcpParentPID = 4848
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpConnect(t, home)
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4848, session.Live{SessionID: "S", Cwd: filepath.Join(home, "proj"), Project: "proj", At: time.Now()})
	showIn(t, home, "S", creditA, creditB)
	rawFact(creditA) // creditB unknown to the server: it would be hidden
	before := retained.Load()
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "bbbbbbbb"))
	if !isErr || !strings.Contains(out, "a different server answers at this address (server b, joined a)") || !strings.Contains(out, "nothing was changed") {
		t.Errorf("%v %s", isErr, out)
	}
	if len(patches()) != 0 || !nothingStored(t, home, before) || len(retireEvents(t, home, feedback.KindRetire))+len(retireEvents(t, home, feedback.KindHide)) != 0 {
		t.Errorf("a verdict acted on another server: %q", curation)
	}
	if got := retireTelemetry(t, home); len(got) != 0 {
		t.Errorf("retire telemetry %q", got)
	}
	setMismatch(t, home, "")
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "bbbbbbbb")); isErr || !strings.HasPrefix(out, "Retired.") {
		t.Errorf("the joined server: %v %s", isErr, out)
	}
}

// rawFact makes each id a world fact of project proj on the fake server.
func rawFact(ids ...string) {
	for _, id := range ids {
		memories[id] = `{"id":"` + id + `","text":"Billing runs PostgreSQL 14.","type":"world","tags":["project:proj"]}`
	}
}

// observation makes id an observation of project proj drawn from sources
// ("<id>|<project>|<text>"), each a world fact on the fake server too.
func observation(id string, sources ...string) {
	var list []string
	for _, s := range sources {
		f := strings.SplitN(s, "|", 3)
		fact := `{"id":"` + f[0] + `","text":` + strconv.Quote(f[2]) + `,"type":"world","tags":["project:` + f[1] + `"]}`
		memories[f[0]] = fact
		list = append(list, fact)
	}
	memories[id] = `{"id":"` + id + `","text":"Billing runs PostgreSQL 14 everywhere.","type":"observation","tags":["project:proj"],"source_memories":[` + strings.Join(list, ",") + `]}`
}

// patches are the PATCH calls the fake server accepted on memories.
func patches() []string {
	var out []string
	for _, c := range curation {
		if strings.HasPrefix(c, "PATCH ") && strings.Contains(c, "/memories/") {
			out = append(out, c)
		}
	}
	return out
}

// retireEvents are the journal's events of kind in session S.
func retireEvents(t *testing.T, home, kind string) []feedback.Event {
	t.Helper()
	var out []feedback.Event
	for _, e := range eventsOf(journalEvents(t, home), kind) {
		if e.Session == "S" {
			out = append(out, e)
		}
	}
	return out
}

const (
	wantRestore = " in your final message:\n> 🧠"
	creditC     = "cccccccc-0000-4000-8000-000000000003"
	obsO        = "0f0f0f0f-0000-4000-8000-00000000000f"
	srcOne      = "51515151-0000-4000-8000-000000000051"
	srcTwo      = "52525252-0000-4000-8000-000000000052"
	srcThree    = "53535353-0000-4000-8000-000000000053"
)

var noteIDRE = regexp.MustCompile(`note:[0-9a-f-]{36}`)

func TestWrongRawFactIsInvalidated(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	rawFact(creditA)
	_ = feedback.SetTurn(pathsOf(home).FeedbackDir(), "S", feedback.Turn{ID: "p-7", N: 7})
	args := verdict("wrong", "aaaaaaaa")
	args["evidence"] = "psql --version\nprinted\x07 16.4"
	args["correction"] = "Billing runs PostgreSQL 16 since 2026-10-20, " + strings.Repeat("on the primary and on every replica of the cluster, ", 8) + "as psql shows."
	out, isErr := call(t, cs, "memory_credit", args)
	note := noteIDRE.FindString(out)
	if isErr || !strings.HasPrefix(out, "Retired.") || !strings.Contains(out, wantRestore) || !strings.Contains(out, creditA) || note == "" {
		t.Fatalf("%v %s", isErr, out)
	}
	if want := "\n> 🧠❌ **Wrong, retired (proj · aaaaaaaa)** — <what was wrong, and what shows it> · undo: `backsight forget --restore " + creditA + "`"; !strings.HasSuffix(out, want) {
		t.Errorf("the line to print, %q, does not end\n%s", want, out)
	}
	p := patches()
	if len(p) != 1 || !strings.Contains(p[0], "/memories/"+creditA+" ") {
		t.Fatalf("patches %q", p)
	}
	var body struct{ State, Reason string }
	_, raw, _ := strings.Cut(p[0], " {")
	if err := json.Unmarshal([]byte("{"+raw), &body); err != nil {
		t.Fatal(err)
	}
	suffix := " (" + note + ")"
	head, cut := strings.CutSuffix(body.Reason, suffix)
	if body.State != "invalidated" || !cut || !strings.HasPrefix(body.Reason, "wrong: psql --version printed 16.4 → Billing runs PostgreSQL 16") ||
		utf8.RuneCountInString(head) > 300 || strings.ContainsAny(body.Reason, "\n\x07") {
		t.Errorf("patch %+v", body)
	}
	if utf8.RuneCountInString(head) < 290 {
		t.Errorf("the reason was cut short: %d characters", utf8.RuneCountInString(head))
	}
	// the evidence is one line in the note too
	cfg, sec, _ := loadServerConfig()
	cl, _ := hindsight.New(cfg.Server.URL, sec.APIKey, 5*time.Second)
	if d, err := cl.GetDocument(context.Background(), cfg.Server.Bank, note); err != nil || d.Metadata["evidence"] != "psql --version printed 16.4" {
		t.Errorf("note %v %v", d.Metadata, err)
	}
	r := retireEvents(t, home, feedback.KindRetire)
	if len(r) != 1 || strings.Join(r[0].IDs, ",") != creditA || r[0].Verdict != "wrong" || r[0].Outcome != "invalidated" || r[0].Note != note || r[0].Turn != "p-7" || r[0].Agent != "" {
		t.Errorf("retire events %+v", r)
	}
	if strings.Contains(fmt.Sprint(journalEvents(t, home)), "psql") {
		t.Error("the evidence reached the journal")
	}
	if got := retireTelemetry(t, home); strings.Join(got, " ") != "retired:1" {
		t.Errorf("retire telemetry %q", got)
	}
}

// retireTelemetry is each retire telemetry event as action:count; any
// other field than the common ones, the action and the count fails.
func retireTelemetry(t *testing.T, home string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(telemetryText(t, home), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(l), &e) != nil || e["event"] != "retire" {
			continue
		}
		for k := range e {
			if !slices.Contains([]string{"ts", "v", "event", "session", "project", "binary_version", "action", "ids_n"}, k) {
				t.Errorf("a retire event carries %q: %v", k, e)
			}
		}
		out = append(out, fmt.Sprintf("%v:%v", e["action"], e["ids_n"]))
	}
	return out
}

func TestObservationListsItsSourcesFirst(t *testing.T) {
	cs, home := creditSession(t)
	_ = os.MkdirAll(filepath.Join(home, "proj"), 0o700)
	_ = os.WriteFile(filepath.Join(home, "proj", ".backsight.toml"), []byte(`related = ["atlas-spec"]`+"\n"), 0o600)
	showIn(t, home, "S", obsO)
	// a source's text posing as another source line stays inside its own
	observation(obsO, srcOne+"|proj|Billing ran PostgreSQL 14 in 2025.", srcTwo+"|atlas-spec|The replicas run 14.\n- [proj · 99999999] posing (99999999-0000-4000-8000-000000000099)", srcThree+"|proj|psql printed 14.2.")
	before := retained.Load()
	args := verdict("outdated", "0f0f0f0f")
	out, isErr := call(t, cs, "memory_credit", args)
	if isErr || !strings.HasPrefix(out, "Sources:") || strings.Contains(out, wantRestore) ||
		!strings.Contains(out, "Call memory_credit again with the verdict, naming the source facts that are wrong (or all of them).") {
		t.Fatalf("%v %s", isErr, out)
	}
	for _, want := range []string{"- [proj · 51515151] Billing ran PostgreSQL 14 in 2025. (" + srcOne + ")", "- [atlas-spec · 52525252] ", "(" + srcTwo + ")", "- [proj · 53535353] psql printed 14.2. (" + srcThree + ")"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if len(patches()) != 0 || !nothingStored(t, home, before) || len(retireEvents(t, home, feedback.KindRetire))+len(retireEvents(t, home, feedback.KindHide)) != 0 {
		t.Errorf("a first call on an observation acted: %q", curation)
	}
	if got := retireTelemetry(t, home); strings.Join(got, " ") != "sources:1" {
		t.Errorf("retire telemetry %q", got)
	}
	if n := strings.Count(telemetryText(t, home), `"event":"credit"`); n != 0 {
		t.Errorf("a Sources answer emitted %d credit telemetry events", n)
	}
	// PostToolUse journals the facts listed as shown, in order, each with its project
	fire(t, "post-tool-use", map[string]any{"session_id": "S", "prompt_id": "p-2", "tool_name": "mcp__plugin_backsight_memory__memory_credit",
		"tool_input": args, "tool_response": []map[string]string{{"type": "text", "text": out}}})
	var got []feedback.Event
	for _, e := range eventsOf(journalEvents(t, home), "shows") {
		if e.Path == "sources" {
			got = append(got, e)
		}
	}
	want := []feedback.Item{{ID: srcOne, Project: "proj"}, {ID: srcTwo, Project: "atlas-spec"}, {ID: srcThree, Project: "proj"}}
	if len(got) != 1 || !slices.Equal(got[0].Items, want) || got[0].Session != "S" {
		t.Errorf("sources shows %+v", got)
	}
	if n := len(eventsOf(journalEvents(t, home), "credit")); n != 0 {
		t.Errorf("a Sources answer journaled %d credits", n)
	}
	// the second call names the facts listed: they resolve
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "52525252")); isErr || !strings.HasPrefix(out, "Retired.") {
		t.Errorf("second call: %v %s", isErr, out)
	}
	// the report counts once in stats: one credit event, the second call's
	if n := strings.Count(telemetryText(t, home), `"event":"credit"`); n != 1 || !creditEvent(t, home, `"verdict":"outdated"`, `"ok":true`) {
		t.Errorf("%d credit telemetry events for one report:\n%s", n, telemetryText(t, home))
	}
}

func TestNamedSourcesAreRetired(t *testing.T) {
	cs, home := creditSession(t)
	observation(obsO, srcOne+"|proj|one", srcTwo+"|proj|two", srcThree+"|proj|three")
	showIn(t, home, "S", obsO)
	showItems(t, home, "S", "sources", "proj", feedback.Item{ID: srcOne, Project: "proj"}, feedback.Item{ID: srcTwo, Project: "proj"}, feedback.Item{ID: srcThree, Project: "proj"})
	// 1 earlier + the 2 facts named = 3: the observation itself does not count
	retireIn(t, home, "S", feedback.KindRetire, "44444444-0000-4000-8000-000000000004")
	before := retained.Load()
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "0f0f0f0f", "51515151", srcTwo))
	if isErr || !strings.HasPrefix(out, "Retired.") || !strings.Contains(out, srcOne) || !strings.Contains(out, srcTwo) || strings.Contains(out, srcThree) {
		t.Fatalf("%v %s", isErr, out)
	}
	p := patches()
	if len(p) != 2 || !strings.Contains(p[0], "/memories/"+srcOne+" ") || !strings.Contains(p[1], "/memories/"+srcTwo+" ") {
		t.Errorf("patches %q", p)
	}
	r := retireEvents(t, home, feedback.KindRetire)
	if len(r) != 3 || strings.Join(r[1].IDs, ",") != srcOne || strings.Join(r[2].IDs, ",") != srcTwo || len(retireEvents(t, home, feedback.KindHide)) != 0 {
		t.Errorf("retire events %+v", r)
	}
	if retained.Load()-before != 1 {
		t.Errorf("%d notes sent", retained.Load()-before)
	}
	if got := retireTelemetry(t, home); strings.Join(got, " ") != "retired:2" {
		t.Errorf("retire telemetry %q", got)
	}
}

func TestObservationWithoutSourcesIsHidden(t *testing.T) {
	cs, home := creditSession(t)
	const empty = "0e0e0e0e-0000-4000-8000-00000000000e"
	observation(obsO, srcOne+"|proj|one")
	memoryStatus["GET "+obsO] = 500 // its sources cannot be listed
	memories[empty] = `{"id":"` + empty + `","text":"x","type":"observation","tags":["project:proj"],"source_memories":[]}`
	showIn(t, home, "S", obsO, empty)
	out, isErr := call(t, cs, "memory_credit", verdict("outdated", "0f0f0f0f", "0e0e0e0e"))
	note := noteIDRE.FindString(out)
	if isErr || !strings.HasPrefix(out, "Hidden.") || !strings.Contains(out, obsO) || !strings.Contains(out, empty) || !strings.Contains(out, wantRestore) || note == "" {
		t.Fatalf("%v %s", isErr, out)
	}
	if len(patches()) != 0 {
		t.Errorf("an observation was invalidated: %q", curation)
	}
	// the one the server could not read says how to retire it there; the
	// one with no sources has nothing to retire on the server
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "> "):
		case strings.Contains(l, obsO) && (!strings.HasPrefix(l, "Hidden") || !strings.Contains(l, "backsight forget "+obsO) || strings.Contains(l, empty)):
			t.Errorf("unread line %q", l)
		case strings.Contains(l, empty) && (!strings.HasPrefix(l, "Hidden") || strings.Contains(l, "backsight forget")):
			t.Errorf("no-sources line %q", l)
		}
	}
	for _, want := range []string{
		"\n> 🧠🙈 **Hidden on this PC (proj · 0f0f0f0f)** — <what changed, and what shows it> · retire it on the server: `backsight forget " + obsO + "` · undo: `backsight forget --restore " + obsO + "`\n",
		"\n> 🧠🙈 **Hidden on this PC (proj · 0e0e0e0e)** — <what changed, and what shows it> · undo: `backsight forget --restore " + empty + "`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	h := retireEvents(t, home, feedback.KindHide)
	if len(h) != 2 || strings.Join(h[0].IDs, ",") != obsO || strings.Join(h[1].IDs, ",") != empty || h[0].Verdict != "outdated" || h[0].Note != note || h[0].Outcome != "" {
		t.Errorf("hide events %+v", h)
	}
	if len(retireEvents(t, home, feedback.KindRetire)) != 0 {
		t.Error("a hidden observation was journaled retired")
	}
	if got := retireTelemetry(t, home); strings.Join(got, " ") != "hidden:2" {
		t.Errorf("retire telemetry %q", got)
	}
}

// A raw fact retired and an observation hidden in one call: Retired.,
// with the hidden one on its own line.
func TestRetiredAndHiddenAnswerSaysWhich(t *testing.T) {
	cs, home := creditSession(t)
	rawFact(creditA)
	memoryStatus["GET "+obsO] = 500
	showIn(t, home, "S", creditA, obsO)
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "0f0f0f0f"))
	if isErr || !strings.HasPrefix(out, "Retired.") {
		t.Fatalf("%v %s", isErr, out)
	}
	var hiddenLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, obsO) && !strings.HasPrefix(l, "> ") {
			hiddenLine = l
		}
	}
	if !strings.Contains(out, "\n> 🧠❌ **Wrong, retired (proj · aaaaaaaa)** — ") || !strings.Contains(out, "\n> 🧠🙈 **Hidden on this PC (proj · 0f0f0f0f)** — ") {
		t.Errorf("lines to print:\n%s", out)
	}
	if !strings.HasPrefix(hiddenLine, "Hidden") || strings.Contains(hiddenLine, creditA) {
		t.Errorf("hidden line %q in\n%s", hiddenLine, out)
	}
	if len(patches()) != 1 || len(retireEvents(t, home, feedback.KindRetire)) != 1 || len(retireEvents(t, home, feedback.KindHide)) != 1 {
		t.Errorf("patches %q", patches())
	}
	if got := retireTelemetry(t, home); strings.Join(got, " ") != "retired:1 hidden:1" {
		t.Errorf("retire telemetry %q", got)
	}
}

func TestPartialRetirementSaysWhatHappened(t *testing.T) {
	cs, home := creditSession(t)
	rawFact(creditA, creditB, creditC)
	memoryStatus["PATCH "+creditB] = 500
	showIn(t, home, "S", creditA, creditB, creditC)
	before := retained.Load()
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "bbbbbbbb", "cccccccc"))
	if isErr || !strings.HasPrefix(out, "Partly retired.") || !strings.Contains(out, wantRestore) {
		t.Fatalf("%v %s", isErr, out)
	}
	var retiredLine, notLine string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "Invalidated"):
			retiredLine = l
		case strings.HasPrefix(l, "Not retired"):
			notLine = l
		}
	}
	if !strings.Contains(retiredLine, creditA) || strings.Contains(retiredLine, creditB) || !strings.Contains(notLine, creditB) || !strings.Contains(notLine, creditC) || strings.Contains(notLine, creditA) {
		t.Errorf("retired %q, not retired %q in\n%s", retiredLine, notLine, out)
	}
	for _, want := range []string{"\n> 🧠❌ **Wrong, retired (proj · aaaaaaaa)** — ",
		"\n> 🧠⚠️ **Not retired (proj · bbbbbbbb)** — the server refused a retirement · retire it: `backsight forget " + creditB + "`",
		"\n> 🧠⚠️ **Not retired (proj · cccccccc)** — the server refused a retirement · retire it: `backsight forget " + creditC + "`"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if p := patches(); len(p) != 1 || !strings.Contains(p[0], creditA) {
		t.Errorf("patches %q", p)
	}
	if r := retireEvents(t, home, feedback.KindRetire); len(r) != 1 || strings.Join(r[0].IDs, ",") != creditA {
		t.Errorf("retire events %+v", r)
	}
	n, _ := notesOutbox(pathsOf(home)).Pending()
	if sent := retained.Load() - before; sent+int64(n) != 1 {
		t.Errorf("notes: %d sent, %d queued", sent, n)
	}
	if got := retireTelemetry(t, home); strings.Join(got, " ") != "partly:1" {
		t.Errorf("retire telemetry %q", got)
	}
}

// The server refuses the first retirement: nothing retired, said so, and
// the correction stored once all the same.
func TestFailedRetirementRetiresNothingSilently(t *testing.T) {
	cs, home := creditSession(t)
	rawFact(creditA)
	memoryStatus["PATCH "+creditA] = 500
	showIn(t, home, "S", creditA)
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa"))
	if !isErr || !strings.Contains(out, "nothing retired") || !strings.Contains(out, "\n> 🧠⚠️ **Not retired (proj · aaaaaaaa)** — the server refused it · retire it: `backsight forget "+creditA+"`") || noteIDRE.FindString(out) == "" ||
		!strings.Contains(out, "do not call memory_credit again for these memories") || strings.Contains(out, "store it again") {
		t.Errorf("%v %s", isErr, out)
	}
	if len(retireEvents(t, home, feedback.KindRetire)) != 0 {
		t.Error("a refused retirement was journaled")
	}
	if got := retireTelemetry(t, home); len(got) != 0 {
		t.Errorf("retire telemetry %q for a retirement the server refused", got)
	}
}

// The whole chain, before any settle: a shown fact reported outdated is
// invalidated, memory stale lists it as reported, and forget --restore
// makes it valid again and drops it from the list.
func TestOutdatedFactIsListedStaleUntilRestored(t *testing.T) {
	cs, home := creditSession(t)
	rawFact(creditA)
	showIn(t, home, "S", creditA)
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "aaaaaaaa")); isErr || !strings.HasPrefix(out, "Retired.") ||
		!strings.Contains(out, "\n> 🧠⌛ **Outdated, retired (proj · aaaaaaaa)** — <what changed, and what shows it> · undo: `backsight forget --restore "+creditA+"`") {
		t.Fatalf("%v %s", isErr, out)
	}
	if p := patches(); len(p) != 1 || !strings.Contains(p[0], "/memories/"+creditA+" ") || !strings.Contains(p[0], `"state":"invalidated"`) {
		t.Fatalf("patches %q", p)
	}
	code, out, errOut := run(t, "memory", "stale")
	if code != output.OK || !strings.Contains(out, "Reported wrong or outdated\naaaaaaaa ") || !strings.Contains(out, "backsight forget "+creditA+" --purge") {
		t.Fatalf("stale after the verdict: exit %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut := run(t, "forget", "--restore", creditA); code != output.OK || !strings.Contains(out, creditA+" restored") {
		t.Fatalf("restore: exit %d\n%s%s", code, out, errOut)
	}
	if p := patches(); len(p) != 2 || !strings.Contains(p[1], `"state":"valid"`) {
		t.Errorf("patches after restore %q", p)
	}
	if code, out, errOut := run(t, "memory", "stale"); code != output.OK || strings.Contains(out, "aaaaaaaa") || !strings.HasPrefix(out, "Nothing needs review") {
		t.Errorf("stale after the restore: exit %d\n%s%s", code, out, errOut)
	}
}

// Maintenance holds the server's writes: nothing asked, stored or retired.
func TestVerdictWaitsForMaintenance(t *testing.T) {
	cs, home := creditSession(t)
	rawFact(creditA)
	showIn(t, home, "S", creditA)
	d := pathsOf(home)
	_ = os.MkdirAll(filepath.Dir(d.MaintenanceLock()), 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"command":"backup create","since":"`+time.Now().UTC().Format(time.RFC3339)+`"}`), 0o600)
	before := retained.Load()
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa")); !isErr || !strings.Contains(out, "maintenance in progress") {
		t.Errorf("%v %s", isErr, out)
	}
	if len(patches()) != 0 || !nothingStored(t, home, before) {
		t.Errorf("written during maintenance: %q", curation)
	}
}

// recallEvents are the recall audit events of the journal.
func recallEvents(t *testing.T, home string) []feedback.Event {
	t.Helper()
	return eventsOf(journalEvents(t, home), feedback.KindRecall)
}

// A memory_search journals what it returned for the session: each hit's
// rank, rank without the boost, server's score and boost, and whether the
// boost changed the order or the set; nothing changed, no flag.
func TestSearchJournalsARecallAuditEvent(t *testing.T) {
	cs, home := creditSession(t)
	if err := feedback.SetTurn(pathsOf(home).FeedbackDir(), "S", feedback.Turn{ID: "p-7", N: 7, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	recallAnswer = `[{"id":"m1","text":"Proj keeps PostgreSQL 16.","type":"observation","tags":["project:proj"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.72}},
		{"id":"m2","text":"Proj moved to Postgres 16.","type":"observation","tags":["project:proj"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.70}}]`
	if _, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}}); isErr {
		t.Fatal("search failed")
	}
	evs := recallEvents(t, home)
	if len(evs) != 1 || evs[0].Session != "S" || evs[0].Turn != "p-7" || evs[0].Path != "search" || len(evs[0].Flags) != 0 ||
		itemsText(evs[0].Items) != "m1:1:0.72,m2:2:0.7" || evs[0].Items[0].RankWithoutBoost != 0 || evs[0].Items[0].Project != "" {
		t.Fatalf("without a ledger: %+v", evs)
	}

	at := time.Now().UTC().Format(time.RFC3339Nano)
	writeLedger(t, home, `{"schema":1,"watermark":"`+at+`","memories":{"m2":{"shown":10,"credits":10,"credits_at":"`+at+`"}}}`)
	out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}})
	if strings.Index(out, "m2") > strings.Index(out, "m1") {
		t.Errorf("m2 not first:\n%s", out)
	}
	ev := recallEvents(t, home)[1]
	m2, m1 := ev.Items[0], ev.Items[1]
	if strings.Join(ev.Flags, ",") != feedback.FlagOrderChanged || m2.ID != "m2" || m2.Rank != 1 || m2.RankWithoutBoost != 2 || m2.Boost <= 0 ||
		m1.ID != "m1" || m1.Rank != 2 || m1.RankWithoutBoost != 1 || m1.Boost != 0 || m2.ScoreWithoutBoost == nil || *m2.ScoreWithoutBoost != 0.75 || m1.ScoreWithoutBoost == nil || *m1.ScoreWithoutBoost != 0.77 {
		t.Errorf("boosted: %+v", ev)
	}
	// One hit returned: m2 ranked past it without the boost.
	call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}, "limit": 1})
	if ev := recallEvents(t, home)[2]; strings.Join(ev.Flags, ",") != feedback.FlagSetChanged+","+feedback.FlagOrderChanged || len(ev.Items) != 1 {
		t.Errorf("limit 1: %+v", ev)
	}
	// A ledger whose boost leaves the order: no flag.
	writeLedger(t, home, `{"schema":1,"watermark":"`+at+`","memories":{"m1":{"shown":10,"credits":10,"credits_at":"`+at+`"}}}`)
	call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}})
	if ev := recallEvents(t, home)[3]; len(ev.Flags) != 0 || ev.Items[0].ID != "m1" || ev.Items[0].RankWithoutBoost != 1 || ev.Items[0].Boost <= 0 {
		t.Errorf("same order: %+v", ev)
	}
	// A hit only the boost lifted over min_score: set_changed, no rank
	// without the boost.
	recallAnswer = `[{"id":"m1","text":"Proj keeps PostgreSQL 16.","type":"observation","tags":["project:proj"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.72}},
		{"id":"m2","text":"Proj moved to Postgres 16.","type":"observation","tags":["project:proj"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.38}}]`
	writeLedger(t, home, `{"schema":1,"watermark":"`+at+`","memories":{"m2":{"shown":10,"credits":10,"credits_at":"`+at+`"}}}`)
	call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}, "min_score": 0.45})
	if ev := recallEvents(t, home)[4]; strings.Join(ev.Flags, ",") != feedback.FlagSetChanged || len(ev.Items) != 2 || ev.Items[1].ID != "m2" || ev.Items[1].RankWithoutBoost != 0 || ev.Items[1].Boost <= 0 {
		t.Errorf("lifted: %+v", ev)
	}
	if raw, _ := os.ReadFile(filepath.Join(pathsOf(home).FeedbackDir(), "journal-"+time.Now().UTC().Format("2006-01")+".jsonl")); strings.Contains(string(raw), "Postgres") {
		t.Errorf("text in the journal:\n%s", raw)
	}
}

// Without its session, or in a paused one, or with feedback off, a search
// journals nothing.
func TestSearchWithoutItsSessionJournalsNoRecall(t *testing.T) {
	answer := `[{"id":"m1","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.72}}]`
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	recallAnswer = answer
	if out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}}); !strings.Contains(out, "m1") {
		t.Fatalf("search:\n%s", out)
	}
	if evs := recallEvents(t, home); len(evs) != 0 {
		t.Errorf("no record: %+v", evs)
	}
	cs, home = creditSession(t)
	recallAnswer = answer
	_ = sessions(home).Update("S", func(st *session.State) error { st.Paused = true; return nil })
	call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}})
	if evs := recallEvents(t, home); len(evs) != 0 {
		t.Errorf("paused: %+v", evs)
	}
	cs, home = creditSessionWith(t, "\n[feedback]\nenabled = false\n")
	recallAnswer = answer
	call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}})
	if evs := recallEvents(t, home); len(evs) != 0 {
		t.Errorf("feedback off: %+v", evs)
	}
}

// A memory server started before connect holds the old server's config
// and key: a verdict is refused before anything is stored or retired
// (the old server is not this PC's any more), with what to do instead.
func TestVerdictRefusedAfterAConnect(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	rawFact(creditA)
	if err := state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil }); err != nil {
		t.Fatal(err)
	}
	before := retained.Load()
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa"))
	if !isErr || !strings.Contains(out, "joined another server since this session started") || !strings.Contains(out, "nothing was changed") ||
		!strings.Contains(out, "restart Claude Code") {
		t.Errorf("%v %s", isErr, out)
	}
	if len(patches()) != 0 || !nothingStored(t, home, before) || len(retireEvents(t, home, feedback.KindRetire))+len(retireEvents(t, home, feedback.KindHide)) != 0 {
		t.Errorf("a verdict acted after a connect: %q", curation)
	}
}

// A connect while a verdict retires: the retirement after it is not
// sent to the old server, and the answer says why and what to run.
func TestRetirementStopsAtAConnect(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA, creditB)
	rawFact(creditA, creditB)
	var once sync.Once
	fakeHook = func(r *http.Request) {
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/memories/") {
			once.Do(func() {
				_ = state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil })
			})
		}
	}
	t.Cleanup(func() { fakeHook = nil })
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "bbbbbbbb"))
	if isErr || !strings.HasPrefix(out, "Partly retired. This PC joined another server since this session started") ||
		!strings.Contains(out, "Not retired (this PC no longer writes to that server): "+creditB) {
		t.Errorf("%v %s", isErr, out)
	}
	if p := patches(); len(p) != 1 || !strings.Contains(p[0], creditA) {
		t.Errorf("patches %q", p)
	}
	if r := retireEvents(t, home, feedback.KindRetire); len(r) != 1 || strings.Join(r[0].IDs, ",") != creditA {
		t.Errorf("retire events %+v", r)
	}
}

// A refused key is followed in secrets.toml only for the server this
// memory server loaded: once config.toml names another address, or a
// connect raised the sends generation, the key secrets.toml holds now is
// never sent to the old address, and the call is not tried again.
func TestMCPNeverSendsANewKeyToTheOldServer(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, home string){
		"another address": func(t *testing.T, home string) { pointAt(t, home, closedPort(t)) },
		"a connect": func(t *testing.T, home string) {
			if err := state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil }); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cs := mcpSession(t)
			home := os.Getenv("HOME")
			memories["m1"] = `{"id":"m1","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"]}`
			change(t, home)
			rotateTo(t, home)
			fresh, _ := config.LoadSecrets(secretsFile(home))
			var mu sync.Mutex
			var keys []string
			fakeHook = func(r *http.Request) {
				mu.Lock()
				keys = append(keys, r.Header.Get("Authorization"))
				mu.Unlock()
			}
			t.Cleanup(func() { fakeHook = nil })
			refusedCalls.Store(0)
			if out, isErr := call(t, cs, "memory_read", map[string]any{"id": "m1"}); !isErr || refusedCalls.Load() != 1 {
				t.Errorf("%d calls, %v %s", refusedCalls.Load(), isErr, out)
			}
			mu.Lock()
			defer mu.Unlock()
			if slices.Contains(keys, "Bearer "+fresh.APIKey) {
				t.Error("the key secrets.toml holds now reached the old server")
			}
		})
	}
}

// A connect between refresh's reads of config.toml and secrets.toml: a
// same-server reconnect that moved the address (no generation raised),
// or one that raised the generation. The key read after it is never sent
// to the address loaded before it.
func TestMCPRefreshRereadsAfterTheKey(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, home string){
		"address moved": func(t *testing.T, home string) { pointAt(t, home, closedPort(t)) },
		"generation raised": func(t *testing.T, home string) {
			if err := state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil }); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cs := mcpSession(t)
			home := os.Getenv("HOME")
			memories["m1"] = `{"id":"m1","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"]}`
			rotateTo(t, home)
			fresh, _ := config.LoadSecrets(secretsFile(home))
			old := mcpKeyBetweenReads
			mcpKeyBetweenReads = func() { change(t, home) }
			t.Cleanup(func() { mcpKeyBetweenReads = old })
			var mu sync.Mutex
			var keys []string
			fakeHook = func(r *http.Request) {
				mu.Lock()
				keys = append(keys, r.Header.Get("Authorization"))
				mu.Unlock()
			}
			t.Cleanup(func() { fakeHook = nil })
			refusedCalls.Store(0)
			if out, isErr := call(t, cs, "memory_read", map[string]any{"id": "m1"}); !isErr || refusedCalls.Load() != 1 {
				t.Errorf("%d calls, %v %s", refusedCalls.Load(), isErr, out)
			}
			mu.Lock()
			defer mu.Unlock()
			if slices.Contains(keys, "Bearer "+fresh.APIKey) {
				t.Error("the key read after the connect reached the old address")
			}
		})
	}
}

// state.json unreadable while a verdict retires: the retirement after it
// is not sent, and the answer says why, never that the server refused.
func TestRetirementStopsAtAnUnreadableGeneration(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA, creditB)
	rawFact(creditA, creditB)
	var once sync.Once
	fakeHook = func(r *http.Request) {
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/memories/") {
			once.Do(func() { _ = os.WriteFile(pathsOf(home).StateFile(), []byte("{"), 0o600) })
		}
	}
	t.Cleanup(func() { fakeHook = nil })
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa", "bbbbbbbb"))
	if isErr || !strings.HasPrefix(out, "Partly retired. The rest was not sent: cannot read this PC's sends generation") || strings.Contains(out, "refused") {
		t.Errorf("%v %s", isErr, out)
	}
	if p := patches(); len(p) != 1 || !strings.Contains(p[0], creditA) {
		t.Errorf("patches %q", p)
	}
}
