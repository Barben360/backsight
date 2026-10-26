package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

// The lines a user sees in a session (systemMessage), rendered exactly:
// one leading emoji per state, the project after 🗂️ (issue 7).

// noticeShown skips the one-time first-session notice, so a banner is
// only the lines under test.
func noticeShown(t *testing.T, home string) {
	t.Helper()
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error { s.FirstSessionNoticeShown = true; return nil }); err != nil {
		t.Fatal(err)
	}
}

func appendConfig(t *testing.T, home, toml string) {
	t.Helper()
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(raw, []byte("\n"+toml+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startBanner(t *testing.T, home, sid string) string {
	t.Helper()
	return fire(t, "session-start", map[string]any{"session_id": sid, "source": "startup", "cwd": filepath.Join(home, "billing")}).SystemMessage
}

func holdMaintenance(t *testing.T, home string) {
	t.Helper()
	d := pathsOf(home)
	_ = os.MkdirAll(d.State, 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"command":"backup","since":"`+time.Now().UTC().Format(time.RFC3339)+`"}`), 0o600)
}

func TestSessionStartBannerStates(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, home string)
		want  string
	}{
		{"on", func(*testing.T, string) {}, "🧠 backsight · 🗂️ billing · memory on"},
		{"project excluded", func(t *testing.T, home string) {
			appendConfig(t, home, "[privacy]\nexclude_paths = [\""+filepath.Join(home, "billing")+"\"]")
		}, "⏸️ backsight · 🗂️ billing · recall on, this project is not recorded"},
		{"retain = false in .backsight.toml", func(t *testing.T, home string) {
			_ = os.WriteFile(filepath.Join(home, "billing", ".backsight.toml"), []byte("retain = false\n"), 0o600)
		}, "⏸️ backsight · 🗂️ billing · recall on, this project is not recorded"},
		{"recording off", func(t *testing.T, home string) {
			appendConfig(t, home, "[components]\nwrite = false")
		}, "🔕 backsight · 🗂️ billing · recall on, recording off"},
		{"recording off wins over an excluded project", func(t *testing.T, home string) {
			appendConfig(t, home, "[components]\nwrite = false\n[privacy]\nexclude_paths = [\""+filepath.Join(home, "billing")+"\"]")
		}, "🔕 backsight · 🗂️ billing · recall on, recording off"},
		{"server unreachable", func(*testing.T, string) { recentDown.Store(true) },
			"⚠️ backsight · 🗂️ billing · memory unavailable (server unreachable) · run `backsight doctor`"},
		{"unreachable wins over an excluded project", func(t *testing.T, home string) {
			recentDown.Store(true)
			appendConfig(t, home, "[privacy]\nexclude_paths = [\""+filepath.Join(home, "billing")+"\"]")
		}, "⚠️ backsight · 🗂️ billing · memory unavailable (server unreachable) · run `backsight doctor`"},
		{"breaker open", func(t *testing.T, home string) {
			_ = os.MkdirAll(pathsOf(home).State, 0o700)
			_ = os.WriteFile(filepath.Join(pathsOf(home).State, "breaker.json"), []byte(`{"open_until":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`), 0o600)
		}, "⚠️ backsight · 🗂️ billing · memory unavailable (server failing, retried in a few minutes) · run `backsight doctor`"},
		{"maintenance", holdMaintenance, "🛠️ backsight · 🗂️ billing · memory unavailable (maintenance in progress), back when it ends"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := hookHome(t)
			noticeShown(t, home)
			c.setup(t, home)
			if got := startBanner(t, home, "s1"); got != c.want {
				t.Errorf("banner\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestSessionStartBannerExtraLines(t *testing.T) {
	home := hookHome(t)
	noticeShown(t, home)
	setBuild(t, "0.1.0", "abc1234")
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.PluginVersion = "0.0.0-old"
		s.UpdateCheck = &state.UpdateCheck{CheckedAt: time.Now(), Installed: "0.1.0", Latest: "0.2.0"}
		return nil
	})
	d := pathsOf(home)
	_ = os.MkdirAll(d.CursorsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.CursorsDir(), "q1.json"), []byte(`{"quarantined":true}`), 0o600)
	lines := strings.Split(startBanner(t, home, "s1"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %q", lines)
	}
	if lines[0] != "🧠 backsight · 🗂️ billing · memory on" {
		t.Errorf("status line %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "⚠️ Claude Code wiring drifted (plugin 0.0.0-old is not this binary's (") ||
		!strings.HasSuffix(lines[1], ")) · run `backsight client install`") {
		t.Errorf("drift line %q", lines[1])
	}
	if lines[2] != "⬆️ backsight update 0.2.0 available · see `backsight changelog`" {
		t.Errorf("update line %q", lines[2])
	}
	if lines[3] != "⚠️ 1 session quarantined (not sent to memory) · run `backsight doctor`" {
		t.Errorf("quarantine line %q", lines[3])
	}
	_ = os.WriteFile(filepath.Join(d.CursorsDir(), "q2.json"), []byte(`{"quarantined":true}`), 0o600)
	if got := startBanner(t, home, "s2"); !strings.HasSuffix(got, "\n⚠️ 2 sessions quarantined (not sent to memory) · run `backsight doctor`") {
		t.Errorf("plural quarantine %q", got)
	}
}

func TestFirstSessionNoticeLeadsTheBanner(t *testing.T) {
	home := hookHome(t)
	lines := strings.Split(startBanner(t, home, "s1"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "👋 Memory is on: this session is stored on http://127.0.0.1:") ||
		!strings.HasSuffix(lines[0], "; `/backsight:pause` stops it for this session, `/backsight:pause project` for this project.") ||
		lines[1] != "🧠 backsight · 🗂️ billing · memory on" {
		t.Errorf("banner %q", lines)
	}
}

func TestPromptUnavailableLines(t *testing.T) {
	home := hookHome(t)
	recallStatus.Store(500)
	if got := prompt(t, home, "s1", longPrompt).SystemMessage; got != "⚠️ backsight · memory unavailable (server unreachable) · run `backsight doctor`" {
		t.Errorf("unreachable %q", got)
	}
	recallStatus.Store(0)
	holdMaintenance(t, home)
	if got := prompt(t, home, "s2", longPrompt).SystemMessage; got != "🛠️ backsight · memory unavailable (maintenance in progress), back when it ends" {
		t.Errorf("maintenance %q", got)
	}
}
