package fileimport

import (
	"errors"
	"path"
	"strings"
)

// ValidGlob reports a malformed --exclude pattern before anything is read.
func ValidGlob(p string) error {
	if p == "" {
		return errors.New("empty pattern")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}

// MatchGlob matches a '/'-separated pattern against a path relative to the
// import root: '*', '?' and '[...]' within one segment (path.Match), '**'
// as a whole segment for zero or more segments. A pattern ending in '/**'
// also matches the directory itself, so the walk can prune it.
func MatchGlob(pattern, rel string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for i := 0; i <= len(segs); i++ {
				if matchSegs(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
