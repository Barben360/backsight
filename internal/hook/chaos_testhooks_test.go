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
		{"panic", "panic", far},
		{"fatal", "fatal", far},
		{"hang", "timeout", 400 * time.Millisecond},
		{"kill", "killed", far},
		{"exit2", "exit", far},
	} {
		out, f := super(t, "echo", c.chaos, c.deadline)
		if len(out) != 0 || f == nil || f.Kind != c.kind {
			t.Errorf("%s=%s: out %q failure %+v", ChaosEnv, c.chaos, out, f)
		}
	}
	if out, f := super(t, "echo", "", far); f != nil || len(out) == 0 {
		t.Errorf("no chaos: out %q failure %+v", out, f)
	}
}
