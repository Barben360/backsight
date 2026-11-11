//go:build linux || darwin

package session

import (
	"os"
	"testing"
	"time"
)

// initAt is when this test binary's package variables were set: after
// its process started.
var initAt = time.Now()

// This process started after its binary was written and before its
// package variables were set: no bound on how long ago that was. The
// second either way is the start's precision (boot time and ps both in
// whole seconds).
func TestProcessStartOfThisTest(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	built, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	start, ok := ProcessStart(os.Getpid())
	if !ok || start.Before(built.ModTime().Add(-time.Second)) || start.After(initAt.Add(time.Second)) {
		t.Errorf("this test process started at %v (%v), want between its binary's write %v and its start-up %v", start, ok, built.ModTime(), initAt)
	}
	if _, ok := ProcessStart(1 << 30); ok {
		t.Error("a pid that cannot exist has a start time")
	}
}
