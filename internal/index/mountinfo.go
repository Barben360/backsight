package index

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// parseMountInfo returns the filesystem type of the longest mount point
// holding dir, from /proc/self/mountinfo lines. It compiles everywhere so
// its test runs everywhere.
func parseMountInfo(r io.Reader, dir string) string {
	best, bestType := -1, ""
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// Optional fields sit before the " - " separator, so the type is
		// the first field after it.
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
