package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

const optinSession = "01a1c72b-9533-7bb0-9002-52dca295a387" // session-root.txt

// codexRolloutAt puts the recorded root rollout in dir's sessions/.
func codexRolloutAt(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile("../testdata/codex/0.167.0/rollouts/root.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(dir, "sessions", "2026", "11", "10")
	_ = os.MkdirAll(day, 0o700)
	p := filepath.Join(day, "rollout-2026-11-10T20-34-44-"+optinSession+".jsonl")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC) // settled by the idle rule
	_ = os.Chtimes(p, old, old)
	return p
}

// waitingSessions are the session ids `retain waiting` lists, and the
// sessions `retain backfill --plan` would send.
func waitingSessions(t *testing.T) ([]string, int) {
	t.Helper()
	code, out, errOut := run(t, "retain", "waiting", "--json")
	if code != output.OK {
		t.Fatalf("retain waiting: exit %d\n%s\n%s", code, out, errOut)
	}
	var env struct {
		Data waitingReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var ids []string
	for _, w := range env.Data.Sessions {
		ids = append(ids, w.Session)
	}
	_, out, _ = run(t, "retain", "backfill", "--plan", "--json")
	var plan struct {
		Data struct {
			Plan struct {
				Send int `json:"send"`
			} `json:"plan"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &plan)
	return ids, plan.Data.Plan.Send
}

// cursorFor reports that the flush recorded a cursor for session (it was
// read and sent).
func cursorFor(home, session string) bool {
	m, _ := filepath.Glob(filepath.Join(pathsOf(home).CursorsDir(), "*"+session+"*"))
	return len(m) > 0
}

// Recording Codex's conversations is opt-in: a machine that never wired
// Codex lists, surveys and flushes nothing of ~/.codex/sessions, whatever
// it holds; once client install --codex recorded the home, its rollouts
// are listed and flushed.
func TestCodexRolloutsAreReadOnlyOnceWired(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)) // retain_since before the session
	run(t, "jobs", "tick")
	withClock(t, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	codexRolloutAt(t, filepath.Join(home, ".codex"))

	if got := conversations(home, codex); len(got) != 0 {
		t.Errorf("unwired: listed %v", got)
	}
	ids, send := waitingSessions(t)
	if slices.Contains(ids, optinSession) || send != 0 {
		t.Errorf("unwired: waiting %v, backfill would send %d", ids, send)
	}
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || cursorFor(home, optinSession) {
		t.Errorf("unwired: jobs tick exit %d, flushed the rollout: %s", code, out)
	}

	// Wired (what client install --codex records): listed, surveyed, flushed.
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.ClientFor(harnessid.Codex).PluginVersion = "0.0.0-dev+abc"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := conversations(home, codex); len(got) != 1 || !strings.Contains(got[0], optinSession) {
		t.Errorf("wired: listed %v", got)
	}
	ids, send = waitingSessions(t)
	if !slices.Contains(ids, optinSession) || send != 1 {
		t.Errorf("wired: waiting %v, backfill would send %d", ids, send)
	}
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !cursorFor(home, optinSession) {
		t.Errorf("wired: jobs tick exit %d, no cursor for the rollout: %s", code, out)
	}
}

// A Codex home wired elsewhere (--codex-home) is the one read: never the
// default ~/.codex, even when it holds rollouts too.
func TestCodexRolloutsAreReadInTheRecordedHomeOnly(t *testing.T) {
	home := devInstall(t, true)
	codexRolloutAt(t, filepath.Join(home, ".codex"))
	other := filepath.Join(home, "codex-other")
	_ = os.MkdirAll(other, 0o700)
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		c := s.ClientFor(harnessid.Codex)
		c.Profile, c.PluginVersion = other, "0.0.0-dev+abc"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := conversations(home, codex); len(got) != 0 {
		t.Errorf("listed the default home's: %v", got)
	}
	p := codexRolloutAt(t, other)
	if got := conversations(home, codex); !slices.Equal(got, []string{p}) {
		t.Errorf("listed %v, want %s", got, p)
	}
}
