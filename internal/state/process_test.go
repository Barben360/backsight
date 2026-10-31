package state

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestProcessAliveOwnProcess(t *testing.T) {
	if !ProcessAlive(os.Getpid(), time.Now()) {
		t.Error("this process, since now: dead")
	}
	if !ProcessAlive(os.Getpid(), time.Time{}) {
		t.Error("this process, no start time: dead")
	}
}

func TestProcessAliveExitedChild(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if ProcessAlive(cmd.Process.Pid, time.Now()) {
		t.Errorf("exited child %d: alive", cmd.Process.Pid)
	}
}

// PID 0 and negative PIDs name process groups to kill(2): never a process.
func TestProcessAliveRefusesNonPositivePID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if ProcessAlive(pid, time.Now()) {
			t.Errorf("pid %d: alive", pid)
		}
	}
}

// A PID that runs but started after since is a reused one (Linux tells
// start times).
func TestProcessAliveReusedPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("start times are read from /proc")
	}
	if ProcessAlive(os.Getpid(), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("a process started after since: alive")
	}
}
