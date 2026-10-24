package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
)

func setComponents(t *testing.T, home, toml string) {
	t.Helper()
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("[components]\n"+toml+"\n")...), 0o600)
}

// M2 review: components.write = false stops every writer (SPEC §5
// exclusivity), not only two hooks.
func TestWriteOffStopsEveryWriter(t *testing.T) {
	home := hookHome(t)
	setComponents(t, home, "write = false")
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	before := retained.Load()
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "components.write is off") {
		t.Errorf("tick: exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "store", "--kind", "decision", "Keep PostgreSQL 16."); code != output.Blocked {
		t.Errorf("store: exit %d", code)
	}
	cs := mcpSessionWith(t, "[components]\nwrite = false\n")
	if out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "x"}); !isErr || !strings.Contains(out, "components.write") {
		t.Errorf("memory_store: %v %s", isErr, out)
	}
	if retained.Load() != before {
		t.Errorf("%d items sent with write off", retained.Load()-before)
	}
}

func TestReadOffStopsTheReadTools(t *testing.T) {
	cs := mcpSessionWith(t, "[components]\nread = false\n")
	for tool, args := range map[string]map[string]any{"memory_search": {"queries": []string{"q"}}, "memory_read": {"id": "m1"}} {
		if out, isErr := call(t, cs, tool, args); !isErr || !strings.Contains(out, "components.read") {
			t.Errorf("%s: %v %s", tool, isErr, out)
		}
	}
}

// A subagent's memory search is not the main thread's: the gate still asks
// the main thread to search, and its seen set is untouched.
func TestSubagentToolCallsLeaveTheParentPromptAlone(t *testing.T) {
	home := hookHome(t)
	_ = sessions(home).Update("s1", func(st *session.State) error { st.Prompts = 2; return nil })
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "agent_id": "a1", "tool_name": "mcp__plugin_backsight_memory__memory_search",
		"tool_response": "18ffc1b1-7481-42b0-be72-f487a392d647"})
	st, _ := sessions(home).Load("s1")
	if st.SearchedPromptID != 0 || len(st.Seen) != 0 {
		t.Errorf("%+v", st)
	}
	if g := gate(t, home); g.Specific.Decision != "deny" {
		t.Error("the gate let the main thread through on a subagent's search")
	}
}

func TestCompactionReleasesMemoriesNoLongerInContext(t *testing.T) {
	home := hookHome(t)
	_ = sessions(home).Update("s1", func(st *session.State) error {
		for i := 1; i <= 8; i++ {
			st.Injected = append(st.Injected, session.Injection{ID: "m" + itoa(i), Text: "t", Prompt: i})
			st.Seen = append(st.Seen, "m"+itoa(i))
		}
		st.Seen = append(st.Seen, "searched-only")
		return nil
	})
	fire(t, "session-start", map[string]any{"session_id": "s1", "source": "compact", "cwd": home})
	st, _ := sessions(home).Load("s1")
	if strings.Join(st.Seen, ",") != "m3,m4,m5,m6,m7,m8" {
		t.Errorf("seen after compaction %v", st.Seen)
	}
}

// Briefings mute nothing outside their own session.
func TestBriefingsDoNotMuteOtherSessions(t *testing.T) {
	home := hookHome(t)
	recentAnswer["observation"] = "[" + obs("m1", "memory m1 about billing", "2026-10-24T09:00:00Z") + "]"
	for _, sid := range []string{"a", "b", "c"} {
		fire(t, "session-start", map[string]any{"session_id": sid, "source": "startup", "cwd": filepath.Join(home, "billing")})
	}
	recallAnswer = hits(0.9)
	if c := prompt(t, home, "cleared", longPrompt).Specific.Context; !strings.Contains(c, "memory m1") {
		t.Errorf("a briefed memory was muted in a session that never saw it:\n%s", c)
	}
}

