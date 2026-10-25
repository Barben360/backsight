// Package release reads backsight's releases (SPEC §11.7, decision 0067
// block 2): their versions, notes and assets, from GitHub or from a local
// release directory. It reads metadata and opens assets; it verifies
// nothing (signatures belong to internal/signing, decision 0027).
package release

import (
	"context"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Asset is one file of a release. URL is where the source fetches it from
// (the GitHub API asset URL, or a local path); callers open it with Open.
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"-"`
}

// Release is one published version.
type Release struct {
	Version    string    `json:"version"` // without the tag's v, as goreleaser names archives
	Tag        string    `json:"tag"`
	Prerelease bool      `json:"prerelease"` // the source's flag or a semver pre-release
	Published  time.Time `json:"published_at"`
	Notes      string    `json:"notes"`
	Assets     []Asset   `json:"assets"`
}

// Source lists releases and opens their assets.
type Source interface {
	// Name says where releases come from: "github:<owner>/<repo>" or
	// "dir:<path>".
	Name() string
	// Releases lists every published release, in no particular order. Any
	// error means the source could not be read.
	Releases(ctx context.Context) ([]Release, error)
	// Open streams one asset of rel by name; the caller closes it.
	Open(ctx context.Context, rel Release, asset string) (io.ReadCloser, error)
}

// Asset finds an asset by name.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Archive is the release archive for a platform, named as goreleaser does:
// backsight_<version>_<os>_<arch>.tar.gz (.zip on Windows).
func (r Release) Archive(goos, goarch string) (Asset, bool) {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return r.Asset("backsight_" + r.Version + "_" + goos + "_" + goarch + ext)
}

// WantPrereleases is the pre-release policy (decision 0067): shown when the
// user opted in, when the installed version is a pre-release (a dev build
// counts its base tag), or when it is not a version at all (an unstamped
// build).
func WantPrereleases(installed string, optIn bool) bool {
	v, err := Parse(installed)
	return optIn || err != nil || v.Prerelease()
}

// Newer returns the releases strictly newer than after, newest first.
// Pre-releases are dropped unless prereleases is set; releases whose version
// does not parse are always dropped. An after that is not a version (""
// or "dev") sets no lower bound.
func Newer(rels []Release, after string, prereleases bool) []Release {
	floor, ferr := Parse(after)
	type parsed struct {
		r Release
		v Version
	}
	var keep []parsed
	for _, r := range rels {
		v, err := Parse(r.Version)
		if err != nil || (!prereleases && (r.Prerelease || v.Prerelease())) {
			continue
		}
		if ferr == nil && Compare(v, floor) <= 0 {
			continue
		}
		keep = append(keep, parsed{r, v})
	}
	slices.SortStableFunc(keep, func(a, b parsed) int { return Compare(b.v, a.v) })
	out := make([]Release, 0, len(keep))
	for _, p := range keep {
		out = append(out, p.r)
	}
	return out
}

// NewerThan reports whether version v is newer than installed. A v that is
// not a version never is; an installed that is not one (an unstamped
// build) is older than every version.
func NewerThan(v, installed string) bool {
	pv, err := Parse(v)
	if err != nil {
		return false
	}
	pi, err := Parse(installed)
	return err != nil || Compare(pv, pi) > 0
}

// Find is the release whose version equals v (with or without the v).
func Find(rels []Release, v string) (Release, bool) {
	want, err := Parse(v)
	if err != nil {
		return Release{}, false
	}
	for _, r := range rels {
		if got, err := Parse(r.Version); err == nil && Compare(got, want) == 0 {
			return r, true
		}
	}
	return Release{}, false
}

const summaryChars = 80

var shaPrefix = regexp.MustCompile(`^[0-9a-f]{7,40} `)

// Summary is a one-line digest of release notes: the first entry of a
// goreleaser changelog (commit hash dropped) with the count of the others,
// or the first line of prose.
func Summary(notes string) string {
	var first string
	more := 0
	for _, line := range strings.Split(notes, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bullet := false
		for _, p := range []string{"* ", "- "} {
			if s, ok := strings.CutPrefix(line, p); ok {
				line, bullet = strings.TrimSpace(s), true
			}
		}
		switch {
		case first == "":
			first = shaPrefix.ReplaceAllString(line, "")
			if !bullet {
				return clip(first)
			}
		case bullet:
			more++
		}
	}
	if first == "" {
		return ""
	}
	first = clip(first)
	if more > 0 {
		first += " (+" + strconv.Itoa(more) + " more)"
	}
	return first
}

func clip(s string) string {
	r := []rune(s)
	if len(r) <= summaryChars {
		return s
	}
	return strings.TrimSpace(string(r[:summaryChars])) + "…"
}
