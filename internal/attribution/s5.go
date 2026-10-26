package attribution

import (
	"slices"
	"strings"
)

// Thresholds of rule S5 (decision 0072, E32).
const (
	// WriteWeight: a writing call counts as this many reads.
	WriteWeight = 3
	// A turn filed in a transverse alias moves to the repository holding
	// at least MinVotes weighted votes and Share of them.
	MinVotes = 4
	Share    = 0.6
	// A turn filed in a repository moves to another one holding at least
	// OverrideVotes weighted votes and OverrideShare of every vote, the
	// launch repository's implicit ones included.
	OverrideVotes = 3
	OverrideShare = 0.75
	// mentionChars: how much of the prompt is searched for project names.
	mentionChars = 2000
)

// Sources of a decision.
const (
	SourceEvidence   = "evidence"         // the turn's own paths
	SourceMention    = "mention"          // the prompt names the project, no path against it
	SourceBoth       = "evidence+mention" // one repository named by both
	SourceContinuity = "continuity"       // a transverse turn with no path keeps the previous turn's project
	SourceCWD        = "cwd"              // the applied project stays: nothing clear against it
	SourceNone       = "none"             // a transverse turn stays transverse
)

// Count is one project's weighted votes.
type Count struct {
	Project string `json:"project"`
	N       int    `json:"n"`
}

// Votes are a turn's evidence, each list in the order its projects first
// appeared (it breaks ties, as E32's counters did): Explicit from calls
// whose paths name a project, Implicit from shell calls with no path, for
// the repository they ran in.
type Votes struct {
	Explicit []Count `json:"explicit,omitempty"`
	Implicit []Count `json:"implicit,omitempty"`
}

func add(c *[]Count, p string, n int) {
	for i := range *c {
		if (*c)[i].Project == p {
			(*c)[i].N += n
			return
		}
	}
	*c = append(*c, Count{Project: p, N: n})
}

func get(c []Count, p string) int {
	for _, x := range c {
		if x.Project == p {
			return x.N
		}
	}
	return 0
}

func sum(c []Count) int {
	n := 0
	for _, x := range c {
		n += x.N
	}
	return n
}

// top is the leader; ties go to the first seen.
func top(c []Count) (string, int) {
	best, n := "", 0
	for _, x := range c {
		if x.N > n {
			best, n = x.Project, x.N
		}
	}
	return best, n
}

// Projects lists every project the turn touched, explicitly first.
func (v Votes) Projects() []string {
	var out []string
	for _, c := range [][]Count{v.Explicit, v.Implicit} {
		for _, x := range c {
			if !slices.Contains(out, x.Project) {
				out = append(out, x.Project)
			}
		}
	}
	return out
}

// Tally counts a turn's calls: each own project a call's paths name gets
// one vote from it (WriteWeight when the call writes); a shell call with
// no path votes for the repository it ran in.
func Tally(calls []Call, r *Repos) Votes {
	var v Votes
	for _, c := range calls {
		w := 1
		if c.Write {
			w = WriteWeight
		}
		var projects []string
		for _, p := range c.Paths {
			if q := r.Project(p); q != "" && !slices.Contains(projects, q) {
				projects = append(projects, q)
			}
		}
		slices.Sort(projects)
		for _, q := range projects {
			add(&v.Explicit, q, w)
		}
		if len(c.Paths) == 0 && c.Bash && c.CWD != "" {
			if q := r.Project(c.CWD); q != "" {
				add(&v.Implicit, q, w)
			}
		}
	}
	return v
}

// Previous is the decision of a session's previous main turn, which a
// transverse turn with no path of its own may keep.
type Previous struct {
	Project string `json:"project,omitempty"`
	Source  string `json:"source,omitempty"`
}

// Decide applies S5 to a turn filed in base (the project the record rule
// gives it; transverse when base is a transverse alias). mentions are the
// own project names its prompt names. It returns the turn's project and
// why.
func Decide(base string, transverse bool, v Votes, mentions []string, prev Previous) (string, string) {
	exp := v.Explicit
	if transverse {
		if p := majority(exp, MinVotes, Share); p != "" {
			return p, SourceEvidence
		}
		if len(mentions) == 1 && allAre(exp, mentions[0]) {
			return mentions[0], SourceMention
		}
		if len(exp) == 1 && slices.Contains(mentions, exp[0].Project) {
			return exp[0].Project, SourceBoth
		}
		if len(exp) == 0 && prev.Project != "" && prev.Source != SourceCWD && prev.Source != SourceNone {
			return prev.Project, SourceContinuity
		}
		return base, SourceNone
	}
	if q, n := top(exp); n > 0 && q != base {
		if n >= OverrideVotes && float64(n)/float64(sum(exp)+sum(v.Implicit)) >= OverrideShare {
			return q, SourceEvidence
		}
		if slices.Contains(mentions, q) && get(exp, base)+get(v.Implicit, base) == 0 {
			return q, SourceBoth
		}
	}
	return base, SourceCWD
}

func majority(exp []Count, minVotes int, share float64) string {
	tot := sum(exp)
	if tot < minVotes {
		return ""
	}
	p, n := top(exp)
	if float64(n)/float64(tot) >= share {
		return p
	}
	return ""
}

func allAre(exp []Count, p string) bool {
	for _, x := range exp {
		if x.Project != p {
			return false
		}
	}
	return true
}

// Mentions lists the names among names that the prompt's start names as a
// whole word (letters, digits, _ and - glue), the way recall finds them.
// Names shorter than 4 characters or starting with a dot are left out.
func Mentions(prompt string, names []string) []string {
	if r := []rune(prompt); len(r) > mentionChars {
		prompt = string(r[:mentionChars])
	}
	text := strings.ToLower(prompt)
	var out []string
	for _, n := range names {
		if len([]rune(n)) < 4 || strings.HasPrefix(n, ".") || slices.Contains(out, n) {
			continue
		}
		if word(text, strings.ToLower(n)) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func word(text, name string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], name)
		if i < 0 {
			return false
		}
		a, b := from+i, from+i+len(name)
		if (a == 0 || !wordByte(text[a-1])) && (b == len(text) || !wordByte(text[b])) {
			return true
		}
		from = a + 1
	}
}

func wordByte(b byte) bool {
	return b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}
