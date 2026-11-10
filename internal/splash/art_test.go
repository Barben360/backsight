package splash

import (
	"testing"
	"unicode/utf8"
)

func TestArtShapes(t *testing.T) {
	for name, art := range map[string][]ArtLine{"blocks": blocks, "braille": braille, "ascii": ascii} {
		if len(art) != 7 {
			t.Errorf("%s: %d rows, want 7", name, len(art))
		}
		for i, l := range art {
			if utf8.RuneCountInString(l.Mask) > utf8.RuneCountInString(l.Text) {
				t.Errorf("%s row %d: mask longer than text", name, i)
			}
			if utf8.RuneCountInString(l.Text) > logoColumn-2 {
				t.Errorf("%s row %d: %q wider than the logo column", name, i, l.Text)
			}
		}
	}
	for i, l := range ascii {
		for _, r := range l.Text {
			if r >= 0x80 {
				t.Errorf("ascii row %d holds %q", i, r)
			}
		}
	}
}

func TestLogoChoice(t *testing.T) {
	for _, c := range []struct {
		kind string
		utf8 bool
		want string
	}{
		{"blocks", true, "blocks"}, {"", true, "blocks"}, {"nope", true, "blocks"},
		{"braille", true, "braille"}, {"braille", false, "ascii"}, {"blocks", false, "ascii"},
	} {
		got := Logo(c.kind, c.utf8)
		want := map[string][]ArtLine{"blocks": blocks, "braille": braille, "ascii": ascii}[c.want]
		if &got[0] != &want[0] {
			t.Errorf("Logo(%q, %v) is not %s", c.kind, c.utf8, c.want)
		}
	}
}

func TestLocaleUTF8(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{"LANG": "en_US.UTF-8"}, true},
		{map[string]string{"LANG": "C.utf8"}, true},
		{map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, false},
		{map[string]string{"LC_CTYPE": "fr_FR.UTF-8", "LANG": "C"}, true},
		{map[string]string{"LANG": "POSIX"}, false},
		{map[string]string{}, false},
	} {
		if got := localeUTF8(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("localeUTF8(%v) = %v, want %v", c.env, got, c.want)
		}
	}
}
