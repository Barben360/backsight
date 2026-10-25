package report

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Barben360/backsight/internal/redact"
)

// MaxField caps every string of the diagnostic, in runes.
const MaxField = 400

// httpBody is the server's answer quoted in an error (hindsight.APIError
// and its wrappers): a validation error echoes the request, which may hold
// memory text or a prompt, so everything after the status goes.
var httpBody = regexp.MustCompile(`(HTTP \d{3}(?: \([a-z_]+\))?:) .*`)

// Scrubber removes from text what a report must never show: secrets (the
// redaction rules), server answers, the home directory (shown as ~) and
// the names that identify the user (machine, bank, remote server host).
type Scrubber struct {
	home  []string
	names []name
}

type name struct {
	re   *regexp.Regexp
	with string
}

// NewScrubber scrubs home and its resolved form.
func NewScrubber(home string) *Scrubber {
	s := &Scrubber{}
	seen := map[string]bool{}
	add := func(h string) {
		h = strings.TrimRight(h, `/\`)
		if len(h) < 2 || seen[h] {
			return
		}
		seen[h] = true
		s.home = append(s.home, h)
		if fs := filepath.ToSlash(h); fs != h && !seen[fs] {
			seen[fs] = true
			s.home = append(s.home, fs)
		}
	}
	if home != "" {
		add(home)
		if real, err := filepath.EvalSymlinks(home); err == nil {
			add(real)
		}
	}
	return s
}

// Name replaces every whole-word occurrence of v with placeholder (a
// machine name, a bank, a remote server host). Values of fewer than 3
// characters are ignored: they would eat ordinary words.
func (s *Scrubber) Name(v, placeholder string) {
	if len(v) < 3 {
		return
	}
	s.names = append(s.names, name{regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(v) + `($|[^A-Za-z0-9_-])`), "${1}" + placeholder + "${2}"})
}

// String scrubs one value.
func (s *Scrubber) String(v string) string {
	v = httpBody.ReplaceAllString(v, "$1 [server answer omitted]")
	for _, h := range s.home {
		v = replacePrefix(v, h)
	}
	v, _ = redact.Text(v)
	for _, n := range s.names {
		// Twice: adjacent occurrences share their separator.
		v = n.re.ReplaceAllString(n.re.ReplaceAllString(v, n.with), n.with)
	}
	return truncate(v, MaxField)
}

// replacePrefix shows home as ~ wherever it starts a path; /home/ann is
// kept inside /home/anna.
func replacePrefix(v, home string) string {
	var b strings.Builder
	for {
		i := strings.Index(v, home)
		if i < 0 {
			b.WriteString(v)
			return b.String()
		}
		end := i + len(home)
		b.WriteString(v[:i])
		if end < len(v) && wordByte(v[end]) {
			b.WriteString(home)
		} else {
			b.WriteString("~")
		}
		v = v[end:]
	}
}

func wordByte(c byte) bool {
	return c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// Diagnostic scrubs every string of d, map keys included, through its JSON
// form: a field added later is scrubbed without anyone remembering to.
func (s *Scrubber) Diagnostic(d Diagnostic) (Diagnostic, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return d, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return d, err
	}
	if raw, err = json.Marshal(s.walk(tree)); err != nil {
		return d, err
	}
	var out Diagnostic
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (s *Scrubber) walk(v any) any {
	switch t := v.(type) {
	case string:
		return s.String(t)
	case []any:
		for i := range t {
			t[i] = s.walk(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[s.String(k)] = s.walk(x)
		}
		return out
	}
	return v
}
