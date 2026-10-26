//go:build !testhooks

package hook

// chaos does nothing outside a testhooks build: a release ignores
// ChaosEnv.
func chaos() {}
