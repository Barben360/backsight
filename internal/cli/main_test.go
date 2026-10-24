package cli

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// refuseDocker stands in for the real runner: a test that reaches Docker
// without installing a fake fails instead of touching the machine.
type refuseDocker struct{}

func (refuseDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("test reached docker %v without a fake runner", args)
}

func TestMain(m *testing.M) {
	composeRunner = refuseDocker{}
	os.Exit(m.Run())
}
