package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
)

func codexClient(t *testing.T) (*mcp.ClientSession, string) {
	t.Helper()
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recallAnswer = fmt.Sprintf(`[{"id":%q,"text":"Fact for billing.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.9}}]`, creditA)
	memories[creditA] = fmt.Sprintf(`{"id":%q,"text":"Fact for billing.","type":"observation","tags":["project:billing"]}`, creditA)
	return mcpConnectMode(t, home, true), home
}

func codexMeta(thread, turn, callID string) mcp.Meta {
	return mcp.Meta{"threadId": thread, "callId": callID, "x-codex-turn-metadata": map[string]any{"turn_id": turn, "thread_id": thread}}
}

func codexCall(t *testing.T, cs *mcp.ClientSession, meta mcp.Meta, name string, args map[string]any) (string, bool) {
	t.Helper()
	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: meta, Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, c := range r.Content {
		if c, ok := c.(*mcp.TextContent); ok {
			out.WriteString(c.Text)
		}
	}
	return out.String(), r.IsError
}

func codexEvents(t *testing.T, home string) []feedback.Event {
	t.Helper()
	e, _, err := feedback.Read(pathsOf(home).FeedbackDir(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestCodexCreditsAreJournaledAndIsolated(t *testing.T) {
	cs, home := codexClient(t)
	search := map[string]any{"queries": []string{"billing decision"}}
	if out, bad := codexCall(t, cs, codexMeta("thread-a", "turn-1", "search-1"), "memory_search", search); bad {
		t.Fatal(out)
	}
	credit := map[string]any{"ids": []string{creditA[:8]}}
	if out, bad := codexCall(t, cs, codexMeta("thread-b", "turn-1", "credit-1"), "memory_credit", credit); !bad {
		t.Fatalf("foreign credit accepted: %s", out)
	}
	for i := 0; i < 2; i++ {
		if out, bad := codexCall(t, cs, codexMeta("thread-a", "turn-1", "credit-1"), "memory_credit", credit); bad || !strings.Contains(out, creditA[:8]) {
			t.Fatalf("%v %s", bad, out)
		}
	}
	var shows, credits int
	for _, e := range codexEvents(t, home) {
		if e.Kind == "shows" {
			shows++
			if e.Session != "codex-thread-a" || e.Turn != "turn-1" || e.Items[0].ID != creditA {
				t.Fatalf("show: %+v", e)
			}
		}
		if e.Kind == "credit" {
			credits++
			if e.Session != "codex-thread-a" || e.IDs[0] != creditA || e.Verdict != "used" {
				t.Fatalf("credit: %+v", e)
			}
		}
	}
	if shows != 1 || credits != 1 {
		t.Fatalf("shows=%d credits=%d", shows, credits)
	}
}

func TestCodexReadAndNoneSurviveReconnect(t *testing.T) {
	cs, home := codexClient(t)
	if out, bad := codexCall(t, cs, codexMeta("thread-a", "turn-1", "read-1"), "memory_read", map[string]any{"id": creditA}); bad {
		t.Fatal(out)
	}
	cs2 := mcpConnectMode(t, home, true)
	if out, bad := codexCall(t, cs2, codexMeta("thread-a", "turn-2", "credit-1"), "memory_credit", map[string]any{"ids": []string{creditA}}); bad {
		t.Fatal(out)
	}
	if out, bad := codexCall(t, cs2, codexMeta("thread-a", "turn-3", "credit-2"), "memory_credit", map[string]any{"verdict": "none"}); bad {
		t.Fatal(out)
	}
	var used, none int
	for _, e := range codexEvents(t, home) {
		if e.Kind == "credit" {
			if e.Verdict == "none" {
				none++
			} else {
				used++
			}
		}
	}
	if used != 1 || none != 1 {
		t.Fatalf("used=%d none=%d", used, none)
	}
}

func TestCodexRefusesMissingOrConflictingMetadata(t *testing.T) {
	cs, home := codexClient(t)
	for _, meta := range []mcp.Meta{nil, {"threadId": "a"}, codexMeta("../a", "t", "c"), codexMeta("a", "", "c"), codexMeta("a", "t", "")} {
		if out, bad := codexCall(t, cs, meta, "memory_credit", map[string]any{"verdict": "none"}); !bad {
			t.Fatalf("accepted %+v: %s", meta, out)
		}
	}
	meta := codexMeta("a", "t", "c")
	meta["x-codex-turn-metadata"].(map[string]any)["thread_id"] = "b"
	if out, bad := codexCall(t, cs, meta, "memory_credit", map[string]any{"verdict": "none"}); !bad {
		t.Fatalf("conflict accepted: %s", out)
	}
	if e := codexEvents(t, home); len(e) != 0 {
		t.Fatalf("invalid metadata wrote events: %+v", e)
	}
}

func TestCodexCreditsFailOnJournalErrorAndCannotMutateMemories(t *testing.T) {
	cs, home := codexClient(t)
	if _, ok := toolNames(t, cs)["memory_store"]; ok {
		t.Fatal("capture must not be enabled by the credit adapter")
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "c"), "memory_credit", map[string]any{"verdict": "wrong", "ids": []string{creditA}, "correction": "different", "evidence": "test"}); !bad {
		t.Fatal(out)
	}
	dir := pathsOf(home).FeedbackDir()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "c"), "memory_credit", map[string]any{"verdict": "none"}); !bad {
		t.Fatalf("false success: %s", out)
	}
}

