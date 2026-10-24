package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/telemetry"
)

// mcpSession starts the memory server on a fake bank and connects a client.
func mcpSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	cfg, sec, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := pathsOf(home)
	eng, err := newEngine(cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	srv := newMCPServer(mcpDeps{dirs: d, home: home, cfg: cfg, sec: sec, engine: eng, current: "billing", tw: telemetry.New(d.State, telemetry.Common{})})
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
