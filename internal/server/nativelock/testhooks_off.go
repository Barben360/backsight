//go:build !testhooks

package nativelock

// testLock: only a testhooks build loads another lock.
func testLock() string { return "" }
