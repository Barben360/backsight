//go:build linux

package index

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const goos = "linux"

func osFSType(dir string) FSInfo {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
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
	best, bestType := -1, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(pre)
		after := strings.Fields(post)
		if len(fields) < 5 || len(after) < 1 {
			continue
		}
		mp := unescapeMount(fields[4])
		if under("linux", dir, mp) && len(mp) > best {
			best, bestType = len(mp), after[0]
		}
	}
	return bestType
}

// unescapeMount decodes the \040-style octal escapes of mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
