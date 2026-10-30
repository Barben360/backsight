//go:build darwin

package index

import "golang.org/x/sys/unix"

const goos = "darwin"

func osFSType(dir string) FSInfo {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		// Unknown falls back to local: refusing to open on a guess would break
		// the common case; the path rules still run.
		return FSInfo{FSLocal, ""}
	}
	return classifyDarwin(unix.ByteSliceToString(st.Fstypename[:]))
}
