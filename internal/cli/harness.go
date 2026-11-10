package cli

import (
	"context"
	"os"

	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/state"
)

// claudeCode is Claude Code's adapter, for the commands that wire Claude
// Code (client, setup, uninstall, update).
var claudeCode = harness.Claude{}

// toolRunner runs the command of harness id: the claude CLI through
// claudeRunner (tests replace it); nil for a harness whose command
// backsight never runs.
func toolRunner(id string) harness.Runner {
	if id != harness.ClaudeCode {
		return nil
	}
	return func(ctx context.Context, env []string, args ...string) ([]byte, error) {
		return claudeRunner.Run(ctx, env, args...)
	}
}

// harnessEnv is the Env of a call to a in profile ("" its default).
func harnessEnv(a harness.Adapter, home, profile string) harness.Env {
	return harness.Env{Home: home, Profile: profile, Run: toolRunner(a.ID())}
}

// wiredProfile is the profile of a that backsight is wired to: the shell's
// (a hook runs with the session's, the user's own shell names it) when
// set, else the one client install recorded, else a's default. A
// scheduled job has no shell's profile: the recorded one is how it reads
// the right one.
func wiredProfile(a harness.Adapter) string {
	if v := a.ProfileVar(); v != "" {
		if p := os.Getenv(v); p != "" {
			return p
		}
	}
	if d, _, err := machine(); err == nil {
		if st, err := state.Load(d.StateFile()); err == nil {
			return st.ProfileOf(a.ID())
		}
	}
	return ""
}

// conversations lists a's conversations in the profile backsight is wired
// to (wiredProfile).
func conversations(home string, a harness.Adapter) []string {
	return a.Conversations(harness.Env{Home: home, Profile: wiredProfile(a)})
}