func TestCodexConcurrentCreditsDoNotDoubleCount(t *testing.T) {
	cs, home := codexClient(t)
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "read"), "memory_read", map[string]any{"id": creditA}); bad {
		t.Fatal(out)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: codexMeta("a", "t", "credit"), Name: "memory_credit", Arguments: map[string]any{"ids": []string{creditA}}})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var n int
	for _, e := range codexEvents(t, home) {
		if e.Kind == "credit" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("concurrent credits: %d", n)
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "different"), "memory_credit", map[string]any{"verdict": "none"}); !bad {
		t.Fatalf("conflicting verdict: %s", out)
	}
}

func TestCodexPauseAndFailedReadProduceNoCredits(t *testing.T) {
	cs, home := codexClient(t)
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "bad-read"), "memory_read", map[string]any{"id": creditB}); !bad {
		t.Fatalf("missing memory: %s", out)
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "bad-credit"), "memory_credit", map[string]any{"ids": []string{creditB}}); !bad {
		t.Fatalf("unseen memory: %s", out)
	}
	store := session.Store{Dir: pathsOf(home).SessionsDir()}
	if err := store.Update("codex-a", func(st *session.State) error { st.Paused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "paused"), "memory_credit", map[string]any{"verdict": "none"}); !bad {
		t.Fatalf("paused: %s", out)
	}
	if es := codexEvents(t, home); len(es) != 0 {
		t.Fatalf("unexpected events: %+v", es)
	}
}

func TestCodexCreditsFeedExistingSettlement(t *testing.T) {
	cs, home := codexClient(t)
	if out, bad := codexCall(t, cs, codexMeta("a", "t1", "read"), "memory_read", map[string]any{"id": creditA}); bad {
		t.Fatal(out)
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t1", "credit"), "memory_credit", map[string]any{"ids": []string{creditA}}); bad {
		t.Fatal(out)
	}
	es := codexEvents(t, home)
	settled, late := feedback.Decide(es, feedback.Params{TurnTimeout: time.Minute}, time.Now().Add(3*time.Hour))
	if len(settled) != 1 || settled[0].Outcome != feedback.OutcomeCredit || late != 0 {
		t.Fatalf("settled=%+v late=%d", settled, late)
	}
}

func TestCodexAndDefaultClientShareBankWithoutSharingCredits(t *testing.T) {
	cs, home := codexClient(t)
	mcpParentPID = 4848
	t.Cleanup(func() { mcpParentPID = 0 })
	if err := session.WriteLive(pathsOf(home).SessionsDir(), 4848, session.Live{SessionID: "legacy", Cwd: home, Project: "billing", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	legacy := mcpConnect(t, home)
	if _, ok := toolNames(t, legacy)["memory_store"]; !ok {
		t.Fatal("default client lost its store tool")
	}
	if out, bad := call(t, legacy, "memory_read", map[string]any{"id": creditA}); bad {
		t.Fatal(out)
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "read"), "memory_read", map[string]any{"id": creditA}); bad {
		t.Fatal(out)
	}
	showIn(t, home, "legacy", creditB)
	if out, bad := call(t, legacy, "memory_credit", map[string]any{"ids": []string{creditA}}); !bad {
		t.Fatalf("default client borrowed a credit: %s", out)
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "credit"), "memory_credit", map[string]any{"ids": []string{creditB}}); !bad {
		t.Fatalf("new client borrowed a credit: %s", out)
	}
	if out, bad := call(t, legacy, "memory_credit", map[string]any{"ids": []string{creditB}}); bad {
		t.Fatal(out)
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "credit-own"), "memory_credit", map[string]any{"ids": []string{creditA}}); bad {
		t.Fatal(out)
	}
}

func TestCodexInvalidClientIsUsageError(t *testing.T) {
	code, _, _ := run(t, "mcp", "--client", "unknown")
	if code != output.Usage {
		t.Fatalf("code=%d", code)
	}
}

func TestCodexCreditSchemaExcludesCorrections(t *testing.T) {
	cs, home := codexClient(t)
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tool := range listed.Tools {
		if tool.Name != "memory_credit" {
			continue
		}
		found = true
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		for _, unsupported := range []string{"correction", "evidence", "confirmed", "wrong", "outdated"} {
			if strings.Contains(string(raw), unsupported) {
				t.Fatalf("unsupported credit argument advertised: %s", raw)
			}
		}
	}
	if !found {
		t.Fatal("credit tool missing")
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "c"), "memory_credit", map[string]any{"verdict": "none", "correction": "unsupported"}); !bad {
		t.Fatalf("correction field accepted: %s", out)
	}
	if events := codexEvents(t, home); len(events) != 0 {
		t.Fatalf("refused arguments wrote events: %+v", events)
	}
}

func TestCodexCreditTelemetryMatchesPersistedVerdicts(t *testing.T) {
	cs, home := codexClient(t)
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "read"), "memory_read", map[string]any{"id": creditA}); bad {
		t.Fatal(out)
	}
	for _, callID := range []string{"credit-1", "credit-2"} {
		if out, bad := codexCall(t, cs, codexMeta("a", "t", callID), "memory_credit", map[string]any{"ids": []string{creditA}}); bad {
			t.Fatal(out)
		}
	}
	if out, bad := codexCall(t, cs, codexMeta("a", "t", "conflict"), "memory_credit", map[string]any{"verdict": "none"}); !bad {
		t.Fatalf("conflicting credit accepted: %s", out)
	}
	var successes, failures int
	for _, line := range strings.Split(telemetryText(t, home), "\n") {
		if !strings.Contains(line, `"event":"credit"`) {
			continue
		}
		if strings.Contains(line, `"ok":true`) {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("credit telemetry: success=%d failure=%d", successes, failures)
	}
}
