package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/testtool"
)

// refuseDocker stands in for the real runner: a test that reaches Docker
// without installing a fake fails instead of touching the machine.
type refuseDocker struct{}

func (refuseDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("test reached docker %v without a fake runner", args)
}

// refuseSys, refuseClaude and refuseSpawn do the same for systemctl and
// launchctl, claude and the detached flush or server start.
type refuseSys struct{}

func (refuseSys) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("test reached %s %v without a fake runner", name, args)
}

// refuseTools: no test runs a native tool (uv, pg0, psql, hindsight-admin).
type refuseTools struct{}

func (refuseTools) Run(_ context.Context, _ []string, _ io.Reader, path string, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("test reached %s %v without a fake tool runner", path, args)
}

type refuseClaude struct{}

func (refuseClaude) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("test reached claude %v without a fake runner", args)
}

func refuseSpawn(args ...string) error {
	return fmt.Errorf("test reached a detached backsight %v without a fake spawn", args)
}

func refuseSpawnLogged(_ *os.File, args ...string) error { return refuseSpawn(args...) }

// refuseSSH: no test reaches a real ssh server without installing a fake.
type refuseSSH struct{}

func (refuseSSH) Run(_ context.Context, args []string, _ io.Reader, _, _ io.Writer) (int, error) {
	return -1, fmt.Errorf("test reached ssh %v without a fake runner", args)
}

// testHookFault makes the test binary, run as `hook-child`, fail before
// the handler as a crashing child would: panic | fatal | hang | kill |
// exit2. Test code only; the binary's own chaos switch exists in testhooks
// builds alone.
const testHookFault = "BACKSIGHT_TEST_HOOK_FAULT"

func hookChildFault(mode string) {
	switch mode {
	case "panic":
		panic("test fault")
	case "fatal":
		var mu sync.Mutex
		mu.Unlock() // "fatal error: sync: unlock of unlocked mutex": not recoverable
	case "hang":
		time.Sleep(time.Hour)
	case "kill":
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Kill()
		}
		time.Sleep(time.Hour)
	case "exit2":
		os.Exit(2)
	}
}

// goraceSet: the caller chose the race options, which TestMain keeps.
var goraceSet bool

func TestMain(m *testing.M) {
	testtool.Main() // started as a fake program: behave as it and exit
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
		if len(args) > 0 && args[0] == "hook-child" {
			hookChildFault(os.Getenv(testHookFault))
		}
		os.Exit(Execute(args, os.Stdout, os.Stderr))
	}
	composeRunner = refuseDocker{}
	nativeSys, nativeTools = refuseSys{}, refuseTools{}
	sysRunner, claudeRunner, spawnDetached, spawnLogged = refuseSys{}, refuseClaude{}, refuseSpawn, refuseSpawnLogged
	sshRunner = refuseSSH{}
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
