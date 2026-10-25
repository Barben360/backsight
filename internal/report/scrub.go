package report

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/redact"
)

// MaxField caps every string of the diagnostic, in runes.
const MaxField = 400

// httpBody is the server's answer quoted in an error (hindsight.APIError
// and its wrappers): a validation error echoes the request, which may hold
// memory text or a prompt, so everything after the status goes, the lines
// after a raw newline included.
var httpBody = regexp.MustCompile(`(?s)(HTTP \d{3}(?: \([a-z_]+\))?:) .*`)

// SecretPlaceholder stands for a value read from a secret store.
const SecretPlaceholder = "[redacted:secret]"

// minSecret is the shortest secret value scrubbed: a shorter one would eat
// ordinary words, and backsight's own secrets are 32 random bytes.
const minSecret = 8

// Scrubber removes from text what a report must never show: secrets (the
// redaction rules and the values of the secret stores), server answers,
// the directories that name the user (home shown as ~, the other roots as
// placeholders) and the names that identify the user (machine, bank,
// remote server host, OS user name).
type Scrubber struct {
	home     []string
	prefixes []prefix // home and the roots, longest first
	secrets  []string // longest first
	names    []name
}

type prefix struct{ path, with string }

type name struct {
	re   *regexp.Regexp
	with string
}

// NewScrubber scrubs home and its resolved form.
func NewScrubber(home string) *Scrubber {
	s := &Scrubber{}
	for _, h := range pathForms(home) {
		s.home = append(s.home, h)
		s.addPrefix(h, "~")
	}
	return s
}

// pathForms are p, its resolved form and their forward-slash spellings.
func pathForms(p string) []string {
	if p == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		h = strings.TrimRight(h, `/\`)
		for _, f := range []string{h, filepath.ToSlash(h)} {
			if len(f) >= 2 && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	add(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		add(real)
	}
	return out
}

func (s *Scrubber) addPrefix(p, with string) {
	s.prefixes = append(s.prefixes, prefix{p, with})
	sort.SliceStable(s.prefixes, func(i, j int) bool { return len(s.prefixes[i].path) > len(s.prefixes[j].path) })
}

// Root shows dir as placeholder wherever it starts a path (backsight's
// data, config, state and binary roots, the backup directory). A root at
// or under the home directory is left to the home's ~.
func (s *Scrubber) Root(dir, placeholder string) {
	forms := pathForms(dir)
	for _, f := range forms {
		for _, h := range s.home {
			if under(f, h) {
				return
			}
		}
	}
	for _, f := range forms {
		s.addPrefix(f, placeholder)
	}
}

// under reports whether p is dir or a path inside it.
func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/") || strings.HasPrefix(p, dir+`\`)
}

// Secret replaces v wherever it shows, in its URL-escaped forms too: a
// value from secrets.toml or the server's .env, which may follow no
// pattern the redaction rules know. Values shorter than minSecret are
// ignored.
func (s *Scrubber) Secret(v string) {
	if len(v) < minSecret {
		return
	}
	for _, f := range encodings(v) {
		if !slices.Contains(s.secrets, f) {
			s.secrets = append(s.secrets, f)
		}
	}
	sort.SliceStable(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
}

// encodings are v and its URL-escaped forms: query, path, and the path
// form of hindsight's bankPath, which escapes ':' too.
func encodings(v string) []string {
	out := []string{v}
	for _, f := range []string{url.QueryEscape(v), url.PathEscape(v), strings.ReplaceAll(url.PathEscape(v), ":", "%3A")} {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// minName is the shortest name replaced outside a path segment.
const minName = 4

// Name replaces v with placeholder (a machine name, a bank, a remote
// server host, the OS user name), in its URL-escaped forms too, wherever
// no letter or digit touches it: `-`, `_` and `.` are boundaries, so
// backsight-annsbox and annsbox-2 lose the name, but annsboxer keeps anns
// (privacy over readability, decision 0067 block 3). A value of fewer than
// minName characters is replaced only as a whole path segment: anywhere
// else it would eat ordinary words.
func (s *Scrubber) Name(v, placeholder string) {
	if v == "" {
		return
	}
	before, after := `(^|[^A-Za-z0-9])`, `($|[^A-Za-z0-9])`
	if len(v) < minName {
		before, after = `(^|[/\\])`, `($|[/\\])`
	}
	for _, f := range encodings(v) {
		s.names = append(s.names, name{regexp.MustCompile(before + regexp.QuoteMeta(f) + after), "${1}" + placeholder + "${2}"})
	}
}

// String scrubs one value.
func (s *Scrubber) String(v string) string {
	v = httpBody.ReplaceAllString(v, "$1 [server answer omitted]")
	for _, sec := range s.secrets {
		v = strings.ReplaceAll(v, sec, SecretPlaceholder)
	}
	for _, p := range s.prefixes {
		v = replacePrefix(v, p.path, p.with)
	}
	v, _ = redact.Text(v)
	for _, n := range s.names {
		// Twice: adjacent occurrences share their separator.
		v = n.re.ReplaceAllString(n.re.ReplaceAllString(v, n.with), n.with)
	}
	return truncate(v, MaxField)
}

// replacePrefix shows dir as with wherever it starts a path; /home/ann is
// kept inside /home/anna.
func replacePrefix(v, home, with string) string {
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
			b.WriteString(with)
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
