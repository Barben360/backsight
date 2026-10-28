package config

import (
	"errors"
	"math"
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
