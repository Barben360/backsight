package config

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Claude Code's table, and its name in a file written before the per-tool
// tables.
const (
	claudeCodeTable   = "clients.claude_code"
	legacyClientTable = "client"
)

// SetClientRetention returns raw with [clients.claude_code] retention_days
// set to days (0 removes the key), editing lines in place so the user's
// comments, layout and line endings stay (decision 0052). An older file's
// [client] table moves first (MigrateClientTable).
func SetClientRetention(raw []byte, days int) ([]byte, error) {
	out, _, err := MigrateClientTable(raw)
	if err != nil {
		return nil, err
	}
	value := ""
	if days > 0 {
		value = strconv.Itoa(days)
	}
	return setKey(out, claudeCodeTable, "retention_days", value)
}

// MigrateClientTable returns raw with the [client] table of an older file
// moved to [clients.claude_code], line by line so the user's comments,
// layout and line endings stay. Without a [clients.claude_code] table, the
// [client] header is renamed in place; with one, retention_days moves into
// it (a second, different value is refused) and the [client] header goes
// once only blank lines are left under it. changed is false for a file
// without a [client] table. Forms a line edit cannot change safely are
// refused: a dotted key, an inline table, a multi-line string.
func MigrateClientTable(raw []byte) (out []byte, changed bool, err error) {
	refused := errors.New("config.toml sets [" + legacyClientTable + "] in a form backsight cannot edit (dotted key, inline table or multi-line string): move it to [" + claudeCodeTable + "] by hand")
	if regexp.MustCompile(`(?m)^\s*` + legacyClientTable + `\s*[.=]`).Match(raw) {
		return nil, false, refused
	}
	lines := splitLines(string(raw))
	legacy, current := -1, -1
	for i, l := range lines {
		switch headerName(l) {
		case legacyClientTable:
			legacy = i
		case claudeCodeTable:
			current = i
		}
	}
	if legacy < 0 {
		return raw, false, nil
	}
	if regexp.MustCompile(`"""|'''`).Match(raw) {
		return nil, false, refused
	}
	if current < 0 {
		l := lines[legacy]
		open := strings.Index(l, "[")
		end := open + strings.Index(l[open:], "]")
		lines[legacy] = l[:open+1] + claudeCodeTable + l[end:]
		return []byte(strings.Join(lines, "")), true, nil
	}

	// Both tables: retention_days, the only key [client] ever held, moves.
	sectionEnd := func(h int) int {
		for i := h + 1; i < len(lines); i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
				return i
			}
		}
		return len(lines)
	}
	keyLine := func(h int) (int, string) {
		for i := h + 1; i < sectionEnd(h); i++ {
			t := strings.TrimSpace(lines[i])
			if strings.HasPrefix(t, "retention_days ") || strings.HasPrefix(t, "retention_days=") {
				_, rest, _ := strings.Cut(t, "=")
				rest = strings.TrimSpace(rest)
				end, _ := valueEnd(rest)
				return i, strings.TrimSpace(rest[:end])
			}
		}
		return -1, ""
	}
	oldAt, oldValue := keyLine(legacy)
	newAt, newValue := keyLine(current)
	var moved []string
	drop := map[int]bool{}
	if oldAt >= 0 {
		drop[oldAt] = true
		switch {
		case newAt < 0:
			moved = append(moved, strings.TrimLeft(lines[oldAt], " \t"))
			if !strings.HasSuffix(moved[0], "\n") {
				moved[0] += lineEnding(string(raw))
			}
		case !sameInt(oldValue, newValue):
			return nil, false, errors.New("config.toml sets " + LegacyRetentionKey + " = " + oldValue + " and " + RetentionKey + " = " + newValue + ": keep " + RetentionKey + " only, by hand")
		}
	}
	empty := true
	for i := legacy + 1; i < sectionEnd(legacy); i++ {
		if !drop[i] && strings.TrimSpace(lines[i]) != "" {
			empty = false
		}
	}
	if empty {
		drop[legacy] = true
	}
	var res []string
	for i, l := range lines {
		if !drop[i] {
			res = append(res, l)
		}
		if i == current {
			res = append(res, moved...)
		}
	}
	return []byte(strings.Join(res, "")), true, nil
}

// splitLines splits s after each "\n", without an empty last line.
func splitLines(s string) []string {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// headerName is the table a "[table]" header line opens, "" for any other
// line (an array of tables included).
func headerName(l string) string {
	t := strings.TrimSpace(l)
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") {
		return ""
	}
	return strings.TrimSpace(strings.Trim(strings.SplitN(t, "#", 2)[0], " []"))
}

