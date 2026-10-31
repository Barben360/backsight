//go:build windows && testhooks

package cli

import "os"

// A testhooks build writes the user PATH to the throwaway key named by
// BACKSIGHT_TEST_USERPATH_KEY (install.ps1's tests run a built binary): no
// test writes the real Environment key. A release has no testhooks tag.
func init() {
	if k := os.Getenv("BACKSIGHT_TEST_USERPATH_KEY"); k != "" {
		userPathKey = k
	}
}
