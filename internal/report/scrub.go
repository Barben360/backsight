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
// remote server host, git remotes, OS user name).
type Scrubber struct {
	home     []string
	prefixes []prefix // home and the roots, longest first
	secrets  []string // longest first
	names    []name   // longest first
	values   []string // every value given to Name
}

type prefix struct{ path, with string }

type name struct {
	re   *regexp.Regexp
	with string
	n    int // the length of the value it stands for
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
//
// Names apply longest first, so a project named after the machine goes
// whole; a value already given keeps its first placeholder.
func (s *Scrubber) Name(v, placeholder string) { s.name(v, placeholder, "") }

// name is Name; flags "(?i)" matches v in any case.
func (s *Scrubber) name(v, placeholder, flags string) {
	if v == "" || slices.Contains(s.values, v) {
		return
	}
	s.values = append(s.values, v)
	before, after := `(^|[^A-Za-z0-9])`, `($|[^A-Za-z0-9])`
	if len(v) < minName {
		before, after = `(^|[/\\])`, `($|[/\\])`
	}
	for _, f := range encodings(v) {
		s.addName(flags+before+regexp.QuoteMeta(f)+after, placeholder, len(v))
	}
}

// RemotePlaceholder stands for a git remote.
const RemotePlaceholder = "<remote>"

// Remote hides a recorded git remote (project.NormaliseRemote's host/path,
// or a local path) as <remote> in every form a message quotes it, in any
// case (the record is lower case, git's own messages are not): whole, as
// scp (host:path), without its host, and its owner: the namespace before
// the repository's name and its first segment (decision 0067 block 4
// review rules). A public name as owner is kept, and the repository's
// name alone is a project's (Project).
func (s *Scrubber) Remote(r string) {
	r = strings.TrimSpace(r)
	if r == "" {
		return
	}
	forms := []string{r}
	if host, path, ok := strings.Cut(r, "/"); ok && host != "" && path != "" {
		forms = append(forms, host+":"+path, host+":/"+path, path)
		if i := strings.LastIndex(path, "/"); i > 0 {
			ns := path[:i]
			owner, _, _ := strings.Cut(ns, "/")
			for _, o := range []string{ns, owner} {
				if !slices.Contains(publicNames, strings.ToLower(o)) {
					forms = append(forms, o)
				}
			}
		}
	}
	for _, f := range forms {
		s.name(f, RemotePlaceholder, "(?i)")
	}
}

func (s *Scrubber) addName(re, placeholder string, n int) {
	s.names = append(s.names, name{regexp.MustCompile(re), "${1}" + placeholder + "${2}", n})
	sort.SliceStable(s.names, func(i, j int) bool { return s.names[i].n > s.names[j].n })
}

// ProjectPlaceholder stands for a project name.
const ProjectPlaceholder = "<project>"

// publicNames are not a user's project names whatever the bank says:
// names every install shares (Claude Code's directory, the default
// transverse alias, the tools' own names) and the placeholders' words.
// Hiding them would blank backsight's own paths and commands, not a user.
var publicNames = []string{".claude", "claude", "backsight", "hindsight",
	"project", "host", "user", "bank", "server", "remote", "data", "config", "state", "bin", "backups", "compose"}

// projectPrefix is where a name of any length is a project's: a project:
// tag, escaped or not, and a --project flag.
const projectPrefix = `(project:|project%3[Aa]|--project[ =])`

// projectEnd ends a project name: '-', '_' and '.' continue it, a '.' that
// ends a sentence does not.
const projectEnd = `($|[^A-Za-z0-9_.-]|\.(?:$|[^A-Za-z0-9_-]))`

// Project hides a project name as <project>, the way Name does; a name
// under minName characters is hidden after a project: tag or a --project
// flag too (project:api), where it can be nothing but a project. Public
// names are kept.
func (s *Scrubber) Project(v string) {
	if v == "" || slices.Contains(publicNames, strings.ToLower(v)) {
		return
	}
	known := slices.Contains(s.values, v)
	s.Name(v, ProjectPlaceholder)
	if len(v) < minName && !known {
		for _, f := range encodings(v) {
			s.addName(projectPrefix+regexp.QuoteMeta(f)+projectEnd, ProjectPlaceholder, len(v))
		}
	}
}

// listedProject replaces a short project name as a whole word, in the
// check that lists it (doctor.Check.Projects): there it is a name, not a
// word of the text.
func listedProject(text, p string) string {
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(p) + projectEnd)
	return re.ReplaceAllString(re.ReplaceAllString(text, "${1}"+ProjectPlaceholder+"${2}"), "${1}"+ProjectPlaceholder+"${2}")
}

// String scrubs one value.
func (s *Scrubber) String(v string) string { return s.scrub(v, true) }

// scrub scrubs v; names false keeps the names (an identifier).
func (s *Scrubber) scrub(v string, names bool) string {
	v = httpBody.ReplaceAllString(v, "$1 [server answer omitted]")
	for _, sec := range s.secrets {
		v = strings.ReplaceAll(v, sec, SecretPlaceholder)
	}
	for _, p := range s.prefixes {
		v = replacePrefix(v, p.path, p.with)
	}
	v, _ = redact.Text(v)
	for _, n := range s.names {
		if !names {
			break
		}
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
// form: a field added later is scrubbed without anyone remembering to. The
// project names doctor's checks list are hidden too, the short ones as
// whole words in their own check, after the rest (a remote holding one
// goes whole first); check ids and error classes keep every name (see
// identifiers).
func (s *Scrubber) Diagnostic(d Diagnostic) (Diagnostic, error) {
	for _, c := range d.Doctor.Checks {
		for _, p := range c.Projects {
			s.Project(p)
		}
	}
	out, err := s.diagnostic(d)
	if err != nil || len(out.Doctor.Checks) != len(d.Doctor.Checks) {
		return out, err
	}
	for i, c := range d.Doctor.Checks {
		o := &out.Doctor.Checks[i]
		for _, p := range c.Projects {
			if p != "" && len(p) < minName && !slices.Contains(publicNames, strings.ToLower(p)) {
				o.Summary, o.Detail, o.Fix = listedProject(o.Summary, p), listedProject(o.Detail, p), listedProject(o.Fix, p)
			}
		}
	}
	return out, nil
}

func (s *Scrubber) diagnostic(d Diagnostic) (Diagnostic, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return d, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return d, err
	}
	if raw, err = json.Marshal(s.walk(tree, "")); err != nil {
		return d, err
	}
	var out Diagnostic
	err = json.Unmarshal(raw, &out)
	return out, err
}

// identifiers are the JSON paths of the diagnostic's fixed-vocabulary
// strings: check ids, error classes (values, and the keys of the
// error_classes map). Built by backsight from its own words (Errors keeps
// free text out of a class), they are the same for every user, so the
// search link finds duplicates; no name replaces a word in them (a project
// called tick would rename job/tick/failed). Paths and secrets are still
// scrubbed, and a value outside identifier characters is scrubbed whole.
var identifiers = map[string]bool{"doctor.checks[].id": true, "recent_errors[].class": true, "counts.error_classes{}": true}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

func (s *Scrubber) field(v, path string) string {
	return s.scrub(v, !identifiers[path] || !identifier.MatchString(v))
}

func (s *Scrubber) walk(v any, path string) any {
	switch t := v.(type) {
	case string:
		return s.field(t, path)
	case []any:
		for i := range t {
			t[i] = s.walk(t[i], path+"[]")
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[s.field(k, path+"{}")] = s.walk(x, strings.TrimPrefix(path+"."+k, "."))
		}
		return out
	}
	return v
}
