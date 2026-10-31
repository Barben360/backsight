package doctor

import (
	"os"
	"runtime"

	"github.com/Barben360/backsight/internal/fsutil"
)

// ReadHome fills f's Home facts from the directory itself: its mode on
// Unix, the accounts its ACL lets read it on Windows.
func (f *NativeFacts) ReadHome(home string) error {
	f.Home = home
	fi, err := os.Stat(home)
	if err != nil {
		return err
	}
	f.HomeMode = fi.Mode()
	if runtime.GOOS == "windows" {
		f.HomeACL = true
		f.HomeReaders, err = fsutil.OtherReaders(home)
	}
	return err
}
