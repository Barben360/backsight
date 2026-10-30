//go:build linux

package index

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const goos = "linux"

func osFSType(dir string) FSInfo {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		// Unknown falls back to local: refusing to open, or dropping WAL
		// on a guess, would break the common case; the path rules still run.
		return FSInfo{FSLocal, ""}
	}
	magic := int64(uint32(st.Type))
	mt := ""
	if magic == 0x65735546 {
		mt = mountTypeOf(dir)
	}
	return classifyLinux(magic, mt)
}

// mountTypeOf is the filesystem type of the longest mount point holding dir.
func mountTypeOf(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	return parseMountInfo(f, dir)
}
