//go:build testhooks

package server

// TestImage, set with -ldflags "-X …/internal/server.TestImage=<ref>" in a
// testhooks build (make build TEST_HOOKS=1 TEST_IMAGE=…), pins another
// Hindsight image: CI's update proof upgrades the server to it. A release
// has no testhooks tag, so no such variable, and -X is then ignored.
var TestImage string

func init() {
	if TestImage != "" {
		HindsightImage = TestImage
	}
}
