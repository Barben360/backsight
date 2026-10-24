package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if !strings.Contains(h.SystemMessage, "Memory is on") || !strings.Contains(h.SystemMessage, "/backsight:pause") {
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
	if h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": home}); h.Specific.Context != "" || h.SystemMessage != "" {
		t.Errorf("read=false still injected: %+v", h)
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
