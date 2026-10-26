package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// refuseDocker stands in for the real runner: a test that reaches Docker
// without installing a fake fails instead of touching the machine.
type refuseDocker struct{}

func (refuseDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("test reached docker %v without a fake runner", args)
}

// goraceSet: the caller chose the race options, which TestMain keeps.
var goraceSet bool

func TestMain(m *testing.M) {
	// The race runtime sleeps 1 s at exit while other threads live: twice
	// per hook (the shim and its child) under -race, past SessionEnd's
	// 800 ms child deadline. The children inherit this (issue #281).
	if _, goraceSet = os.LookupEnv("GORACE"); !goraceSet {
		_ = os.Setenv("GORACE", "atexit_sleep_ms=0")
	}
	if os.Getenv("BACKSIGHT_TEST_HOOK_CHILD") == "1" { // the supervisor's child (hook_test.go)
		args := os.Args[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "-test.") {
			args = args[1:]
		}
		os.Exit(Execute(args, os.Stdout, os.Stderr))
	}
	composeRunner = refuseDocker{}
	githubAPI = "http://" + hangUpListener().Addr().String() // nothing answers: no test reaches GitHub
	os.Exit(m.Run())
}

// The race runtime's exit sleep (1 s per process with live threads) made
// the hook children miss their deadlines: TestMain turns it off for them,
// unless the caller chose GORACE.
func TestRaceChildrenSkipTheExitSleep(t *testing.T) {
	if goraceSet {
		t.Skip("GORACE set by the caller")
	}
	if got := os.Getenv("GORACE"); got != "atexit_sleep_ms=0" {
		t.Errorf("GORACE=%q", got)
	}
}

// liveProcess is the PID
 of a process that runs until the test ends and is
// neither this process nor its parent: a lock owner that is alive and
// someone else.
func liveProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}
