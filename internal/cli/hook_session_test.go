package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
)

type hookOut struct {
	SystemMessage string `json:"systemMessage"`
	Specific      struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// fire runs one event handler in-process (the supervisor has its own tests).
func fire(t *testing.T, event string, in map[string]any) hookOut {
	t.Helper()
	raw, _ := json.Marshal(in)
	out, err := handleHook(context.Background(), event, raw)
	if err != nil {
		t.Fatalf("%s: %v", event, err)
	}
	var h hookOut
	if len(out) > 0 {
		if err := json.Unmarshal(out, &h); err != nil {
			t.Fatalf("%s printed invalid JSON: %s", event, out)
		}
	}
	return h
}

func hookHome(t *testing.T) string {
	t.Helper()
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	_ = os.MkdirAll(filepath.Join(home, "billing"), 0o700)
	t.Setenv("NO_COLOR", "1") // banners compare as plain text
	return home
}

func obs(id, text, updated string) string {
	return `{"id":"` + id + `","text":"` + text + `","fact_type":"observation","tags":["project:billing"],"updated_at":"` + updated + `"}`
}

func TestSessionStartGivesTheGuideAndTheBriefing(t *testing.T) {
	home := hookHome(t)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16 for partial indexes.", time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339)) + "," +
		obs("o2", strings.Repeat("long ", 80), time.Now().Add(-49*time.Hour).UTC().Format(time.RFC3339)) + "]"
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	ctx := h.Specific.Context
	if h.Specific.Event != "SessionStart" || !strings.HasPrefix(ctx, instructions.Guide) || !strings.Contains(ctx, "Where you left off on billing "+instructions.HintsLine+":") ||
		!strings.Contains(ctx, "PostgreSQL 16") || !strings.Contains(ctx, "3h ago") || !strings.Contains(ctx, "2d ago") {
		t.Fatalf("context:\n%s", ctx)
	}
	for _, line := range strings.Split(ctx, "\n") {
		if strings.HasPrefix(line, "- ") && len([]rune(line)) > 240 {
			t.Errorf("briefing item over 220 chars: %d", len([]rune(line)))
		}
	}
	if !strings.Contains(h.SystemMessage, "billing") {
		t.Errorf("banner %q", h.SystemMessage)
	}
	env, _ := os.ReadFile(envFile)
	if !strings.Contains(string(env), "export BACKSIGHT_PROJECT='billing'") || !strings.Contains(string(env), "export BACKSIGHT_SESSION='s1'") {
		t.Errorf("env file:\n%s", env)
	}
	st, _ := sessions(home).Load("s1")
	if st.Project != "billing" || len(st.Seen) != 2 || len(st.Injected) != 2 || st.Injected[0].Rule != "briefing" || st.Injected[0].Prompt != 0 {
		t.Errorf("state %+v", st)
	}
}

func TestBriefingFallsBackToRawFactsThenEmpty(t *testing.T) {
	home := hookHome(t)
	recentAnswer[""] = `[{"id":"f1","text":"We keep PostgreSQL 16.","fact_type":"world","tags":["project:billing"],"updated_at":"2026-10-24T09:00:00Z"}]`
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.Specific.Context, "We keep PostgreSQL 16.") || !strings.Contains(h.Specific.Context, instructions.BriefingRaw) {
		t.Errorf("raw:\n%s", h.Specific.Context)
	}
	recentAnswer[""] = ""
	h = fire(t, "session-start", map[string]any{"session_id": "s2", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.Specific.Context, instructions.BriefingEmpty) {
		t.Errorf("empty:\n%s", h.Specific.Context)
	}
}

func TestSessionStartWithTheServerDownStillGuides(t *testing.T) {
	home := hookHome(t)
	recentDown.Store(true)
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.HasPrefix(h.Specific.Context, instructions.Guide) || strings.Contains(h.Specific.Context, "Where you left off") || !strings.Contains(h.SystemMessage, "unavailable") {
		t.Errorf("%+v", h)
	}
}

func TestFirstSessionNoticeOnlyOnce(t *testing.T) {
	home := hookHome(t)
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.SystemMessage, "Memory is on") || !strings.Contains(h.SystemMessage, "`/backsight:pause`") {
		t.Fatalf("banner %q", h.SystemMessage)
	}
	h = fire(t, "session-start", map[string]any{"session_id": "s2", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if strings.Contains(h.SystemMessage, "Memory is on") {
		t.Error("notice shown twice")
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if !st.FirstSessionNoticeShown {
		t.Error("not recorded")
	}
}

func TestResumeAndClearGiveTheGuideOnly(t *testing.T) {
	home := hookHome(t)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	for _, src := range []string{"resume", "clear"} {
		h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": src, "cwd": filepath.Join(home, "billing")})
		if h.Specific.Context != instructions.Guide {
			t.Errorf("%s: %q", src, h.Specific.Context)
		}
	}
}

func TestCompactReplaysRecentInjectionsAndStores(t *testing.T) {
	home := hookHome(t)
	_ = sessions(home).Update("s1", func(st *session.State) error {
		for i := 1; i <= 8; i++ {
			st.Injected = append(st.Injected, session.Injection{ID: "m" + itoa(i), Text: "memory " + itoa(i), Project: "billing", Prompt: i})
		}
		st.Stored = []string{"note:abc"}
		return nil
	})
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "compact", "cwd": filepath.Join(home, "billing")})
	c := h.Specific.Context
	if !strings.HasPrefix(c, instructions.Guide) || !strings.Contains(c, "memory 8") || !strings.Contains(c, "memory 3") || strings.Contains(c, "memory 2\n") || !strings.Contains(c, "note:abc") {
		t.Errorf("compact:\n%s", c)
	}
}

func TestSubagentStartGivesAShortGuideAndTwoItems(t *testing.T) {
	home := hookHome(t)
	_ = sessions(home).Update("s1", func(st *session.State) error {
		for i := 1; i <= 4; i++ {
			st.Injected = append(st.Injected, session.Injection{ID: "b" + itoa(i), Text: "briefed " + itoa(i) + " " + strings.Repeat("x", 300), Rule: "briefing"})
		}
		return nil
	})
	h := fire(t, "subagent-start", map[string]any{"session_id": "s1", "agent_type": "general-purpose", "cwd": filepath.Join(home, "billing")})
	c := h.Specific.Context
	if h.Specific.Event != "SubagentStart" || !strings.HasPrefix(c, instructions.SubagentGuide) || !strings.Contains(c, "briefed 1") || !strings.Contains(c, "briefed 2") ||
		strings.Contains(c, "briefed 3") || len([]rune(c)) > 600 {
		t.Errorf("%d chars:\n%s", len([]rune(c)), c)
	}
	if h := fire(t, "subagent-start", map[string]any{"session_id": "s1", "agent_type": "Explore", "cwd": filepath.Join(home, "billing")}); h.Specific.Context != "" {
		t.Errorf("skip-listed type got %q", h.Specific.Context)
	}
}

func TestReadHooksRespectTheComponentsFlagAndAMissingConfig(t *testing.T) {
	home := hookHome(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("[components]\nread = false\n")...), 0o600)
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	t.Setenv(claudePIDEnv, "31338")
	if h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": home}); h.Specific.Context != "" || h.SystemMessage != "" {
		t.Errorf("read=false still injected: %+v", h)
	}
	// write side still on: pause and the MCP server must find the session
	if env, _ := os.ReadFile(envFile); !strings.Contains(string(env), "BACKSIGHT_SESSION='s1'") {
		t.Errorf("no env file with read=false: %q", env)
	}
	if _, ok := session.ReadLive(pathsOf(home).SessionsDir(), 31338); !ok {
		t.Error("no live record with read=false")
	}
	_ = os.Remove(cfg)
	if h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": home}); h.Specific.Context != "" {
		t.Errorf("no config still injected: %+v", h)
	}
}

