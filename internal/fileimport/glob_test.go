package fileimport

import "testing"

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, rel string
		want         bool
	}{
		{"**/_index.md", "_index.md", true},
		{"**/_index.md", "a/b/_index.md", true},
		{"**/_index.md", "a/b/index.md", false},
		{"KNOWLEDGE/_archive/**", "KNOWLEDGE/_archive/x.md", true},
		{"KNOWLEDGE/_archive/**", "KNOWLEDGE/_archive", true},
		{"KNOWLEDGE/_archive/**", "KNOWLEDGE/_archived/x.md", false},
		{"*.md", "a.md", true},
		{"*.md", "d/a.md", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"logs/**", "logs", true},
		{"logs/**", "logsx/a.md", false},
		{"?.txt", "a.txt", true},
		{"[ab].txt", "c.txt", false},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pattern, c.rel); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pattern, c.rel, got, c.want)
		}
	}
}

func TestValidGlob(t *testing.T) {
	for _, p := range []string{"**/_index.md", "a/*.md", "[ab]/x"} {
		if err := ValidGlob(p); err != nil {
			t.Errorf("ValidGlob(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"[", "a/[b", ""} {
		if err := ValidGlob(p); err == nil {
			t.Errorf("ValidGlob(%q) = nil, want an error", p)
		}
	}
}
