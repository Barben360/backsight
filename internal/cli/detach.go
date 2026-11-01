package cli

import (
	"os"

	"github.com/Barben360/backsight/internal/detach"
)

// startDetachedTo starts `backsight <args>` detached from the caller (its
// own session, or out of its process group and job on Windows), its
// output going to out (nil: discarded), and does not wait for it.
func startDetachedTo(out *os.File, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return detach.Start(out, exe, args...)
}