func TestSessionStartBannerShowsDrift(t *testing.T) {
	home := hookHome(t)
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if strings.Contains(h.SystemMessage, "drifted") {
		t.Errorf("never installed: no drift line: %q", h.SystemMessage)
	}
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error { s.PluginVersion = "0.0.0-old"; return nil })
	h = fire(t, "session-start", map[string]any{"session_id": "s2", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.SystemMessage, "drifted") || !strings.Contains(h.SystemMessage, "backsight client install") || strings.Count(h.SystemMessage, "drifted") != 1 {
		t.Errorf("banner %q (one drift line wanted)", h.SystemMessage)
	}
}

// After a backfill every memory was written by the server on one day: the
// briefing ages them by when they happened (mentioned_at).
func TestBriefingAgesByEventTime(t *testing.T) {
	home := hookHome(t)
	recentHasEventTime.Store(true)
	now := time.Now().UTC()
	recentAnswer["observation"] = `[{"id":"o1","text":"Billing keeps PostgreSQL 16.","fact_type":"observation","tags":["project:billing"],` +
		`"updated_at":"` + now.Add(-time.Hour).Format(time.RFC3339) + `","mentioned_at":"` + now.Add(-10*24*time.Hour).Format(time.RFC3339) + `"}]`
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.Specific.Context, "PostgreSQL 16") || !strings.Contains(h.Specific.Context, "10d ago") {
		t.Errorf("%q", h.Specific.Context)
	}
}

