package fileimport

import (
	"bytes"
	"strings"
	"time"
)

const maxFrontmatterLines = 64

var bom = []byte("\xef\xbb\xbf")

// frontmatter returns the lines of a block that starts at byte 0 (after an
// optional BOM) with a line "---" and ends at the next line "---" or "...",
// at most maxFrontmatterLines inside, and the content after it.
func frontmatter(content []byte) (lines []string, rest []byte, ok bool) {
	b := bytes.TrimPrefix(content, bom)
	first, after, found := bytes.Cut(b, []byte("\n"))
	if !found || strings.TrimRight(string(first), "\r") != "---" {
		return nil, content, false
	}
	for n := 0; n <= maxFrontmatterLines; n++ {
		line, next, more := bytes.Cut(after, []byte("\n"))
		l := strings.TrimRight(string(line), "\r")
		if l == "---" || l == "..." {
			return lines, next, true
		}
		if !more || n == maxFrontmatterLines {
			return nil, content, false
		}
		lines = append(lines, l)
		after = next
	}
	return nil, content, false
}

// stripFrontmatter is the content after the frontmatter block, or the
// content itself when there is none.
func stripFrontmatter(content []byte) []byte {
	_, rest, _ := frontmatter(content)
	return rest
}

var dateLayouts = []struct {
	layout string
	local  bool
}{
	{time.RFC3339, false},
	{"2006-01-02T15:04:05", true},
	{"2006-01-02 15:04:05", true},
	{"2006-01-02 15:04", true},
	{"2006-01-02", true},
}

// FrontmatterDate is the frontmatter's date, else its created, as UTC. A
// value after now + 24 h, before 1970 or unparsable does not count.
func FrontmatterDate(content []byte, now time.Time) (time.Time, bool) {
	lines, _, ok := frontmatter(content)
	if !ok {
		return time.Time{}, false
	}
	values := map[string]string{}
	for _, l := range lines {
		k, v, found := strings.Cut(l, ":")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		if _, seen := values[k]; !seen {
			values[k] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	for _, key := range []string{"date", "created"} {
		if v, ok := values[key]; ok {
			if t, ok := parseDate(v, now); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func parseDate(v string, now time.Time) (time.Time, bool) {
	for _, l := range dateLayouts {
		var t time.Time
		var err error
		if l.local {
			t, err = time.ParseInLocation(l.layout, v, time.Local)
		} else {
			t, err = time.Parse(l.layout, v)
		}
		if err != nil {
			continue
		}
		t = t.UTC()
		if t.Before(time.Unix(0, 0)) || t.After(now.Add(24*time.Hour)) {
			return time.Time{}, false
		}
		return t, true
	}
	return time.Time{}, false
}
