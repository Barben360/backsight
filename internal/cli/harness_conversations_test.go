package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// Codex's reader is always among the readers, on or not: a rollout a
// hook or a cursor names is read as one, never as a Claude Code
// transcript; any other path is Claude Code's.
func TestConversationReadersKnowCodex(t *testing.T) {
	rs := conversationReaders()
	rollout := "/h/.codex/sessions/2026/11/10/rollout-2026-11-10T20-34-44-01a1c72b-9533-7bb0-9002-52dca295a387.jsonl"
	if h := retain.HarnessOf(rs, rollout); h.ID() != harnessid.Codex || h.SessionOf(rollout) != "01a1c72b-9533-7bb0-9002-52dca295a387" {
		t.Errorf("a rollout is read by %s", h.ID())
	}
	if h := retain.HarnessOf(rs, "/h/.claude/projects/p/s1.jsonl"); h.ID() != "claude-code" {
		t.Errorf("a Claude Code transcript is read by %s", h.ID())
	}
	withFakeTool(t)
	ids := []string{}
	for _, r := range conversationReaders() {
		ids = append(ids, r.ID())
	}
	if !slices.Equal(ids, []string{"claude-code", harnessid.Codex, "fake-tool"}) {
		t.Errorf("readers %v", ids)
	}
}

// Every harness's sessions are listed where Claude Code's are, beyond the
// flush: retain waiting names the second harness's session by its id, and
// backfill's plan counts it.
func TestEveryHarnessesSessionsAreWaitingAndSurveyed(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)) // retain_since before the sessions
	run(t, "jobs", "tick")
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	withClock(t, at.Add(10*time.Minute))
	withFakeTool(t)
	writeTranscript(t, home, "live", at)
	const sid = "0193f0aa-1111-4222-8333-444455556666"
	fk := filepath.Join(home, ".fake-tool", "sessions", "rollout-2026-10-24T09-00-00-"+sid+".fz")
	writeFakeConversation(t, fk, "user|fk1-u1|"+at.Format(time.RFC3339)+"|"+filepath.Join(home, "billing")+"|The fake tool keeps billing on PostgreSQL 17.\n")
	code, out, errOut := run(t, "retain", "waiting", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	var env struct {
		Data waitingReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var sessions []string
	for _, w := range env.Data.Sessions {
		sessions = append(sessions, w.Session)
	}
	slices.Sort(sessions)
	if !slices.Equal(sessions, []string{sid, "live"}) {
		t.Errorf("waiting %v, want both harnesses' sessions\n%s", sessions, out)
	}
	code, out, errOut = run(t, "retain", "backfill", "--plan", "--json")
	var plan struct {
		Data struct {
			Plan struct {
				Send int `json:"send"`
			} `json:"plan"`
		} `json:"data"`
	}
	if code != output.OK || json.Unmarshal([]byte(out), &plan) != nil || plan.Data.Plan.Send != 2 {
		t.Errorf("backfill's plan does not count both harnesses' sessions: exit %d\n%s\n%s", code, out, errOut)
	}
}

// A queued Codex rollout that Codex compressed in place since is still
// to send: only a session whose conversation is gone leaves the backfill
// queue.
func TestBackfillKeepsACompressedRollout(t *testing.T) {
	home := devInstall(t, true)
	day := filepath.Join(home, ".codex", "sessions", "2026", "11", "10")
	_ = os.MkdirAll(day, 0o700)
	packed := filepath.Join(day, "rollout-2026-11-10T20-34-44-01a1c72b-9533-7bb0-9002-52dca295a387.jsonl")
	if err := os.WriteFile(packed+".zst", []byte("compressed"), 0o600); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(day, "rollout-2026-11-10T20-34-58-01a1c72b-cd0a-7550-ae7a-76acc8d479c6.jsonl")
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.Backfill = &state.Backfill{Total: 2, MaxCost: 10, Currency: "USD", Sessions: []string{packed, gone}}
		return nil
	})
	n, err := dropSettled(pathsOf(home))
	st, _ := state.Load(pathsOf(home).StateFile())
	if err != nil || n != 1 || st.Backfill == nil || !slices.Equal(st.Backfill.Sessions, []string{packed}) {
		t.Errorf("dropped %d (%v), queue %+v: want the gone one dropped, the compressed one kept", n, err, st.Backfill)
	}
}