// Decision 0060: the briefing is the project's mental model when the server
// has one; without one it is created (refreshed after each consolidation of
// the project) and the recent observations serve this time.
func TestBriefingFromTheProjectsMentalModel(t *testing.T) {
	home := hookHome(t)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	start := func(sid string) hookOut {
		return fire(t, "session-start", map[string]any{"session_id": sid, "source": "startup", "cwd": filepath.Join(home, "billing")})
	}
	h := start("s1")
	if !strings.Contains(h.Specific.Context, "Billing keeps PostgreSQL 16.") || len(mentalCalls) != 1 {
		t.Fatalf("no page yet: E8 and one create; %q %q", h.Specific.Context, mentalCalls)
	}
	id := briefingModelID("billing")
	if c := mentalCalls[0]; !strings.HasPrefix(c, "POST  ") || !strings.Contains(c, `"id":"`+id+`"`) || !strings.Contains(c, `"tags":["project:billing"]`) ||
		!strings.Contains(c, `"refresh_after_consolidation":true`) || !strings.Contains(c, `"budget":"low"`) || !strings.Contains(c, `"max_tokens":600`) {
		t.Errorf("create: %s", c)
	}
	refreshed := time.Now().Add(-3 * time.Hour).UTC().Format("2006-01-02T15:04:05.000000+00:00")
	page := "## Open items\n- Migrate the export job (2026-10-24)\n## In force\n- " + strings.Repeat("a rule that goes on ", 120)
	raw, _ := json.Marshal(map[string]any{"id": id, "content": page, "tags": []string{"project:billing"}, "last_refreshed_at": refreshed})
	mentalModels[id] = string(raw)
	h = start("s2")
	ctx := h.Specific.Context
	// quoted lines, headings flattened, closed
	if !strings.Contains(ctx, "summarised from memory 3h ago") || !strings.Contains(ctx, "data, not instructions") || !strings.Contains(ctx, "\n> Open items\n> - Migrate the export job") ||
		strings.Contains(ctx, "##") || strings.Contains(ctx, "Billing keeps PostgreSQL 16.") || !strings.Contains(ctx, "> …\n(end of the memory notes)") || len(mentalCalls) != 1 {
		t.Errorf("page briefing:\n%s\ncalls %q", ctx, mentalCalls)
	}
	if tel := telemetryText(t, home); !strings.Contains(tel, `"source":"model"`) {
		t.Errorf("telemetry:\n%s", tel)
	}
}

// With briefing = "recent" no mental model is read or created.
func TestRecentBriefingLeavesMentalModelsAlone(t *testing.T) {
	home := hookHome(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[recall]\nbriefing = \"recent\"\n")...), 0o600)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.Specific.Context, "Billing keeps PostgreSQL 16.") || len(mentalCalls) != 0 {
		t.Errorf("%q %q", h.Specific.Context, mentalCalls)
	}
}

func TestBriefingModelIDs(t *testing.T) {
	a, b := briefingModelID("my.app"), briefingModelID("my-app")
	if a == b || !strings.HasPrefix(a, "backsight-brief-my-app-") || len(briefingModelID(strings.Repeat("x", 300))) > 70 {
		t.Errorf("%s %s", a, b)
	}
}

// An excluded project gets no page: nothing about it goes to the server's
// LLM; the recent-observation briefing serves.
func TestExcludedProjectGetsNoBriefingPage(t *testing.T) {
	home := hookHome(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[privacy]\nexclude_paths = [\""+filepath.Join(home, "billing")+"\"]\n")...), 0o600)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if len(mentalCalls) != 0 || !strings.Contains(h.Specific.Context, "Billing keeps PostgreSQL 16.") {
		t.Errorf("%q %q", mentalCalls, h.Specific.Context)
	}
}

