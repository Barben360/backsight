package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/testtool"
)

// A shell that ran a hook, a verify or a scratch stack exports variables
// that point backsight at the real machine: isolate drops every one.
func TestIsolateClearsTheMachinesEnvironment(t *testing.T) {
	real := t.TempDir() // stands for the machine's own state
	for _, k := range isolatedEnv {
		t.Setenv(k, real)
	}
	home := isolate(t)
	for _, k := range isolatedEnv {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s=%q survives isolate", k, v)
		}
	}
	d, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{d.Data, d.Config, d.State, d.Bin} {
		if !strings.HasPrefix(dir, home+string(filepath.Separator)) {
			t.Errorf("%s is outside the test's home %s", dir, home)
		}
	}
}

// The runners below stand in for systemctl/launchctl, claude and the
// detached flush: a test that reaches one without its fake gets an error
// instead of running the command (TestMain).

func TestSysRunnerRefusesWithoutAFake(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := sysRunner.Run(context.Background(), "sh", "-c", ": > "+marker)
	if err == nil || !strings.Contains(err.Error(), "without a fake") {
		t.Errorf("the real runner answered: %v", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Error("the command ran")
	}
}

func TestClaudeRunnerRefusesWithoutAFake(t *testing.T) {
	bin, marker := t.TempDir(), filepath.Join(t.TempDir(), "ran")
	// A claude of its own on PATH: the real one is never run, even when
	// the default is the real runner.
	testtool.Write(t, bin, "claude", testtool.Fake{Stdout: "2.1.310\n", Script: "write " + marker + " ran"})
	t.Setenv("PATH", bin)
	_, err := claudeRunner.Run(context.Background(), nil, "--version")
	if err == nil || !strings.Contains(err.Error(), "without a fake") {
		t.Errorf("the real runner answered: %v", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Error("claude ran")
	}
}

func TestSpawnDetachedRefusesWithoutAFake(t *testing.T) {
	// -test.run=^$: the real one would start this test binary, which then
	// runs nothing.
	if err := spawnDetached("-test.run=^$"); err == nil || !strings.Contains(err.Error(), "without a fake") {
		t.Errorf("the real spawn answered: %v", err)
	}
}
