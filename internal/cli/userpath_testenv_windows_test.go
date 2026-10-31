//go:build windows

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

// userPathKeyEnv hands the throwaway key to re-executed test binaries.
const userPathKeyEnv = "BACKSIGHT_TEST_USERPATH_KEY"

// isolateUserPath points the user PATH at a throwaway key under HKCU, so
// no test touches the real Environment, nor a child the test re-executes
// (it inherits the key through the environment). Only the process that
// made the key deletes it.
func isolateUserPath() (cleanup func()) {
	if k := os.Getenv(userPathKeyEnv); k != "" {
		userPathKey = k
		return func() {}
	}
	key := fmt.Sprintf(`Software\backsight-test-%d`, os.Getpid())
	userPathKey = key
	_ = os.Setenv(userPathKeyEnv, key)
	return func() { _ = registry.DeleteKey(registry.CURRENT_USER, key) }
}
