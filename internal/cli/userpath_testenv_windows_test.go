//go:build windows

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

// isolateUserPath points the user PATH at a throwaway key under HKCU, so
// no test touches the real Environment; cleanup deletes the key.
func isolateUserPath() (cleanup func()) {
	key := fmt.Sprintf(`Software\backsight-test-%d`, os.Getpid())
	userPathKey = key
	return func() { _ = registry.DeleteKey(registry.CURRENT_USER, key) }
}
