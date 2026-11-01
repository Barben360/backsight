package plugin

import (
	"os"
	"testing"

	"github.com/Barben360/backsight/internal/testtool"
)

// TestMain lets the test binary run as a fake shim (testtool).
func TestMain(m *testing.M) {
	testtool.Main()
	os.Exit(m.Run())
}
