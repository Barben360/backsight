// Package attribution decides which project a turn of work belongs to from
// the paths its tool calls name (decision 0072, rule S5 of experiment E32):
// a path majority over the user's own repositories, where a writing call
// weighs three reads, able to override the directory a session was
// launched in. It is pure over its inputs (the turn's records, the
// repositories on disk, the configuration), so the same turn always gets
// the same answer.
package attribution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Call is the evidence one tool call gives: the absolute paths it names,
// the directory it ran in, and whether it writes.
type Call struct {
	Paths []string
	CWD   string
	Write bool
	Bash  bool // a shell command: with no path it votes for its directory
}

// writeTools change files.
var writeTools = map[string]bool{"Edit": true, "Write": true, "NotebookEdit": true, "MultiEdit": true}

// pathKeys hold one path each.
var pathKeys = []string{"file_path", "path", "notebook_path"}

var (
	// writeCmd: a shell command that commits, publishes or edits in place.
	writeCmd = regexp.MustCompile(`git\s+(?:-C\s+\S+\s+)?(?:commit|push|merge|rebase)\b|gh\s+pr\s+(?:create|merge)|\bsed\s+-i|>\s*[~/$]`)
	cdRe     = regexp.MustCompile(`(?:^|[;&|(]\s*|\s)cd\s+(['"]?)([^\s'";&|]+)`)
	gitCRe   = regexp.MustCompile(`git\s+-C\s+(['"]?)([^\s'";&|]+)`)
	cwdRe    = regexp.MustCompile(`--cwd[= ](['"]?)([^\s'";&|]+)`)
	// absRe is anchored: absPaths tries it where no word character comes
	// before (a lookbehind RE2 lacks).
	absRe = regexp.MustCompile("^((?:~|\\$HOME|\\$\\{HOME\\})?/[A-Za-z0-9._@+-][^\\s'\"`;|&<>(){}\\[\\],:*?]*)")
)

// CallOf reads one tool call's evidence. A path is absolute, under ~ or
// $HOME, or relative to cwd when a path key gives it; a relative path in a
// command is no evidence (the shell may have moved before it).
func CallOf(tool string, input map[string]json.RawMessage, cwd, home string) Call {
	c := Call{CWD: cwd, Bash: tool == "Bash", Write: writeTools[tool]}
	str := func(k string) (string, bool) {
		var v string
		if json.Unmarshal(input[k], &v) != nil {
			return "", false
		}
		return v, true
	}
	add := func(p string) {
		if p = expand(p, home); p != "" {
			c.Paths = append(c.Paths, p)
		}
	}
	for _, k := range pathKeys {
		if v, ok := str(k); ok && strings.TrimSpace(v) != "" {
			v = strings.TrimSpace(v)
			if !absLike(v) {
				base := cwd
				if base == "" {
					base = home
				}
				v = filepath.Join(base, v)
			}
			add(v)
		}
	}
	if cmd, ok := str("command"); ok && writeCmd.MatchString(cmd) {
		c.Write = true
	}
	keys := make([]string, 0, len(input))
	for k := range input {
		if !slices.Contains(pathKeys, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys) // one order for the same input, whatever the map's
	for _, k := range keys {
		text, ok := str(k)
		if !ok {
			continue
		}
		for _, re := range []*regexp.Regexp{cdRe, gitCRe, cwdRe} {
			for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
				q, end := text[m[2]:m[3]], m[5]
				if q != "" && (end >= len(text) || text[end:end+1] != q) {
					continue // an opening quote with no closing one right after
				}
				add(text[m[4]:m[5]])
			}
		}
		for _, p := range absPaths(text) {
			if strings.HasPrefix(p, "/") && hostPath(p) == p && !userPath(p, home) {
				continue // system paths: never a project
			}
			add(p)
		}
	}
	return c
}

// absPaths lists the absolute (or ~, $HOME) paths of text not glued to a
// word before them.
func absPaths(text string) []string {
	var out []string
	for i := 0; i < len(text); {
		if i == 0 || !glued(text[i-1]) {
			m := ""
			switch b := text[i]; {
			case b == '/' || b == '~' || b == '$':
				m = absRe.FindString(text[i:])
			case driveRe != nil:
				m = driveRe.FindString(text[i:])
			}
			if m != "" {
				out = append(out, m)
				i += len(m)
				continue
			}
		}
		i++
	}
	return out
}

func glued(b byte) bool {
	return b == '_' || b == '.' || b == '$' || b == '/' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// userPath: where a user's repositories live, as opposed to the system's.
func userPath(p, home string) bool {
	for _, root := range []string{"/home", "/tmp", "/opt", "/srv", "/root", "/Users", home, os.TempDir()} {
		if root != "" && root != "/" && strings.HasPrefix(p, root) {
			return true
		}
	}
	return false
}

func absLike(p string) bool {
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "$") || filepath.IsAbs(p)
}

// expand resolves ~, $HOME and ${HOME}; a relative path gives "".
func expand(p, home string) string {
	p = strings.ReplaceAll(strings.ReplaceAll(p, "${HOME}", home), "$HOME", home)
	if strings.HasPrefix(p, "~") {
		p = home + p[1:]
	}
	p = hostPath(p)
	if !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}
