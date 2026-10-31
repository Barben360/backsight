//go:build windows

package fsutil

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func normal(p string) string {
	p = longName(filepath.Clean(p))
	if len(p) >= 2 && p[1] == ':' {
		p = strings.ToUpper(p[:1]) + p[1:]
	}
	return p
}

func samePath(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(Normal(a), Normal(b))
}

// longName expands the 8.3 names of p's longest existing prefix; the rest
// keeps its spelling.
func longName(p string) string {
	if l, err := LongPathName(p); err == nil {
		return l
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(longName(parent), filepath.Base(p))
}

// LongPathName is GetLongPathName: p with every 8.3 component expanded. p
// must exist.
func LongPathName(p string) (string, error) {
	return pathName(p, windows.GetLongPathName)
}

// ShortPathName is GetShortPathName: p's 8.3 spelling, p itself where the
// volume keeps no short names. p must exist.
func ShortPathName(p string) (string, error) {
	return pathName(p, windows.GetShortPathName)
}

func pathName(p string, get func(*uint16, *uint16, uint32) (uint32, error)) (string, error) {
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := get(in, &buf[0], uint32(len(buf)))
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n)
	}
}
