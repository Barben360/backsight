//go:build darwin

package sqlitedb

import "golang.org/x/sys/unix"

const goos = "darwin"

func osFSType(dir string) FSInfo {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		// Unknown falls back to local: refusing to open on a guess would break
		// the common case; the path rules still run.
		return FSInfo{FSLocal, ""}
	}
	return ClassifyDarwin(unix.ByteSliceToString(st.Fstypename[:]))
}
