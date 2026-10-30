package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/hook"
	"github.com/Barben360/backsight/internal/state"
)

// The lines a user sees in a session (systemMessage), rendered exactly:
// one leading emoji per state, the project after 🗂️ (issue 7). hookHome
// sets NO_COLOR, so these compare plain text; TestBannerBrandColor checks
// the color.

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
		{"on", func(*testing.T, string) {}, "🧠 backsight · 🗂️ billing"},
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
	if lines[0] != "🧠 backsight · 🗂️ billing" {
		t.Errorf("status line %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "⚠️ Claude Code wiring drifted (plugin 0.0.0-old is not this binary's (") ||
		!strings.HasSuffix(lines[1], ")) · run `backsight client install`") {
		t.Errorf("drift line %q", lines[1])
	}
	if lines[2] != "⬆️ backsight "+buildinfo.Version+" → 0.2.0 · update here: `/backsight:update` · or in a terminal: `backsight update apply`" {
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
		lines[1] != "🧠 backsight · 🗂️ billing" {
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

func TestBannerBrandColor(t *testing.T) {
	home := hookHome(t)
	noticeShown(t, home)
	t.Setenv("NO_COLOR", "")
	if got, want := startBanner(t, home, "s1"), "🧠 \x1b[1;38;2;201;80;122mbacksight\x1b[0m · 🗂️ billing"; got != want {
		t.Errorf("colored banner\n got %q\nwant %q", got, want)
	}
	recallStatus.Store(500)
	if got, want := prompt(t, home, "s2", longPrompt).SystemMessage, "⚠️ \x1b[1;38;2;201;80;122mbacksight\x1b[0m · memory unavailable (server unreachable) · run `backsight doctor`"; got != want {
		t.Errorf("colored prompt line\n got %q\nwant %q", got, want)
	}
	recallStatus.Store(0)
	t.Setenv("NO_COLOR", "1")
	if got := startBanner(t, home, "s3"); got != "🧠 backsight · 🗂️ billing" {
		t.Errorf("NO_COLOR banner %q", got)
	}
}

// A different server answering at the joined address is a line of its own:
// recall may still answer (from the other server), sending holds.
func TestSessionStartBannerNamesAnotherServer(t *testing.T) {
	home := hookHome(t)
	noticeShown(t, home)
	if got := startBanner(t, home, "s1"); strings.Contains(got, "different memory server") {
		t.Fatalf("no mismatch recorded: %q", got)
	}
	setMismatch(t, home, "b") // local mode: a record left behind holds nothing
	if got := startBanner(t, home, "s2"); strings.Contains(got, "different memory server") {
		t.Fatalf("local mode: %q", got)
	}
	recordMismatch(t, home)
	want := "🧠 backsight · 🗂️ billing\n⚠️ a different memory server answers at the joined address: notes and sessions wait on this PC · run `backsight doctor`"
	if got := startBanner(t, home, "s3"); got != want {
		t.Errorf("banner\n got %q\nwant %q", got, want)
	}
}

// failingSince records that the server has not answered since at.
func failingSince(t *testing.T, home string, at time.Time) {
	t.Helper()
	d := pathsOf(home)
	_ = os.MkdirAll(d.State, 0o700)
	raw := `{"last_ok":"` + at.Add(-time.Hour).UTC().Format(time.RFC3339) + `","failing_since":"` + at.UTC().Format(time.RFC3339) + `","sessions_waiting":2}`
	if err := os.WriteFile(filepath.Join(d.State, "reach.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A server silent for over an hour gets a line of its own, read from the
// reach record (no call of its own); a server answering this briefing, or
// another server at the joined address, does not.
func TestSessionStartBannerAfterAnHourUnreachable(t *testing.T) {
	home := hookHome(t)
	noticeShown(t, home)
	recentDown.Store(true)
	unavailable := "⚠️ backsight · 🗂️ billing · memory unavailable (server unreachable) · run `backsight doctor`"
	failingSince(t, home, time.Now().Add(-59*time.Minute))
	if got := startBanner(t, home, "s1"); got != unavailable {
		t.Errorf("59 min\n got %q\nwant %q", got, unavailable)
	}
	since := time.Now().Add(-61 * time.Minute)
	failingSince(t, home, since)
	start := time.Now()
	got := startBanner(t, home, "s2")
	if took := time.Since(start); took > hook.ChildDeadline("session-start", []byte(`{"source":"startup"}`)) {
		t.Errorf("the hook took %s", took)
	}
	want := unavailable + "\n⚠️ memory server silent since " + since.UTC().Format("2006-01-02 15:04") + " UTC: notes and sessions wait on this PC · run `backsight doctor`"
	if got != want {
		t.Errorf("61 min\n got %q\nwant %q", got, want)
	}
	recentDown.Store(false) // it answers again before the next tick or recall records it
	if got := startBanner(t, home, "s3"); got != "🧠 backsight · 🗂️ billing" {
		t.Errorf("server back: %q", got)
	}
	recentDown.Store(true)
	recordMismatch(t, home)
	if got := startBanner(t, home, "s4"); strings.Contains(got, "silent") || !strings.Contains(got, "different memory server") {
		t.Errorf("mismatch: %q", got)
	}
}
