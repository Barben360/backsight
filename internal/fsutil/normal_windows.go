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

// longName is p's longest existing prefix with its 8.3 names expanded and
// in the disk's case (the volume is case-blind: C:\code\Shop is the disk's
// C:\code\shop); the rest keeps its spelling.
func longName(p string) string {
	if l, err := LongPathName(p); err == nil {
		return diskCase(l)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(longName(parent), filepath.Base(p))
}

// diskCase is p, which exists, with each component in the case the disk
// keeps (FindFirstFile's name), without following links; p as it is when
// a component cannot be listed.
func diskCase(p string) string {
	vol := filepath.VolumeName(p)
	out := vol + `\`
	for _, part := range strings.Split(strings.Trim(p[len(vol):], `\`), `\`) {
		if part == "" {
			continue
		}
		next := filepath.Join(out, part)
		name, err := windows.UTF16PtrFromString(next)
		if err != nil {
			return p
		}
		var fd windows.Win32finddata
		h, err := windows.FindFirstFile(name, &fd)
		if err != nil {
			return p
		}
		_ = windows.FindClose(h)
		found := windows.UTF16ToString(fd.FileName[:])
		if !strings.EqualFold(found, part) {
			found = part // a short name typed: GetLongPathName expanded it already
		}
		out = filepath.Join(out, found)
	}
	return out
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
