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

func TestMain(m *testing.M) {
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

// liveProcess is the PID of a process that runs until the test ends and is
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