func TestPausedSessionsSurvivePruning(t *testing.T) {
	home := hookHome(t)
	d := pathsOf(home)
	_ = sessions(home).Update("old-paused", func(st *session.State) error { st.Paused = true; return nil })
	_ = sessions(home).Update("old", func(st *session.State) error { return nil })
	past := time.Now().Add(-40 * 24 * time.Hour)
	for _, id := range []string{"old-paused", "old"} {
		_ = os.Chtimes(filepath.Join(d.SessionsDir(), id+".json"), past, past)
	}
	pruneSessions(d)
	if _, err := os.Stat(filepath.Join(d.SessionsDir(), "old-paused.json")); err != nil {
		t.Error("a paused session was pruned")
	}
	if _, err := os.Stat(filepath.Join(d.SessionsDir(), "old.json")); err == nil {
		t.Error("an old session was kept")
	}
}

func TestClientConfigApplyRepairsThePermissionRule(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	run(t, "client", "install")
	settings := filepath.Join(home, ".claude", "settings.json")
	_ = os.WriteFile(settings, []byte(`{"permissions":{"allow":["Read"]}}`), 0o600)
	if code, out, _ := run(t, "client", "config", "apply"); code != output.OK || !strings.Contains(out, "memory_search") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	raw, _ := os.ReadFile(settings)
	if !strings.Contains(string(raw), "mcp__plugin_backsight_memory__memory_store") || !strings.Contains(string(raw), `"Read"`) {
		t.Errorf("settings %s", raw)
	}
}

func TestSearchOneFailedScopeIsNotMemoryDown(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recallStatus.Store(0)
	recallAnswer = "[]"
	if code, out, _ := run(t, "search", "nothing", "--project", "other"); code != output.OK || !strings.Contains(out, "nothing in memory") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestWhyOnACorruptSessionIsBlocked(t *testing.T) {
	home := isolate(t)
	d := pathsOf(home)
	_ = os.MkdirAll(d.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.SessionsDir(), "s1.json"), []byte("{bad"), 0o600)
	if code, _, _ := run(t, "why", "--session", "s1"); code != output.Blocked {
		t.Errorf("exit %d", code)
	}
}

func TestConfigBoostAndStickyWindowAreRead(t *testing.T) {
	home := hookHome(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("[recall]\nproject_boost = 0.2\nsticky_window_h = 1\n")...), 0o600)
	recallAnswer = `[{"id":"own","text":"own","type":"observation","tags":["project:billing"],"scores":{"reranker":0.7}},
		{"id":"other","text":"other","type":"observation","tags":["project:.claude"],"scores":{"reranker":0.85}}]`
	code, out, _ := run(t, "recall-prompt", "--cwd", filepath.Join(home, "billing"), "--json", longPrompt)
	if code != 0 || !strings.Contains(out, `"injected": [`+"\n"+`      "own"`) {
		t.Errorf("boost 0.2 must put own (0.7+0.2) before other (0.85):\n%s", out)
	}
	// with a 1 h window, injections 2 h old do not mute
	recallAnswer = hits(0.9)
	st := session.Sticky{Path: filepath.Join(pathsOf(home).State, "sticky.json"), Locks: pathsOf(home).LocksDir()}
	for i := 0; i < 3; i++ {
		_ = st.Record([]string{"m1"}, time.Now().Add(-2*time.Hour), 24*time.Hour)
	}
	if c := prompt(t, home, "s1", longPrompt).Specific.Context; !strings.Contains(c, "memory m1") {
		t.Errorf("sticky window from config ignored:\n%s", c)
	}
}

func TestStopStoresTheReplyRedacted(t *testing.T) {
	home := hookHome(t)
	fire(t, "stop", map[string]any{"session_id": "s1", "cwd": home, "last_assistant_message": "Use GITHUB_TOKEN=ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + " for CI."})
	if st, _ := sessions(home).Load("s1"); strings.Contains(st.LastAssistant, "a1B2c3D4e5F6") {
		t.Errorf("%s", st.LastAssistant)
	}
}

func TestAgentQueriesAreRedacted(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	run(t, "search", "deploy GITHUB_TOKEN=ghp_"+strings.Repeat("a1B2c3D4e5F6", 3))
	for _, q := range recallQueries {
		if strings.Contains(q, "a1B2c3D4e5F6") {
			t.Errorf("query %q", q)
		}
	}
}
