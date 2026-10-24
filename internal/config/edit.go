package config

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// SetClientRetention returns raw with [client] retention_days set to days
// (0 removes the key), editing lines in place so the user's comments,
// layout and line endings stay (decision 0052).
func SetClientRetention(raw []byte, days int) ([]byte, error) {
	value := ""
	if days > 0 {
		value = strconv.Itoa(days)
	}
	return setKey(raw, "client", "retention_days", value)
}

// SetExcludePaths returns raw with [privacy] exclude_paths set to paths,
// on one line (pause --project, decision 0056).
func SetExcludePaths(raw []byte, paths []string) ([]byte, error) {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = strconv.Quote(p)
	}
	return setKey(raw, "privacy", "exclude_paths", "["+strings.Join(quoted, ", ")+"]")
}

// setKey sets key = value in [table] ("" removes the key), line by line.
// Forms a line edit cannot change safely are refused: dotted keys, inline
// tables, multi-line strings, and a multi-line value for the key itself.
func setKey(raw []byte, table, key, value string) ([]byte, error) {
	other := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(table) + `\s*[.=]|"""|'''`)
	if other.Match(raw) {
		return nil, errors.New("config.toml sets [" + table + "] in a form backsight cannot edit (dotted key, inline table or multi-line string): set " + table + "." + key + " by hand")
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
	entry := key + " = " + value
	section := ""
	header := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			section = strings.TrimSpace(strings.Trim(strings.SplitN(t, "#", 2)[0], " []"))
			if section == table {
				header = i
			}
			continue
		}
		if section == table && (strings.HasPrefix(t, key+" ") || strings.HasPrefix(t, key+"=")) {
			_, rest, _ := strings.Cut(t, "=")
			rest = strings.TrimSpace(rest)
			if strings.HasPrefix(rest, "[") && !strings.Contains(strings.SplitN(rest, "#", 2)[0], "]") {
				return nil, errors.New("config.toml spreads " + table + "." + key + " over several lines: put it on one line or set it by hand")
			}
			if value == "" {
				return []byte(strings.Join(append(lines[:i:i], lines[i+1:]...), "")), nil
			}
			comment := ""
			if j := strings.LastIndex(rest, "#"); j >= 0 && !strings.Contains(rest[j:], `"`) {
				comment = " " + rest[j:]
			}
			lines[i] = entry + comment + nl
			return []byte(strings.Join(lines, "")), nil
		}
	}
	if value == "" {
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
	return []byte(s + "[" + table + "]" + nl + entry + nl), nil
}
