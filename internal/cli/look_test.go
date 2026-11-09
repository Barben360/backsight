package cli

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// lookRoot is the command tree as a run sees it, cobra's help and
// completion commands included.
func lookRoot() *cobra.Command {
	root := newRoot(&env{stdout: io.Discard, stderr: io.Discard})
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	return root
}

func walkCommands(c *cobra.Command, visit func(*cobra.Command)) {
	visit(c)
	for _, sub := range c.Commands() {
		walkCommands(sub, visit)
	}
}

// TestLookClassesGolden lists every command with its class (0084 D2): a
// new command fails here until its class is reviewed and recorded.
func TestLookClassesGolden(t *testing.T) {
	var lines []string
	walkCommands(lookRoot(), func(c *cobra.Command) {
		lines = append(lines, c.CommandPath()+"\t"+string(classOf(c)))
	})
	sort.Strings(lines)
	got := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "look-classes.golden")
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("command classes drifted: review each new or changed command's class (decision 0084 D2: plain if a program reads its output or it prints a value to capture), then go test ./internal/cli -run %s -update\ngot:\n%s", t.Name(), got)
	}
}

func TestLookPlainCommandsFromSpec(t *testing.T) {
	root := lookRoot()
	want := map[lookClass][]string{
		lookPlain: {"mcp", "completion", "completion bash", "jobs", "jobs tick", "jobs maintain", "secret", "secret set", "secret list",
			"server token", "backup identity show", "recall-prompt", "detect-replay", "__install-shim"},
		lookNoSplash: {"guide"},
		lookFull:     {"", "help", "version", "doctor", "update check", "update apply", "stats index", "backup identity import"},
	}
	for class, paths := range want {
		for _, p := range paths {
			c, _, err := root.Find(strings.Fields(p))
			if err != nil || c.CommandPath() != strings.TrimSpace("backsight "+p) {
				t.Errorf("%q: not found (%v, found %q)", p, err, c.CommandPath())
				continue
			}
			if got := classOf(c); got != class {
				t.Errorf("%q: class %q, want %q", p, got, class)
			}
		}
	}
}

func TestClassForArgs(t *testing.T) {
	cases := []struct {
		args []string
		want lookClass
	}{
		{[]string{"__complete", "st"}, lookPlain},
		{[]string{"__completeNoDesc"}, lookPlain},
		{[]string{"--json", "--color=never", "jobs", "tick"}, lookPlain},
		{[]string{"--backsight-home", "/x", "jobs", "tick"}, lookPlain},
		{[]string{"nosuch"}, lookFull},
		{nil, lookFull},
		{[]string{"help", "jobs"}, lookFull},
		{[]string{"completion", "zsh"}, lookPlain},
		{[]string{"guide"}, lookNoSplash},
	}
	for _, c := range cases {
		root := newRoot(&env{stdout: io.Discard, stderr: io.Discard})
		if got := classForArgs(root, c.args); got != c.want {
			t.Errorf("classForArgs(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestUnknownCommandStderrUnchanged(t *testing.T) {
	code, out, errOut := run(t, "nosuch")
	if code != 2 || out != "" || !strings.HasPrefix(errOut, `backsight: unknown command "nosuch"`) {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}
