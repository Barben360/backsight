// Package termstyle decides whether an output stream gets color, and
// paints text by role when it does (decision 0084). Color is for people
// at a terminal only: every program-facing output (Claude Code's Bash
// tool, hooks, MCP, pipes, --json, scheduled jobs) keeps its bytes.
package termstyle

import (
	"errors"
	"os"
	"strings"
)

// Mode is what --color or BACKSIGHT_COLOR asks for.
type Mode int

const (
	Auto Mode = iota
	Always
	Never
)

func (m Mode) String() string {
	switch m {
	case Always:
		return "always"
	case Never:
		return "never"
	}
	return "auto"
}

// ParseMode reads auto, always or never (lowercase, exact).
func ParseMode(s string) (Mode, error) {
	switch s {
	case "auto":
		return Auto, nil
	case "always":
		return Always, nil
	case "never":
		return Never, nil
	}
	return Auto, errors.New("want auto, always or never")
}

// Level is how much color a stream gets; None is off.
type Level int

const (
	None Level = iota
	Basic
	Ansi256
	TrueColor
)

// Inputs are what Resolve decides from, for one stream.
type Inputs struct {
	JSON  bool // --json: the envelope is for a program
	Plain bool // the command's class is plain (0084 D2)
	Flag  Mode // --color; Auto when absent
	// Getenv reads the environment; nil means os.Getenv.
	Getenv func(string) string
	// Terminal: the stream's writer is an *os.File on a terminal.
	Terminal bool
}

// Resolve applies 0084 D1, first decisive rule wins: --json and plain
// commands are off; then --color, BACKSIGHT_COLOR (always or never; auto
// and invalid values fall through), NO_COLOR; then auto: a terminal, TERM
// not dumb, and not inside Claude Code (CLAUDECODE). FORCE_COLOR and
// CLICOLOR_FORCE are never read: a shell profile exporting one would send
// escapes into Claude's Bash calls, which inherit that environment.
func Resolve(in Inputs) Level {
	getenv := in.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if in.JSON || in.Plain {
		return None
	}
	if lvl, ok := decide(in.Flag, getenv); ok {
		return lvl
	}
	if m, err := ParseMode(getenv("BACKSIGHT_COLOR")); err == nil {
		if lvl, ok := decide(m, getenv); ok {
			return lvl
		}
	}
	if getenv("NO_COLOR") != "" {
		return None
	}
	if !in.Terminal || getenv("TERM") == "dumb" || getenv("CLAUDECODE") != "" {
		return None
	}
	return DetectLevel(getenv)
}

// decide is a mode's verdict when it is decisive (always or never).
func decide(m Mode, getenv func(string) string) (Level, bool) {
	switch m {
	case Always:
		return DetectLevel(getenv), true
	case Never:
		return None, true
	}
	return None, false
}

// DetectLevel is the terminal's color depth, never None: truecolor for
// COLORTERM truecolor/24bit or Windows Terminal (WT_SESSION), 256 colors
// for a TERM naming 256color, else the 16 basic colors.
func DetectLevel(getenv func(string) string) Level {
	switch ct := getenv("COLORTERM"); {
	case ct == "truecolor" || ct == "24bit", getenv("WT_SESSION") != "":
		return TrueColor
	case strings.Contains(getenv("TERM"), "256color"):
		return Ansi256
	}
	return Basic
}
