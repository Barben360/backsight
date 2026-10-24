package config

import (
	"strconv"
	"strings"
)

// SetClientRetention returns raw with [client] retention_days set to days
// (0 removes the key), editing lines in place so the user's comments and
// layout stay (decision 0052).
func SetClientRetention(raw []byte, days int) []byte {
	lines := strings.SplitAfter(string(raw), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	entry := "retention_days = " + strconv.Itoa(days) + "\n"
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
				return []byte(strings.Join(append(lines[:i:i], lines[i+1:]...), ""))
			}
			lines[i] = entry
			return []byte(strings.Join(lines, ""))
		}
	}
	if days == 0 {
		return raw
	}
	if header >= 0 {
		out := append(append(append([]string{}, lines[:header+1]...), entry), lines[header+1:]...)
		return []byte(strings.Join(out, ""))
	}
	s := string(raw)
	switch {
	case strings.TrimSpace(s) == "":
		s = ""
	case !strings.HasSuffix(s, "\n"):
		s += "\n\n"
	default:
		s += "\n"
	}
	return []byte(s + "[client]\n" + entry)
}
