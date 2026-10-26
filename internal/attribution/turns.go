package attribution

import (
	"encoding/json"
	"strings"

	"github.com/Barben360/backsight/internal/transcript"
)

// IsPrompt says whether a main-session record is a typed prompt, which
// starts a turn: a user record with text once Claude Code's own blocks are
// stripped, not an interruption notice nor a local-command caveat.
func IsPrompt(r transcript.Record) bool {
	for _, t := range r.Turns {
		if t.Role == "user" {
			return !strings.HasPrefix(t.Content, "[Request interrupted") && !strings.HasPrefix(t.Content, "Caveat:")
		}
	}
	return false
}

// PromptText is a prompt record's text.
func PromptText(r transcript.Record) string {
	for _, t := range r.Turns {
		if t.Role == "user" {
			return t.Content
		}
	}
	return ""
}

// Calls lists the evidence of recs' tool calls; a record with no
// directory ran in fallback (the session's launch directory).
func Calls(recs []transcript.Record, fallback, home string) []Call {
	var out []Call
	for _, r := range recs {
		cwd := r.CWD
		if cwd == "" {
			cwd = fallback
		}
		for _, t := range r.Tools {
			out = append(out, CallOf(t.Name, t.Input, cwd, home))
		}
	}
	return out
}

// LearnAll lets r learn the worktrees and clones recs' shell commands make.
func (r *Repos) LearnAll(recs []transcript.Record, fallback string) {
	for _, rec := range recs {
		cwd := rec.CWD
		if cwd == "" {
			cwd = fallback
		}
		for _, t := range rec.Tools {
			var cmd string
			if t.Name == "Bash" && json.Unmarshal(t.Input["command"], &cmd) == nil {
				r.Learn(cmd, cwd)
			}
		}
	}
}

// LearnWords are the words a line must hold for Learn to find something.
var LearnWords = []string{"worktree", "git clone"}
