//go:build linux || darwin

package session

import (
	"os"
	"testing"
	"time"
)

func TestProcessStartOfThisTest(t *testing.T) {
	start, ok := ProcessStart(os.Getpid())
	if !ok || start.After(time.Now()) || time.Since(start) > 10*time.Minute {
		t.Errorf("this test process started at %v (%v)", start, ok)
	}
	if _, ok := ProcessStart(1 << 30); ok {
		t.Error("a pid that cannot exist has a start time")
	}
}
