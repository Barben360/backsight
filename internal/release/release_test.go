package release

import (
	"strings"
	"testing"
)

func rel(v string, pre bool) Release { return Release{Version: v, Tag: "v" + v, Prerelease: pre} }

func versions(rs []Release) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Version)
	}
	return strings.Join(s, " ")
}

func TestNewerKeepsOnlyLaterVersionsNewestFirst(t *testing.T) {
	rs := []Release{rel("0.1.0", false), rel("0.3.0", false), rel("0.2.0", false), rel("garbage", false)}
	for _, c := range []struct{ after, want string }{
		{"0.1.0", "0.3.0 0.2.0"}, // equal is not newer
		{"v0.2.0", "0.3.0"},
		{"0.3.0", ""},
		{"0.4.0", ""}, // installed ahead of every release
		{"v0.2.0-7-gabc1234", "0.3.0"},
		{"dev", "0.3.0 0.2.0 0.1.0"}, // not a version: no lower bound
		{"", "0.3.0 0.2.0 0.1.0"},
	} {
		if got := versions(Newer(rs, c.after, false)); got != c.want {
			t.Errorf("after %q: %q, want %q", c.after, got, c.want)
		}
	}
}

func TestNewerHidesPrereleasesUnlessAsked(t *testing.T) {
	rs := []Release{rel("0.2.0-m1", false), rel("0.1.1", true), rel("0.1.0", false), rel("0.2.0", false)}
	if got := versions(Newer(rs, "0.0.1", false)); got != "0.2.0 0.1.0" {
		t.Errorf("stable only: %q", got) // the semver suffix and the source's flag both mark one
	}
	if got := versions(Newer(rs, "0.0.1", true)); got != "0.2.0 0.2.0-m1 0.1.1 0.1.0" {
		t.Errorf("with pre-releases: %q", got)
	}
}

func TestWantPrereleases(t *testing.T) {
	for _, c := range []struct {
		installed string
		optIn     bool
		want      bool
	}{
		{"0.1.0", false, false},
		{"0.1.0", true, true},
		{"0.1.0-m1", false, true},
		{"v0.0.2-m0-93-gfd77ae6", false, true},
		{"v0.1.0-3-gabc1234", false, false},
		{"dev", false, true},
	} {
		if got := WantPrereleases(c.installed, c.optIn); got != c.want {
			t.Errorf("%q opt-in %v: %v", c.installed, c.optIn, got)
		}
	}
}

func TestSummary(t *testing.T) {
	notes := "## Changelog\n\n* ef1de6b9875108f01eda3b8aade21c303b775e9b ✅ Close the remaining M0 test gaps\n* 3aad77f 🔒️ Follow symlinks\n* 9259c2d 🔒️ Harden the client\n"
	if got := Summary(notes); got != "✅ Close the remaining M0 test gaps (+2 more)" {
		t.Errorf("%q", got)
	}
	if got := Summary("Fixes the backup prune.\n\nMore text."); got != "Fixes the backup prune." {
		t.Errorf("prose: %q", got)
	}
	if got := Summary(""); got != "" {
		t.Errorf("empty: %q", got)
	}
	long := "- " + strings.Repeat("word ", 40)
	if got := Summary(long); len([]rune(got)) > 81 || !strings.HasSuffix(got, "…") {
		t.Errorf("long: %q", got)
	}
}

func TestArchiveAndAsset(t *testing.T) {
	r := Release{Version: "0.1.0", Assets: []Asset{{Name: "checksums.txt"}, {Name: "backsight_0.1.0_linux_amd64.tar.gz"},
		{Name: "backsight_0.1.0_linux_amd64.tar.gz.minisig"}, {Name: "backsight_0.1.0_windows_amd64.zip"}}}
	if a, ok := r.Archive("linux", "amd64"); !ok || a.Name != "backsight_0.1.0_linux_amd64.tar.gz" {
		t.Errorf("linux: %+v %v", a, ok)
	}
	if a, ok := r.Archive("windows", "amd64"); !ok || a.Name != "backsight_0.1.0_windows_amd64.zip" {
		t.Errorf("windows: %+v %v", a, ok)
	}
	if _, ok := r.Archive("darwin", "arm64"); ok {
		t.Error("darwin found without its archive")
	}
	if _, ok := r.Asset("checksums.txt.minisig"); ok {
		t.Error("absent asset found")
	}
	if a, ok := r.Asset("checksums.txt"); !ok || a.Name != "checksums.txt" {
		t.Error("checksums.txt not found")
	}
}

func TestFind(t *testing.T) {
	rs := []Release{rel("0.1.0", false), rel("0.2.0-m1", true)}
	if r, ok := Find(rs, "v0.2.0-m1"); !ok || r.Version != "0.2.0-m1" {
		t.Errorf("%+v %v", r, ok)
	}
	if _, ok := Find(rs, "0.2.0"); ok {
		t.Error("0.2.0 found")
	}
}

func TestNewerThan(t *testing.T) {
	for _, c := range []struct {
		v, installed string
		want         bool
	}{
		{"0.2.0", "0.1.0", true},
		{"0.2.0", "0.2.0", false},
		{"0.2.0", "v0.2.0-1-gabc1234", false},
		{"0.1.0", "0.2.0", false},
		{"0.2.0", "dev", true},
		{"", "0.1.0", false},
		{"garbage", "dev", false},
	} {
		if got := NewerThan(c.v, c.installed); got != c.want {
			t.Errorf("NewerThan(%q, %q) = %v", c.v, c.installed, got)
		}
	}
}
