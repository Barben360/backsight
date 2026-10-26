//go:build testhooks

package hook

import (
	"os"
	"sync"
	"time"
)

// chaos fails the child as ChaosEnv asks: the chaos tests of a testhooks
// build (make test-hooks). A release has no testhooks tag (make
// verify-keys refuses one), so no environment variable can make a user's
// hook fail.
func chaos() {
	switch os.Getenv(ChaosEnv) {
	case "panic":
		panic("chaos test")
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
