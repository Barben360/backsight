package release

import "testing"

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"v0.1.0", "0.1.0", 0}, // the tag's v is not part of the version
		{"0.1.0", "0.1.1", -1},
		{"0.2.0", "0.1.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.10.0", "0.9.0", 1}, // numeric, not lexical
		{"0.1.0-m1", "0.1.0", -1},
		{"0.1.0-m1", "0.1.0-m0", 1},
		{"0.1.0-rc.2", "0.1.0-rc.10", -1}, // numeric identifiers compare as numbers
		{"0.1.0-1", "0.1.0-alpha", -1},    // numeric below alphanumeric
		{"0.1.0-alpha", "0.1.0-alpha.1", -1},
		{"0.0.3-snapshot+1300233", "0.0.3-snapshot+abcdef0", 0}, // build metadata ignored
		// git describe: a dev build sits just past its base tag.
		{"v0.0.2-m0-93-gfd77ae6", "0.0.2-m0", 1},
		{"v0.0.2-m0-93-gfd77ae6", "0.0.2", -1},
		{"v0.0.2-m0-93-gfd77ae6", "0.0.3-m1", -1},
		{"v0.1.0-5-gabc1234", "0.1.0", 1}, // past a stable tag, not a pre-release of it
		{"v0.1.0-5-gabc1234", "0.1.1", -1},
		{"v0.0.2-m0-93-gfd77ae6", "v0.0.2-m0-94-g1234567", -1},
		{"v0.0.2-m0-dirty", "0.0.2-m0", 1},
		{"v0.0.2-m0-93-gfd77ae6-dirty", "v0.0.2-m0-93-gfd77ae6", 1},
	} {
		a, err := Parse(c.a)
		if err != nil {
			t.Fatalf("%s: %v", c.a, err)
		}
		b, err := Parse(c.b)
		if err != nil {
			t.Fatalf("%s: %v", c.b, err)
		}
		if got := Compare(a, b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := Compare(b, a); got != -c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestParseRefusesNonVersions(t *testing.T) {
	for _, s := range []string{"", "dev", "fd77ae6", "none", "1.2", "1.2.3.4", "01.2.3", "1.2.3-", "1.2.3-a..b", "v", "1.2.x"} {
		if v, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", s, v)
		}
	}
}

func TestPrereleaseAndDevFlags(t *testing.T) {
	for _, c := range []struct {
		s        string
		pre, dev bool
		str      string
	}{
		{"v0.1.0", false, false, "0.1.0"},
		{"0.1.0-m1", true, false, "0.1.0-m1"},
		{"0.0.3-snapshot+1300233", true, false, "0.0.3-snapshot+1300233"},
		{"v0.0.2-m0-93-gfd77ae6", true, true, "0.0.2-m0-93-gfd77ae6"}, // its base is a pre-release
		{"v0.1.0-5-gabc1234", false, true, "0.1.0-5-gabc1234"},
		{"v0.1.0-dirty", false, true, "0.1.0-dirty"},
	} {
		v, err := Parse(c.s)
		if err != nil {
			t.Fatalf("%s: %v", c.s, err)
		}
		if v.Prerelease() != c.pre || v.Dev() != c.dev || v.String() != c.str {
			t.Errorf("%s: prerelease %v dev %v string %q, want %v %v %q", c.s, v.Prerelease(), v.Dev(), v.String(), c.pre, c.dev, c.str)
		}
	}
}
