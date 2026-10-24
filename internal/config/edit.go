package config

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// otherForms are ways to write [client] keys that a line edit cannot change
// safely: dotted top-level keys, an inline table, multi-line strings (a
// "[client]" line inside one is text, not a table).
var otherForms = regexp.MustCompile(`(?m)^\s*client\s*[.=]|"""|'''`)

// SetClientRetention returns raw with [client] retention_days set to days
// (0 removes the key), editing lines in place so the user's comments,
// layout and line endings stay (decision 0052).
func SetClientRetention(raw []byte, days int) ([]byte, error) {
	if otherForms.Match(raw) {
		return nil, errors.New("config.toml sets [client] in a form backsight cannot edit (dotted key, inline table or multi-line string): set client.retention_days by hand")
	}
	s := string(raw)
	nl := "\n"
	if strings.Contains(s, "\r\n") {
		nl = "\r\n"
	}
	lines := strings.SplitAfter(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	entry := "retention_days = " + strconv.Itoa(days)
	section := ""
	header := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			section = strings.TrimSpace(strings.Trim(strings.SplitN(t, "#", 2)[0], " []"))
			if section == "client" {
				header = i
			}
			continue
		}
		if section == "client" && (strings.HasPrefix(t, "retention_days ") || strings.HasPrefix(t, "retention_days=")) {
			if days == 0 {
				return []byte(strings.Join(append(lines[:i:i], lines[i+1:]...), "")), nil
			}
			comment := ""
			if _, c, ok := strings.Cut(t, "#"); ok {
				comment = " #" + c
			}
			lines[i] = entry + comment + nl
			return []byte(strings.Join(lines, "")), nil
		}
	}
	if days == 0 {
		return raw, nil
	}
	if header >= 0 {
		out := append(append(append([]string{}, lines[:header+1]...), entry+nl), lines[header+1:]...)
		return []byte(strings.Join(out, "")), nil
	}
	switch {
	case strings.TrimSpace(s) == "":
		s = ""
	case !strings.HasSuffix(s, "\n"):
		s += nl + nl
	default:
		s += nl
	}
	return []byte(s + "[client]" + nl + entry + nl), nil
}
