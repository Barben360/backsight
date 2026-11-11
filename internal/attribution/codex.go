package attribution

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Codex's tools (decision 0085 D7), as its rollout reader hands them over
// (internal/harness): a shell tool runs a command, under its own key
// (exec_command's cmd, the others' command, an argument array already
// joined into one string), in its workdir when it names one; apply_patch
// writes the files its patch names; and a shell command holding a patch
// (an apply_patch heredoc inside exec_command, how edits arrive from a
// model Codex has no metadata for) writes them too. Claude Code never
// names a tool so: its calls read as before.
var codexShell = map[string]string{"exec_command": "cmd", "shell": "command", "shell_command": "command", "local_shell": "command"}

// codexPatchTool is Codex's file-editing tool; its patch is under input
// (a custom tool's raw text) or patch.
const codexPatchTool = "apply_patch"

// codexCall reads the evidence of a Codex tool call; ok false for any
// other tool.
func codexCall(tool string, input map[string]json.RawMessage, cwd, home string) (Call, bool) {
	str := func(k string) string {
		var v string
		_ = json.Unmarshal(input[k], &v)
		return v
	}
	if tool == codexPatchTool {
		patch := str("input")
		if patch == "" {
			patch = str("patch")
		}
		c := Call{CWD: cwd, Write: true}
		c.Paths = patchPaths(patch, cwd, home)
		return c, true
	}
	key, ok := codexShell[tool]
	if !ok {
		return Call{}, false
	}
	cmd := str(key)
	if wd := strings.TrimSpace(str("workdir")); wd != "" {
		if !absLike(wd) && cwd != "" {
			wd = filepath.Join(cwd, wd)
		}
		if p := expand(wd, home); p != "" {
			cwd = p
		}
	}
	// The command reads as a Bash call's (its paths, cd, git -C, a
	// committing or redirecting command); the workdir is its directory,
	// not a path it names.
	in := maps.Clone(input)
	delete(in, key)
	delete(in, "workdir")
	raw, _ := json.Marshal(cmd)
	in["command"] = raw
	c := CallOf("Bash", in, cwd, home)
	if len(PatchFiles(cmd)) > 0 {
		c.Write = true // a patch writes, whether or not its paths resolve here
		for _, f := range patchPaths(cmd, cwd, home) {
			if !slices.Contains(c.Paths, f) {
				c.Paths = append(c.Paths, f)
			}
		}
	}
	return c, true
}

// shellCommand is the command a shell tool call runs, Claude Code's Bash
// or one of Codex's.
func shellCommand(tool string, input map[string]json.RawMessage) (string, bool) {
	key := "command"
	if tool != "Bash" {
		var ok bool
		if key, ok = codexShell[tool]; !ok {
			return "", false
		}
	}
	var cmd string
	if json.Unmarshal(input[key], &cmd) != nil {
		return "", false
	}
	return cmd, true
}

// patchPaths are the files a patch names, absolute (a relative one is in
// cwd, or home when there is none).
func patchPaths(patch, cwd, home string) []string {
	var out []string
	for _, f := range PatchFiles(patch) {
		if !absLike(f) {
			base := cwd
			if base == "" {
				base = home
			}
			f = filepath.Join(base, f)
		}
		if p := expand(f, home); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// patchHeaders open the lines of a Codex patch that name a file.
var patchHeaders = []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "}

// PatchFiles are the files a Codex patch (*** Begin Patch … *** End Patch)
// in text names, in order, each once; none when text holds no patch.
func PatchFiles(text string) []string {
	if !strings.Contains(text, "*** Begin Patch") {
		return nil
	}
	var out []string
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		for _, h := range patchHeaders {
			if f, ok := strings.CutPrefix(line, strings.TrimSpace(h)); ok {
				if f = strings.TrimSpace(f); f != "" && !slices.Contains(out, f) {
					out = append(out, f)
				}
			}
		}
	}
	return out
}
