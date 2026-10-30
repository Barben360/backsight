//go:build testhooks

package nativelock

// TestLock, set with -ldflags "-X …/internal/server/nativelock.TestLock=<path>"
// in a testhooks build (make build TEST_HOOKS=1 TEST_LOCK=<path>), loads
// another lock in place of the embedded one: CI's native upgrade proof
// installs a second version from it. A release has no testhooks tag, so no
// such variable, and -X is then ignored.
var TestLock string

func testLock() string { return TestLock }
