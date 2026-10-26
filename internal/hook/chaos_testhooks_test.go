//go:build testhooks

package hook

import (
	"testing"
	"time"
)

// A testhooks build honours BACKSIGHT_HOOK_CHAOS before the handler runs:
// the chaos tests of a real binary (make test-hooks).
func TestATesthooksBuildHonoursTheChaosSwitch(t *testing.T) {
	for _, c := range []struct {
		chaos, kind string
		deadline    time.Duration
	}{
		{"panic", "panic", 5 * time.Second},
		{"fatal", "fatal", 5 * time.Second},
		{"hang", "timeout", 400 * time.Millisecond},
		{"kill", "killed", 5 * time.Second},
		{"exit2", "exit", 5 * time.Second},
	} {
		out, f := super(t, "echo", c.chaos, c.deadline)
		if len(out) != 0 || f == nil || f.Kind != c.kind {
			t.Errorf("%s=%s: out %q failure %+v", ChaosEnv, c.chaos, out, f)
		}
	}
	if out, f := super(t, "echo", "", 5*time.Second); f != nil || len(out) == 0 {
		t.Errorf("no chaos: out %q failure %+v", out, f)
	}
}