func pageJSON(id, content string) string {
	raw, _ := json.Marshal(map[string]any{"id": id, "content": content, "tags": []string{"project:billing"},
		"last_refreshed_at": time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")})
	return string(raw)
}

// A slow page read gets 1.5 s: the recent-observation briefing still has
// its time, and the session is not reported as unreachable.
func TestASlowPageLeavesTheFallbackItsTime(t *testing.T) {
	home := hookHome(t)
	mentalGetDelay = 4 * time.Second
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	start := time.Now()
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.Specific.Context, "Billing keeps PostgreSQL 16.") || strings.Contains(h.SystemMessage, "unreachable") || time.Since(start) > 3*time.Second {
		t.Errorf("%s: %q %q", time.Since(start), h.Specific.Context, h.SystemMessage)
	}
}

// No page for a project with no memory: it would be summarised from
// nothing, then pruned, in a loop. A failed creation is recorded.
func TestPagesAreCreatedOnlyForProjectsWithMemories(t *testing.T) {
	home := hookHome(t)
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if len(mentalCalls) != 0 || !strings.Contains(h.Specific.Context, "Memory is empty") {
		t.Errorf("empty project: %q %q", mentalCalls, h.Specific.Context)
	}
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	mentalCreateStatus.Store(400)
	fire(t, "session-start", map[string]any{"session_id": "s2", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if len(mentalCalls) != 1 || !strings.Contains(telemetryText(t, home), `"event":"briefing_page_error"`) {
		t.Errorf("failed create: %q", mentalCalls)
	}
}

// While another server answers at the joined address, SessionStart reads
// its briefing there but creates no page on it (a write, and its LLM's
// work); once the joined one answers again, the page is created.
func TestNoBriefingPageIsCreatedOnAnotherServer(t *testing.T) {
	home := hookHome(t)
	recordMismatch(t, home)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if len(mentalCalls) != 0 || !strings.Contains(h.Specific.Context, "Billing keeps PostgreSQL 16.") {
		t.Errorf("another server: %q %q", mentalCalls, h.Specific.Context)
	}
	setMismatch(t, home, "")
	fire(t, "session-start", map[string]any{"session_id": "s2", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if len(mentalCalls) != 1 {
		t.Errorf("the joined server: %q", mentalCalls)
	}
}

// briefingAcrossConnect fires SessionStart for billing, which has memories
// and, when page is not "", that briefing page, with the sends seam set:
// raise makes it raise the sends generation once the hook loaded its
// config, as a whole connect then would. It returns the hook's output and
// whether the seam ran.
func briefingAcrossConnect(t *testing.T, raise bool, page string) (hookOut, bool) {
	t.Helper()
	home := hookHome(t)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	if page != "" {
		mentalModels[briefingModelID("billing")] = pageJSON(briefingModelID("billing"), page)
	}
	ran := false
	old := sendsConfigLoaded
	sendsConfigLoaded = func() {
		ran = true
		if raise {
			if err := state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		sendsConfigLoaded = old // one connect
	}
	t.Cleanup(func() { sendsConfigLoaded = old })
	return fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")}), ran
}

// mentalPosts counts the page creations the fake server saw.
func mentalPosts() int {
	n := 0
	for _, c := range mentalCalls {
		if strings.HasPrefix(c, "POST ") {
			n++
		}
	}
	return n
}

// A SessionStart whose config predates a whole connect still gives its
// briefing (a read), but creates no page: that write would go to the old
// server. The next SessionStart, a process that read the raised
// generation, creates it once: the gate holds back a stale process only,
// never every PC that ever connected.
func TestBriefingPageWaitsAcrossConnect(t *testing.T) {
	h, ran := briefingAcrossConnect(t, true, "")
	if !ran || !strings.Contains(h.Specific.Context, "Billing keeps PostgreSQL 16.") || mentalPosts() != 0 {
		t.Fatalf("seam ran %v, page creations %d (want 0), context %q", ran, mentalPosts(), h.Specific.Context)
	}
	home := os.Getenv("HOME")
	if g := sendsGeneration(pathsOf(home)); g == 0 {
		t.Fatalf("sends generation %d: the connect did not raise it, the second start proves nothing", g)
	}
	fire(t, "session-start", map[string]any{"session_id": "s2", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if n := mentalPosts(); n != 1 {
		t.Errorf("after the connect, a new session created the page %d times, want 1", n)
	}
}

// Reading the page stays ungated: across a connect, an existing page is
// still the briefing (a read from the old server does no harm).
func TestBriefingPageStillReadAcrossConnect(t *testing.T) {
	h, ran := briefingAcrossConnect(t, true, "## In force\n- Billing exports run on Fridays.")
	if !ran || !strings.Contains(h.Specific.Context, "Billing exports run on Fridays.") || strings.Contains(h.Specific.Context, "Billing keeps PostgreSQL 16.") || mentalPosts() != 0 {
		t.Errorf("seam ran %v, page creations %d (want 0), context %q", ran, mentalPosts(), h.Specific.Context)
	}
}

// The control: nothing moved, the page is created once.
func TestBriefingPageCreatedWhenNothingMoved(t *testing.T) {
	h, ran := briefingAcrossConnect(t, false, "")
	if !ran || !strings.Contains(h.Specific.Context, "Billing keeps PostgreSQL 16.") || mentalPosts() != 1 {
		t.Errorf("seam ran %v, page creations %d (want 1), context %q", ran, mentalPosts(), h.Specific.Context)
	}
}

// A page written as one long line is clipped, not reduced to "…".
func TestAOneLinePageIsClipped(t *testing.T) {
	home := hookHome(t)
	id := briefingModelID("billing")
	mentalModels[id] = pageJSON(id, "Open items: "+strings.Repeat("migrate the export job, ", 120))
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.Specific.Context, "> Open items: migrate the export job") || strings.Contains(h.Specific.Context, "> …") {
		t.Errorf("%q", h.Specific.Context)
	}
}

// The page survives a compaction and reaches subagents (they got the
// recent-observation briefing's top items before 0060).
func TestThePageReachesCompactionAndSubagents(t *testing.T) {
	home := hookHome(t)
	id := briefingModelID("billing")
	mentalModels[id] = pageJSON(id, "## Open items\n- Migrate the export job (2026-10-24)")
	fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	c := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "compact", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(c.Specific.Context, "Migrate the export job") {
		t.Errorf("compaction: %q", c.Specific.Context)
	}
	s := fire(t, "subagent-start", map[string]any{"session_id": "s1", "agent_type": "general-purpose", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(s.Specific.Context, "Migrate the export job") {
		t.Errorf("subagent: %q", s.Specific.Context)
	}
}

func TestSessionStartAndPromptRecordTheRepositoryRemote(t *testing.T) {
	home := hookHome(t)
	repo := filepath.Join(home, "billing")
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:Acme/billing.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": repo})
	if st, _ := sessions(home).Load("s1"); st.Project != "billing" || st.Remote != "github.com/acme/billing" {
		t.Errorf("after SessionStart: project %q remote %q", st.Project, st.Remote)
	}
	// A session started outside git and moved into the repository by the
	// prompt hook gets the remote too.
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s2", "cwd": repo, "prompt": "hello there, what did we decide about billing?"})
	if st, _ := sessions(home).Load("s2"); st.Remote != "github.com/acme/billing" || st.ProjectExplicit {
		t.Errorf("after the prompt hook: remote %q, explicit %v", st.Remote, st.ProjectExplicit)
	}
	if st, _ := sessions(home).Load("s1"); st.ProjectExplicit {
		t.Error("a name from git recorded as explicit")
	}
	// A committed .backsight.toml project key: recorded as explicit, so
	// doctor's remotes check takes the share as deliberate (0045).
	_ = os.WriteFile(filepath.Join(repo, ".backsight.toml"), []byte("project = \"platform\"\n"), 0o600)
	fire(t, "session-start", map[string]any{"session_id": "s3", "source": "startup", "cwd": repo})
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s4", "cwd": repo, "prompt": "hello there, what did we decide about billing?"})
	for _, sid := range []string{"s3", "s4"} {
		if st, _ := sessions(home).Load(sid); st.Project != "platform" || !st.ProjectExplicit {
			t.Errorf("%s: project %q, explicit %v", sid, st.Project, st.ProjectExplicit)
		}
	}
}

// The recent-observation briefing is journaled as a show before any turn,
// the first turn's journal reads start before it, and a subagent's
// briefing items are journaled with its agent id.
func TestRecentBriefingAndSubagentShows(t *testing.T) {
	home := hookHome(t)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "," + obs("o2", "Billing moved in October.", "2026-10-23T09:00:00Z") + "]"
	dir := pathsOf(home).FeedbackDir()
	fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	start, ok := feedback.CurrentTurn(dir, "s1")
	if !ok || start.ID != "" || start.N != 0 || start.File == "" {
		t.Fatalf("pointer %+v %v", start, ok)
	}
	fire(t, "subagent-start", map[string]any{"session_id": "s1", "agent_id": "a1", "agent_type": "general-purpose", "cwd": filepath.Join(home, "billing")})
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 2 {
		t.Fatalf("shows %+v", shows)
	}
	if s := shows[0]; s.Path != "briefing" || s.Turn != "" || s.Agent != "" || s.Session != "s1" || itemsText(s.Items) != "o1:1:0,o2:2:0" {
		t.Errorf("briefing %+v items %s", s, itemsText(s.Items))
	}
	if s := shows[1]; s.Path != "briefing" || s.Turn != "" || s.Agent != "a1" || itemsText(s.Items) != "o1:1:0,o2:2:0" {
		t.Errorf("subagent %+v items %s", s, itemsText(s.Items))
	}
	// The first prompt's turn starts where the session did: its reads
	// include the briefing.
	recallAnswer = "[]"
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "prompt_id": "p-1", "prompt": longPrompt, "cwd": filepath.Join(home, "billing")})
	first, _ := feedback.CurrentTurn(dir, "s1")
	if first.ID != "p-1" || first.N != 1 || first.File != start.File || first.Offset != start.Offset {
		t.Errorf("first turn %+v, session start %+v", first, start)
	}
	evs, _, _ := feedback.ReadFrom(first.File, first.Offset)
	if len(eventsOf(evs, "shows")) != 2 {
		t.Errorf("the first turn does not read the briefing: %+v", evs)
	}
	// The second starts where the journal ends.
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "prompt_id": "p-2", "prompt": longPrompt, "cwd": filepath.Join(home, "billing")})
	second, _ := feedback.CurrentTurn(dir, "s1")
	if fi, err := os.Stat(second.File); err != nil || second.Offset <= start.Offset || second.Offset >= fi.Size() {
		t.Errorf("second turn %+v", second)
	}
	// A resumed session starts a new turn pointer, without a briefing.
	fire(t, "session-start", map[string]any{"session_id": "s1", "source": "resume", "cwd": filepath.Join(home, "billing")})
	if cur, _ := feedback.CurrentTurn(dir, "s1"); cur.ID != "" || cur.N != 0 || cur.Offset <= second.Offset {
		t.Errorf("after resume %+v", cur)
	}
	if n := len(eventsOf(journalEvents(t, home), "shows")); n != 2 {
		t.Errorf("resume journaled a briefing: %d shows", n)
	}
}

// Briefing items carry their short id, at session start and in a
// subagent's briefing: a memory the agent cannot name cannot be credited.
func TestBriefingLinesCarryShortIDs(t *testing.T) {
	home := hookHome(t)
	recentAnswer["observation"] = "[" + obs("0b4c2a52-0000-4000-8000-000000000001", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "," +
		obs("1c4c2a52-0000-4000-8000-000000000002", "Billing moved in October.", "2026-10-23T09:00:00Z") + "]"
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	for _, want := range []string{"\n- [billing · 0b4c2a52] Billing keeps PostgreSQL 16. (", "\n- [billing · 1c4c2a52] Billing moved in October. ("} {
		if !strings.Contains(h.Specific.Context, want) {
			t.Errorf("briefing lacks %q:\n%s", want, h.Specific.Context)
		}
	}
	s := fire(t, "subagent-start", map[string]any{"session_id": "s1", "agent_id": "a1", "agent_type": "general-purpose", "cwd": filepath.Join(home, "billing")})
	for _, want := range []string{"\n- [billing · 0b4c2a52] Billing keeps PostgreSQL 16.", "\n- [billing · 1c4c2a52] Billing moved in October."} {
		if !strings.Contains(s.Specific.Context, want) {
			t.Errorf("subagent briefing lacks %q:\n%s", want, s.Specific.Context)
		}
	}
}

// The briefing was shown whether or not the session state could be
// written: its show is journaled with the state busy.
func TestBriefingShowsSurviveABusySessionState(t *testing.T) {
	home := hookHome(t)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	d := pathsOf(home)
	_ = os.MkdirAll(d.LocksDir(), 0o700)
	l, err := state.TryAcquire(filepath.Join(d.LocksDir(), "state-s1.lock"), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.Specific.Context, "Billing keeps PostgreSQL 16.") {
		t.Fatalf("no briefing:\n%s", h.Specific.Context)
	}
	if st, _ := sessions(home).Load("s1"); len(st.Injected) != 0 {
		t.Fatalf("the state was written despite the lock: %+v", st)
	}
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 1 || shows[0].Path != "briefing" || itemsText(shows[0].Items) != "o1:1:0" {
		t.Errorf("shows %+v", shows)
	}
}
