package cli

import (
	"context"
	"fmt"
	"os"
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
	githubAPI = "http://127.0.0.1:1" // closed: no test reaches GitHub
	os.Exit(m.Run())
}
