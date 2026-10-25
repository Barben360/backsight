package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a semantic version as goreleaser and git describe write them.
//
// Ordering follows semver 2.0.0 (core numbers, then pre-release identifiers,
// build metadata ignored), with one addition for development builds: a
// git-describe version such as v0.0.2-m0-93-gfd77ae6 (93 commits past tag
// v0.0.2-m0, optionally -dirty) is not read as a pre-release of 0.0.2-m0-93
// but as "just past its base tag": it is newer than the base tag and than
// every older release, and older than every release above the base. Two
// describe builds of one base order by their commit count, then dirty last.
type Version struct {
	Major, Minor, Patch int
	Pre                 []string // pre-release identifiers of the base tag
	Ahead               int      // git describe: commits past the base tag
	Dirty               bool     // git describe --dirty
	raw                 string   // as given, without the leading v
}

var (
	coreRE     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$`)
	describeRE = regexp.MustCompile(`^(.*)-([0-9]+)-g[0-9a-f]{4,40}$`)
	identRE    = regexp.MustCompile(`^[0-9A-Za-z-]+$`)
)

// Parse reads a version, with or without its tag's leading v. Anything else
// ("dev", a bare commit hash) is an error.
func Parse(s string) (Version, error) {
	raw := strings.TrimPrefix(s, "v")
	m := coreRE.FindStringSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("%q is not a version (MAJOR.MINOR.PATCH[-pre][+build])", s)
	}
	v := Version{raw: raw}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	pre := "-" + m[4]
	if rest, ok := strings.CutSuffix(pre, "-dirty"); ok {
		v.Dirty, pre = true, rest
	}
	if d := describeRE.FindStringSubmatch(pre); d != nil {
		v.Ahead, _ = strconv.Atoi(d[2])
		pre = d[1]
	}
	if pre = strings.TrimPrefix(pre, "-"); pre != "" {
		for _, id := range strings.Split(pre, ".") {
			if !identRE.MatchString(id) || (isNumeric(id) && len(id) > 1 && id[0] == '0') {
				return Version{}, fmt.Errorf("%q: invalid pre-release identifier %q", s, id)
			}
			v.Pre = append(v.Pre, id)
		}
	}
	return v, nil
}

// String is the version as given, without the leading v.
func (v Version) String() string { return v.raw }

// Prerelease reports whether the version (or a dev build's base tag) is a
// pre-release.
func (v Version) Prerelease() bool { return len(v.Pre) > 0 }

// Dev reports a git-describe development build.
func (v Version) Dev() bool { return v.Ahead > 0 || v.Dirty }

// Compare returns -1, 0 or 1 as a is older than, equal to or newer than b.
func Compare(a, b Version) int {
	for _, d := range [3]int{a.Major - b.Major, a.Minor - b.Minor, a.Patch - b.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	if c := comparePre(a.Pre, b.Pre); c != 0 {
		return c
	}
	if a.Ahead != b.Ahead {
		return sign(a.Ahead - b.Ahead)
	}
	switch {
	case a.Dirty == b.Dirty:
		return 0
	case a.Dirty:
		return 1
	}
	return -1
}

// comparePre orders pre-release identifiers per semver: none ranks above
// any; numeric ones compare as numbers and rank below alphanumeric ones;
// a longer list wins when every shared identifier is equal.
func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		xn, yn := isNumeric(x), isNumeric(y)
		switch {
		case xn && yn:
			if len(x) != len(y) { // no leading zeros: longer is larger
				return sign(len(x) - len(y))
			}
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		case xn:
			return -1
		case yn:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return sign(len(a) - len(b))
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func sign(d int) int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}