func lineEnding(s string) string {
	if strings.Contains(s, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// sameInt reports whether two TOML integer literals are one number.
func sameInt(a, b string) bool {
	var x, y struct {
		V int `toml:"v"`
	}
	if toml.Unmarshal([]byte("v = "+a), &x) != nil || toml.Unmarshal([]byte("v = "+b), &y) != nil {
		return a == b
	}
	return x.V == y.V
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

// SetServerBank returns raw with [server] bank set to bank.
func SetServerBank(raw []byte, bank string) ([]byte, error) {
	return setKey(raw, "server", "bank", strconv.Quote(bank))
}

// SetRemote returns raw as a client of the server at url, bank bank
// ([server] url, bank and mode = "remote"), as connect writes it.
func SetRemote(raw []byte, url, bank string) ([]byte, error) {
	out := raw
	var err error
	// Each new key goes right under the table header: url, bank, then
	// mode reads mode, bank, url.
	for _, kv := range [][2]string{{"url", url}, {"bank", bank}, {"mode", "remote"}} {
		if out, err = setKey(out, "server", kv[0], strconv.Quote(kv[1])); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetLLMEstimate returns raw with the [llm] keys a client prices its
// backfill estimate with: the host's provider and model, its prices per
// million tokens and their currency. An empty value or a zero price
// removes its key (the default applies); a negative or non-finite price
// is refused.
func SetLLMEstimate(raw []byte, provider, model, currency string, input, cached, output float64) ([]byte, error) {
	price := func(v float64) (string, error) {
		switch {
		case math.IsNaN(v) || math.IsInf(v, 0) || v < 0:
			return "", errors.New("an LLM price must be a number ≥ 0")
		case v == 0:
			return "", nil
		}
		s := strconv.FormatFloat(v, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0" // a TOML float, not an integer
		}
		return s, nil
	}
	quoted := func(s string) string {
		if s == "" {
			return ""
		}
		return strconv.Quote(s)
	}
	keys := [][2]string{{"provider", quoted(provider)}, {"model", quoted(model)}}
	for _, p := range []struct {
		key string
		v   float64
	}{{"price_input", input}, {"price_cached", cached}, {"price_output", output}} {
		s, err := price(p.v)
		if err != nil {
			return nil, err
		}
		keys = append(keys, [2]string{p.key, s})
	}
	keys = append(keys, [2]string{"currency", quoted(currency)})
	out := raw
	var err error
	// Backwards: each new key goes right under the table header.
	for i := len(keys) - 1; i >= 0; i-- {
		if out, err = setKey(out, "llm", keys[i][0], keys[i][1]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetNetwork returns raw with [network] exposure set, and bind_ip set to
// bindIP ("" removes it: a loopback server has none), as server expose
// changes them.
func SetNetwork(raw []byte, exposure, bindIP string) ([]byte, error) {
	value := ""
	if bindIP != "" {
		value = strconv.Quote(bindIP)
	}
	out, err := setKey(raw, "network", "bind_ip", value)
	if err != nil {
		return nil, err
	}
	return setKey(out, "network", "exposure", strconv.Quote(exposure))
}

// SetRuntime returns raw with [server] runtime set to kind: "native" is
// written, "docker" removes the key (the default), so a Docker install's
// file stays as it was.
func SetRuntime(raw []byte, kind string) ([]byte, error) {
	switch kind {
	case "docker":
		return setKey(raw, "server", "runtime", "")
	case "native":
		return setKey(raw, "server", "runtime", strconv.Quote(kind))
	}
	return nil, errors.New("server.runtime: " + strconv.Quote(kind) + " is not docker or native")
}

// SetStartOnSession returns raw with [server] start_on_session set to on:
// true is written, false removes the key (the default).
func SetStartOnSession(raw []byte, on bool) ([]byte, error) {
	if on {
		return setKey(raw, "server", "start_on_session", "true")
	}
	return setKey(raw, "server", "start_on_session", "")
}

// valueEnd is where a one-line TOML value ends in s (strings and arrays
// read with their quotes, so a "#" or "]" inside a string counts for
// nothing); ok false when an array does not close on this line.
func valueEnd(s string) (int, bool) {
	depth, quote := 0, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		case c == '#' && depth == 0:
			return i, true
		}
	}
	return len(s), depth == 0
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
			end, ok := valueEnd(strings.TrimSpace(rest))
			if !ok {
				return nil, errors.New("config.toml spreads " + table + "." + key + " over several lines: put it on one line or set it by hand")
			}
			if value == "" {
				return []byte(strings.Join(append(lines[:i:i], lines[i+1:]...), "")), nil
			}
			comment := ""
			if c := strings.TrimSpace(strings.TrimSpace(rest)[end:]); strings.HasPrefix(c, "#") {
				comment = " " + c
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
