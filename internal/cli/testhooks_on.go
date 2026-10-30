//go:build testhooks

package cli

import (
	"errors"
	"os"
)

// smokeHook fails the server upgrade's smoke when
// BACKSIGHT_TEST_FAIL_SMOKE is set: CI's update proof of a failed server
// upgrade (decision 0067). A release has no testhooks tag.
func smokeHook() error {
	if os.Getenv("BACKSIGHT_TEST_FAIL_SMOKE") != "" {
		return errors.New("test hook: BACKSIGHT_TEST_FAIL_SMOKE is set")
	}
	return nil
}

// switchFailHook fails the runtime switch at step when
// BACKSIGHT_TEST_FAIL_SWITCH names it: CI's proof of a rolled-back switch.
func switchFailHook(step string) error {
	if os.Getenv("BACKSIGHT_TEST_FAIL_SWITCH") == step {
		return errors.New("test hook: BACKSIGHT_TEST_FAIL_SWITCH=" + step)
	}
	return nil
}
