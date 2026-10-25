//go:build testhooks

package buildinfo

// TestHooks: this binary was built with the testhooks tag, for CI's update
// proof; never a release.
const TestHooks = true
