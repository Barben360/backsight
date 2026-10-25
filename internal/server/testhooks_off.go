//go:build !testhooks

package server

// testImage: only a testhooks build pins another image.
func testImage() string { return "" }
