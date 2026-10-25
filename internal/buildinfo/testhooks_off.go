//go:build !testhooks

package buildinfo

// TestHooks reports a build with the testhooks tag (`make build
// TEST_HOOKS=1`): CI's update proof only. A release never has it (make
// verify-keys refuses one that does).
const TestHooks = false
