package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

// --backsight-home is what a scheduled task passes instead of the
// BACKSIGHT_HOME it cannot carry (decision 0080): on every command, it
// sets BACKSIGHT_HOME before any path is resolved.
func TestBacksightHomeFlagMovesEveryDirectory(t *testing.T) {
	home := isolate(t)
	other := filepath.Join(home, "elsewhere")
	code, out, errOut := run(t, "paths", "--backsight-home", other)
	if code != output.OK {
		t.Fatalf("exit %d %s", code, errOut)
	}
	for _, d := range []string{"data", "config", "state", "bin"} {
		if !strings.Contains(out, filepath.Join(other, d)+"\n") {
			t.Errorf("%s is not under %s:\n%s", d, other, out)
		}
	}
	if strings.Contains(out, filepath.Join(home, "backsight")) {
		t.Errorf("the environment's BACKSIGHT_HOME still used:\n%s", out)
	}
	// Before the subcommand too: it is the root's.
	if _, out, _ := run(t, "--backsight-home", other, "paths"); !strings.Contains(out, filepath.Join(other, "state")) {
		t.Errorf("before the subcommand:\n%s", out)
	}
}

// A relative directory is refused as a usage error, before anything
// runs, and BACKSIGHT_HOME is left as it was.
func TestBacksightHomeFlagRefusesARelativeDirectory(t *testing.T) {
	home := isolate(t)
	before := os.Getenv("BACKSIGHT_HOME")
	for _, rel := range []string{"bs", `.\bs`, ""} {
		code, out, errOut := run(t, "paths", "--backsight-home", rel)
		if code != output.Usage || !strings.Contains(errOut, "--backsight-home") || !strings.Contains(errOut, "absolute") || out != "" {
			t.Errorf("%q: exit %d, out %q, err %q", rel, code, out, errOut)
		}
		if got := os.Getenv("BACKSIGHT_HOME"); got != before {
			t.Errorf("%q: BACKSIGHT_HOME became %q", rel, got)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "bs")); err == nil {
		t.Error("a directory was made from the relative path")
	}
}

// Hidden: it is the tasks' business, not the user's.
func TestBacksightHomeFlagIsHidden(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"--help"}, {"paths", "--help"}, {"jobs", "tick", "--help"}} {
		if _, out, _ := run(t, args...); strings.Contains(out, "backsight-home") {
			t.Errorf("%v shows it:\n%s", args, out)
		}
	}
}

// The server task registered by an earlier build runs
// `server supervise --backsight-home <dir>`: that command line still
// reads the supervisor's configuration from <dir>.
func TestServerSuperviseStillTakesBacksightHome(t *testing.T) {
	home := isolate(t)
	other := filepath.Join(home, "task home")
	code, _, errOut := run(t, "server", "supervise", "--backsight-home", other)
	if code == output.OK || code == output.Usage {
		t.Fatalf("exit %d %s", code, errOut)
	}
	// Its config is read from there: none yet, so it stops at config.toml
	// or supervise.json under other, never under the environment's home.
	if !strings.Contains(errOut, other) || strings.Contains(errOut, filepath.Join(home, "backsight")) {
		t.Errorf("not read from %s: %s", other, errOut)
	}
	if code, _, errOut := run(t, "server", "supervise", "--backsight-home", "rel"); code != output.Usage || !strings.Contains(errOut, "absolute") {
		t.Errorf("a relative one: exit %d %s", code, errOut)
	}
}

// --backsight-state-dir is BACKSIGHT_STATE_DIR for a task: the state alone
// moves; a relative one is refused; it is hidden.
func TestBacksightStateDirFlag(t *testing.T) {
	home := isolate(t)
	st := filepath.Join(home, "task state")
	code, out, errOut := run(t, "paths", "--backsight-state-dir", st)
	if code != output.OK || !strings.Contains(out, st+"\n") || !strings.Contains(out, filepath.Join(home, "backsight", "config")+"\n") {
		t.Errorf("exit %d %s\n%s", code, errOut, out)
	}
	before := os.Getenv("BACKSIGHT_STATE_DIR")
	if code, _, errOut := run(t, "paths", "--backsight-state-dir", "st"); code != output.Usage || !strings.Contains(errOut, "--backsight-state-dir") || !strings.Contains(errOut, "absolute") {
		t.Errorf("relative: exit %d %s", code, errOut)
	}
	if os.Getenv("BACKSIGHT_STATE_DIR") != before {
		t.Error("a refused flag changed BACKSIGHT_STATE_DIR")
	}
	if _, out, _ := run(t, "jobs", "tick", "--help"); strings.Contains(out, "state-dir") {
		t.Errorf("shown:\n%s", out)
	}
}
