package fileimport

import (
	"strings"
	"testing"
	"time"
)

func TestFrontmatterDate(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	local := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.Local).UTC()
	}
	cases := []struct {
		name, content string
		want          time.Time
		ok            bool
	}{
		{"date only", "---\ndate: 2025-03-04\n---\nbody", local(2025, 3, 4), true},
		{"created rfc3339 quoted", "---\ncreated: \"2025-03-04T10:00:00Z\"\n---\nbody", time.Date(2025, 3, 4, 10, 0, 0, 0, time.UTC), true},
		{"single quotes", "---\ndate: '2025-03-04'\n---\n", local(2025, 3, 4), true},
		{"date wins over created", "---\ncreated: 2020-01-01\ndate: 2025-03-04\n---\n", local(2025, 3, 4), true},
		{"created when date unparsable", "---\ndate: someday\ncreated: 2020-01-01\n---\n", local(2020, 1, 1), true},
		{"datetime with space", "---\ndate: 2025-03-04 10:30\n---\n", time.Date(2025, 3, 4, 10, 30, 0, 0, time.Local).UTC(), true},
		{"future beyond a day", "---\ndate: 2026-11-04\n---\n", time.Time{}, false},
		{"before 1970", "---\ndate: 1969-12-31\n---\n", time.Time{}, false},
		{"unparsable", "---\ndate: tomorrow\n---\n", time.Time{}, false},
		{"no frontmatter", "date: 2025-03-04\n", time.Time{}, false},
		{"not at byte 0", "\n---\ndate: 2025-03-04\n---\n", time.Time{}, false},
		{"unterminated", "---\ndate: 2025-03-04\nbody", time.Time{}, false},
		{"crlf", "---\r\ndate: 2025-03-04\r\n---\r\nbody", local(2025, 3, 4), true},
		{"bom", "\xef\xbb\xbf---\ndate: 2025-03-04\n---\n", local(2025, 3, 4), true},
		{"dots close", "---\ndate: 2025-03-04\n...\n", local(2025, 3, 4), true},
		{"too long", "---\n" + strings.Repeat("k: v\n", 64) + "date: 2025-03-04\n---\n", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := FrontmatterDate([]byte(c.content), now)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("%s: FrontmatterDate = %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestStripFrontmatter(t *testing.T) {
	if got := stripFrontmatter([]byte("---\ntype: person\n---\n\nhello\n")); strings.TrimSpace(string(got)) != "hello" {
		t.Errorf("stripFrontmatter kept %q", got)
	}
	if got := stripFrontmatter([]byte("no front\n---\n")); string(got) != "no front\n---\n" {
		t.Errorf("stripFrontmatter changed a file without frontmatter: %q", got)
	}
}
