package attribution

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/testtool"
	"github.com/Barben360/backsight/internal/transcript"
)

const addHello = "*** Begin Patch\n*** Add File: hello.txt\n+hi\n*** Update File: /home/u/src/b/main.go\n@@\n-a\n+b\n*** Delete File: old.txt\n*** Update File: x.go\n*** Move to: y.go\n*** End Patch\n"

// Codex's tool map (decision 0085 D7): its shell tools read as Bash, in
// their workdir; apply_patch, alone or as a heredoc in the shell, writes
// the files its patch names, relative ones in the call's directory.
func TestCodexCallsGiveTheirEvidence(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX paths in the tool calls")
	home := "/home/u"
	cases := []struct {
		name  string
		tool  string
		in    map[string]json.RawMessage
		cwd   string
		paths []string
		write bool
		bash  bool
	}{
		{"exec_command's cmd", "exec_command", input("cmd", "cd /home/u/src/a && go test ./..."), "/home/u", []string{"/home/u/src/a", "/home/u/src/a"}, false, true},
		{"a commit in exec_command", "exec_command", input("cmd", "git commit -m x"), "/home/u/src/a", nil, true, true},
		{"the workdir is the call's directory, not a path it names", "exec_command", input("cmd", "go test ./...", "workdir", "/home/u/src/c"), "/home/u", nil, false, true},
		{"a heredoc patch in the workdir", "exec_command", input("cmd", "apply_patch <<'EOF'\n"+addHello+"EOF\n", "workdir", "/home/u/src/a"), "/home/u",
			[]string{"/home/u/src/a/hello.txt", "/home/u/src/b/main.go", "/home/u/src/a/old.txt", "/home/u/src/a/x.go", "/home/u/src/a/y.go"}, true, true},
		{"a relative workdir", "exec_command", input("cmd", "apply_patch <<'EOF'\n"+addHello+"EOF\n", "workdir", "sub"), "/home/u/src/a",
			[]string{"/home/u/src/a/sub/hello.txt", "/home/u/src/b/main.go", "/home/u/src/a/sub/old.txt", "/home/u/src/a/sub/x.go", "/home/u/src/a/sub/y.go"}, true, true},
		{"shell's command", "shell", input("command", "echo hi > /home/u/src/a/out.txt"), "", []string{"/home/u/src/a/out.txt"}, true, true},
		{"shell_command", "shell_command", input("command", "ls ~/src/b"), "/home/u", []string{"/home/u/src/b"}, false, true},
		{"apply_patch as a custom tool", "apply_patch", input("input", addHello), "/home/u/src/a",
			[]string{"/home/u/src/a/hello.txt", "/home/u/src/b/main.go", "/home/u/src/a/old.txt", "/home/u/src/a/x.go", "/home/u/src/a/y.go"}, true, false},
		{"apply_patch as a function", "apply_patch", input("patch", "*** Begin Patch\n*** Add File: n.go\n*** End Patch"), "/home/u/src/a", []string{"/home/u/src/a/n.go"}, true, false},
		{"apply_patch with no patch", "apply_patch", input("input", "nothing"), "/home/u/src/a", nil, true, false},
	}
	for _, c := range cases {
		got := CallOf(c.tool, c.in, c.cwd, home)
		slices.Sort(got.Paths)
		want := slices.Clone(c.paths)
		slices.Sort(want)
		if !slices.Equal(got.Paths, want) || got.Write != c.write || got.Bash != c.bash {
			t.Errorf("%s: paths %q write %v bash %v, want %q %v %v", c.name, got.Paths, got.Write, got.Bash, want, c.write, c.bash)
		}
	}
	if got := CallOf("exec_command", input("cmd", "go test", "workdir", "/home/u/src/c"), "/home/u", home); got.CWD != "/home/u/src/c" {
		t.Errorf("an exec_command's directory is %q, want its workdir", got.CWD)
	}
}

// Claude Code's calls read as before: a patch in a Bash command, or a
// tool named like Codex's shell input key, is no write.
func TestClaudeCallsIgnoreCodexPatches(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX paths in the tool calls")
	got := CallOf("Bash", input("command", "cat <<'EOF'\n"+addHello+"EOF"), "/home/u/src/a", "/home/u")
	if got.Write || slices.Contains(got.Paths, "/home/u/src/a/hello.txt") {
		t.Errorf("a Claude Code Bash call with a patch text: %+v", got)
	}
	if got := CallOf("Bash", input("cmd", "git commit -m x"), "/home/u", "/home/u"); got.Write {
		t.Errorf("a Bash call's cmd key is no command: %+v", got)
	}
}

// The worktrees a Codex shell command adds are learnt as a Bash one's.
func TestCodexShellCommandsTeachWorktrees(t *testing.T) {
	w := newWorld(t)
	gone := filepath.Join(w.home, ".cache", "dev", "wt-codex")
	r := w.repos(w.alpha)
	r.LearnAll([]transcript.Record{{CWD: w.alpha, Tools: []transcript.ToolUse{{Name: "exec_command", Input: input("cmd", "git -C "+w.beta+" worktree add "+gone+" -b feat")}}}}, w.home)
	if got := r.Project(filepath.Join(gone, "x.go"), nil); got != "beta" {
		t.Errorf("a worktree added by exec_command: %q, want beta", got)
	}
}

func TestPatchFiles(t *testing.T) {
	if got := PatchFiles(addHello); !slices.Equal(got, []string{"hello.txt", "/home/u/src/b/main.go", "old.txt", "x.go", "y.go"}) {
		t.Errorf("%q", got)
	}
	if got := PatchFiles("*** Add File: a.go\n"); got != nil {
		t.Errorf("no Begin Patch: %q", got)
	}
}
