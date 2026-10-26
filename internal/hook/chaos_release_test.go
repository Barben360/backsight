//go:build !testhooks

package hook

import (
	"strings"
	"testing"
	"time"
)

// A release build (no testhooks tag) ignores BACKSIGHT_HOOK_CHAOS: the
// variable cannot make a user's hook panic, hang or exit.
func TestAReleaseBuildIgnoresTheChaosSwitch(t *testing.T) {
	for _, mode := range []string{"panic", "fatal", "hang", "kill", "exit2"} {
		out, f := super(t, "echo", mode, 5*time.Second)
		if f != nil || !strings.Contains(string(out), `"additionalContext":"ok"`) {
			t.Errorf("%s=%s: out %q failure %+v", ChaosEnv, mode, out, f)
		}
	}
}
