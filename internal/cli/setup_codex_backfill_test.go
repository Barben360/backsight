package cli

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

func backfillQuestion(t *testing.T, args ...string) (setup.Question, bool) {
	t.Helper()
	code, out, errOut := run(t, append([]string{"setup", "--questions", "--json"}, args...)...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, q := range parseSetup(t, out).Data.Questions {
		if q.ID == "backfill" {
			return q, true
		}
	}
	return setup.Question{}, false
}

// The backfill question covers the tools setup wires: Codex's rollouts of
// the home being wired count (not recorded yet), Claude Code's transcripts
// as before; each tool named once Codex is among them.
func TestSetupBackfillCountsCodexsSessions(t *testing.T) {
	home, _ := setupMachine(t)
	withClock(t, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	withCodex(t, home)
	codexHomeWith(t, home, codexStart)
	codexRolloutAt(t, filepath.Join(home, ".codex"))
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)

	q, ok := backfillQuestion(t, "--answer", "tools=codex")
	if !ok || q.Prompt != "Send your 1 past Codex session to the memory, in the background?" || q.Default != "yes" {
		t.Errorf("tools=codex: %v %q default %q", ok, q.Prompt, q.Default)
	}
	q, _ = backfillQuestion(t, "--answer", "tools=both")
	if q.Prompt != "Send your 2 past sessions (1 Claude Code, 1 Codex) to the memory, in the background?" {
		t.Errorf("tools=both: %q", q.Prompt)
	}
	// Claude Code alone, Codex never wired: its rollouts are not read, the
	// question is as before Codex.
	q, _ = backfillQuestion(t, "--answer", "tools=claude-code")
	if q.Prompt != "Send your 1 past session to the memory, in the background?" {
		t.Errorf("tools=claude-code: %q", q.Prompt)
	}
	// Codex wired by an earlier run: a rerun for Claude Code alone still
	// covers it.
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.ClientFor("codex").PluginVersion = "1.0.0+abc"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	q, _ = backfillQuestion(t, "--answer", "tools=claude-code")
	if q.Prompt != "Send your 2 past sessions (1 Claude Code, 1 Codex) to the memory, in the background?" {
		t.Errorf("rerun, Codex wired: %q", q.Prompt)
	}
}

// The backfill step queues what the question counted: Codex's rollouts with
// --tools, Claude Code's from the profile setup wires, as before Codex
// when it is alone. The plan says which tools. The verify step runs each
// tool's verify, with one round trip.
func TestSetupBackfillStepQueuesEachTool(t *testing.T) {
	for _, c := range []struct {
		tools, want, plan string
		verify            []string
	}{
		{"codex", "retain backfill --queue --tools codex", "queue past Codex sessions for jobs tick", []string{"verify --codex"}},
		{"both", "retain backfill --queue --tools both --projects-dir ", "queue past sessions of Claude Code and Codex for jobs tick", []string{"verify", "verify --codex --no-round-trip"}},
	} {
		t.Run(c.tools, func(t *testing.T) {
			home, steps := setupMachine(t)
			withLLMKey(t, home)
			withClock(t, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
			steps.real["client install"] = true
			withCodex(t, home)
			codexHomeWith(t, home, codexStart)
			codexRolloutAt(t, filepath.Join(home, ".codex"))
			args := []string{"--answer", "provider=deepseek", "--answer", "tools=" + c.tools, "--answer", "backfill=yes", "--answer", "consent=yes", "--non-interactive", "--json"}
			code, out, _ := run(t, append([]string{"setup", "--plan"}, args...)...)
			if by := stepsByName(parseSetup(t, out).Data.Steps); code != output.OK || by["backfill"].Status != "planned" || by["backfill"].Detail != c.plan {
				t.Errorf("plan: exit %d %+v", code, by["backfill"])
			}
			code, out, errOut := run(t, append([]string{"setup"}, args...)...)
			if code != output.OK {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			cmds := steps.commands()
			if !slices.ContainsFunc(cmds, func(cmd string) bool { return strings.HasPrefix(cmd, c.want) }) {
				t.Errorf("commands %q, want %q", cmds, c.want)
			}
			var verifies []string
			for _, cmd := range cmds {
				if strings.HasPrefix(cmd, "verify") {
					verifies = append(verifies, cmd)
				}
			}
			if !slices.Equal(verifies, c.verify) {
				t.Errorf("verify runs %q, want %q", verifies, c.verify)
			}
			by := stepsByName(parseSetup(t, out).Data.Steps)
			if by["backfill"].Status != "done" || by["verify"].Status != "done" || !strings.Contains(by["verify"].Detail, "Codex") {
				t.Errorf("steps backfill %+v verify %+v", by["backfill"], by["verify"])
			}
		})
	}
}

// retain backfill --tools: the queue holds exactly the sessions of the
// tools named, the count setup's estimate gave; Codex's only once wired;
// --projects-dir is Claude Code's.
func TestRetainBackfillTools(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	rollout := codexRolloutAt(t, filepath.Join(home, ".codex"))
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	claudeDir := filepath.Join(home, ".claude", "projects")

	if code, out, _ := run(t, "retain", "backfill", "--plan", "--tools", "codex", "--json"); code != output.NeedsUser || !strings.Contains(out, "client install --codex") {
		t.Errorf("Codex not wired: exit %d %s", code, out)
	}
	wireCodexRecord(t, home)
	queue := func(args ...string) []string {
		t.Helper()
		code, out, errOut := run(t, append([]string{"retain", "backfill", "--queue", "--json"}, args...)...)
		if code != output.OK {
			t.Fatalf("%v: exit %d %s %s", args, code, out, errOut)
		}
		var env struct {
			Data backfillResult `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &env)
		st, _ := state.Load(pathsOf(home).StateFile())
		if st.Backfill == nil || env.Data.Total != len(st.Backfill.Sessions) {
			t.Fatalf("%v: total %d, state %+v", args, env.Data.Total, st.Backfill)
		}
		return st.Backfill.Sessions
	}
	if got := queue("--tools", "codex"); !slices.Equal(got, []string{rollout}) {
		t.Errorf("--tools codex queued %v", got)
	}
	if got := queue("--tools", "both", "--projects-dir", claudeDir); len(got) != 2 || !slices.Contains(got, rollout) {
		t.Errorf("--tools both queued %v", got)
	}
	if got := queue("--tools", "claude-code"); len(got) != 1 || slices.Contains(got, rollout) {
		t.Errorf("--tools claude-code queued %v", got)
	}
	// --projects-dir alone is Claude Code's, as before --tools.
	if got := queue("--projects-dir", claudeDir); len(got) != 1 || slices.Contains(got, rollout) {
		t.Errorf("--projects-dir queued %v", got)
	}
	if code, _, _ := run(t, "retain", "backfill", "--plan", "--tools", "codex", "--projects-dir", claudeDir); code != output.Usage {
		t.Errorf("--projects-dir with Codex alone: exit %d", code)
	}
	if code, _, _ := run(t, "retain", "backfill", "--plan", "--tools", "cursor"); code != output.Usage {
		t.Errorf("--tools cursor: exit %d", code)
	}
}
