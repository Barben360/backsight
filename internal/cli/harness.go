package cli

import (
	"context"
	"os"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// claudeCode is Claude Code's adapter, for the commands that wire Claude
// Code (client, setup, uninstall, update).
var claudeCode = harness.Claude{}

// toolRunner runs the command of harness id: the claude CLI through
// claudeRunner, the codex CLI through codexRunner (tests replace both);
// nil for a harness whose command backsight never runs.
func toolRunner(id string) harness.Runner {
	runner := map[string]func() claudeCLI{
		harness.ClaudeCode: func() claudeCLI { return claudeRunner },
		harnessid.Codex:    func() claudeCLI { return codexRunner },
	}[id]
	if runner == nil {
		return nil
	}
	return func(ctx context.Context, env []string, args ...string) ([]byte, error) {
		return runner().Run(ctx, env, args...) // read at the call: tests replace the runners
	}
}

// harnessEnv is the Env of a call to a in profile ("" its default).
func harnessEnv(a harness.Adapter, home, profile string) harness.Env {
	return harness.Env{Home: home, Profile: profile, Run: toolRunner(a.ID())}
}

// wiredRecord is what drift compares for a, from st: the profile wired
// and what client install recorded there. Claude Code's profile is the
// shell's (a hook runs with the session's, decision 0048), its version
// state's plugin_version; another tool's are those its own state entry
// records, never a shell's (0085 D1).
func wiredRecord(a harness.Adapter, home, data string, st state.State) (harness.Env, harness.Recorded) {
	env := harness.Env{Home: home}
	rec := harness.Recorded{ShimTarget: st.ShimTarget, Data: data, BinaryVersion: buildinfo.Version}
	if a.ID() == harness.ClaudeCode {
		env.Profile, rec.Version = os.Getenv(a.ProfileVar()), st.PluginVersion
		return env, rec
	}
	if c := st.ClientOf(a.ID()); c != nil {
		env.Profile, rec.Version = c.Profile, c.PluginVersion
	}
	return env, rec
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
// to (wiredProfile). A tool whose home is recorded when it is wired
// (Codex, a HomeRecorder) is read only once wired there: recording its
// conversations is opt-in, so a machine that never wired it reads nothing
// of its default home (~/.codex), whatever it holds.
func conversations(home string, a harness.Adapter) []string {
	if _, records := a.(harness.HomeRecorder); records && !toolWired(a) {
		return nil
	}
	return a.Conversations(harness.Env{Home: home, Profile: wiredProfile(a)})
}

// toolWired reports that client install recorded a's integration: its
// state entry names a plugin version.
func toolWired(a harness.Adapter) bool {
	d, _, err := machine()
	if err != nil {
		return false
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return false
	}
	c := st.ClientOf(a.ID())
	return c != nil && c.PluginVersion != ""
}

// conversationReaders are the conversation readers of every harness that
// is on, and Codex's, on or not: retain reads each conversation it is handed
// with the one that recognises its path, so a Codex rollout a hook or a
// cursor names is never read as a Claude Code transcript.
func conversationReaders() []retain.Harness {
	var out []retain.Harness
	codex := false
	for _, a := range harness.All() {
		out, codex = append(out, a), codex || a.ID() == harnessid.Codex
	}
	if a, ok := harness.ByID(harnessid.Codex); ok && !codex {
		out = append(out, a)
	}
	return out
}

// otherConversations lists the conversations of every harness that is on
// besides Claude Code's, in the profiles backsight is wired to.
func otherConversations(home string) []string {
	var out []string
	for _, a := range harness.All() {
		if a.ID() != harness.ClaudeCode {
			out = append(out, conversations(home, a)...)
		}
	}
	return out
}

// allConversations lists the conversations of every harness that is on:
// Claude Code's (transcripts), then the others'.
func allConversations(home string) []string {
	return append(transcripts(home), otherConversations(home)...)
}

// filesOf are the conversations of groups, in order.
func filesOf(groups []harnessFiles) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g.files...)
	}
	return out
}
