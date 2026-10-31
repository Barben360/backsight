package paths

import (
	"os"
	"runtime"

	"github.com/Barben360/backsight/internal/fsutil"
)

// EnsurePrivate makes backsight's own roots (data, config, state; not bin,
// which holds the shim on PATH) private to the user: on Windows each is
// given a protected DACL when it exists and any other account could read it
// (missing ones are made private by whatever creates them, fsutil.MkdirPrivate) (BACKSIGHT_HOME=C:\x gets the privacy %LOCALAPPDATA% has).
// On Unix it does nothing: the writers create 0700 as before.
func (d Dirs) EnsurePrivate() error {
	if runtime.GOOS != "windows" {
		return nil
	}
	for _, p := range []string{d.Data, d.Config, d.State} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if who, err := fsutil.OtherReaders(p); err != nil || len(who) > 0 {
			if err := fsutil.MakePrivate(p); err != nil {
				return err
			}
		}
	}
	return nil
}
