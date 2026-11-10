package recall

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
)

// Index is the entity index (A.3 entities.json): which projects mention
// which names, so a prompt naming them can search those projects (the
// related scope) and the quiet-prompt nudge can name terms. Hooks only read
// it; jobs rebuild it.
type Index struct {
	BuiltAt time.Time                 `json:"built_at"`
	Names   map[string][]ProjectCount `json:"names"`
	// Known are the bank's projects with their memory counts and last
	// activity, so a hook can name them to the agent without a server call.
	// An index written before it existed has none (Stale rebuilds it).
	Known []ProjectInfo `json:"projects,omitempty"`
}

// ProjectInfo is one project of the bank: its memories and when the last
// one was mentioned (zero when that read failed).
type ProjectInfo struct {
	Name     string    `json:"name"`
	Memories int       `json:"memories"`
	Last     time.Time `json:"last,omitzero"`
}

// ProjectCount is one project's mentions of a name.
type ProjectCount struct {
	Project string
	Count   int
}

// MarshalJSON writes [project, count] pairs (A.3).
func (p ProjectCount) MarshalJSON() ([]byte, error) { return json.Marshal([]any{p.Project, p.Count}) }

func (p *ProjectCount) UnmarshalJSON(b []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil || len(pair) != 2 {
		return errors.New("want [project, count]")
	}
	if err := json.Unmarshal(pair[0], &p.Project); err != nil {
		return err
	}
	return json.Unmarshal(pair[1], &p.Count)
}

const (
	// IndexMaxAge: older indexes are rebuilt by the next job.
	IndexMaxAge = 24 * time.Hour
	// A name mentioned in many projects (docker, github) says nothing about
	// where to look (C12).
	maxProjectsPerName = 8
	minNameLen         = 4
	entitiesPerProject = 200
	// ListMinMemories: a project with fewer memories is a stray directory
	// (a session once started in it), not one worth naming to the agent.
	ListMinMemories = 10
	// ListMaxIdle: a project quiet for longer is left out of the list
	// (still matched when a prompt names it).
	ListMaxIdle = 90 * 24 * time.Hour
	// NamedMinLen: shorter project names are too likely to be ordinary
	// words for a prompt naming one to mean that project.
	NamedMinLen = 5
)

// BuildIndex reads every project's entities from the server.
func BuildIndex(ctx context.Context, c *hindsight.Client, bank string, now time.Time) (Index, error) {
	idx := Index{BuiltAt: now.UTC(), Names: map[string][]ProjectCount{}}
	tags, err := c.Tags(ctx, bank, "project:")
	if err != nil {
		return idx, err
	}
	seen := map[string]bool{}
	for _, t := range tags {
		if seen[t.Tag] {
			continue // a server repeating tags must not double the counts
		}
		seen[t.Tag] = true
		project := strings.TrimPrefix(t.Tag, "project:")
		ents, err := c.Entities(ctx, bank, t.Tag, entitiesPerProject)
		if err != nil {
			return idx, err
		}
		info := ProjectInfo{Name: project, Memories: t.Count}
		if m, err := c.Recent(ctx, bank, t.Tag, "", 1); err == nil && len(m) > 0 {
			info.Last = m[0].When().UTC() // a failed read leaves the activity unknown, not the index
		}
		idx.Known = append(idx.Known, info)
		for _, e := range ents {
			idx.Names[e.Name] = append(idx.Names[e.Name], ProjectCount{project, e.Mentions})
		}
	}
	return idx, nil
}

// Stale: older than IndexMaxAge, or written before the index held the
// projects (an index with names but no projects).
func (idx Index) Stale(now time.Time) bool {
	return now.Sub(idx.BuiltAt) >= IndexMaxAge || (len(idx.Names) > 0 && len(idx.Known) == 0)
}

// Listed are the projects worth naming to the agent: not in skip, at
// least ListMinMemories memories, active within ListMaxIdle (or of unknown
// activity), most recently active first.
func (idx Index) Listed(skip []string, now time.Time) []ProjectInfo {
	var out []ProjectInfo
	for _, p := range idx.Known {
		if p.Name == "" || contains(skip, p.Name) || p.Memories < ListMinMemories || (!p.Last.IsZero() && now.Sub(p.Last) > ListMaxIdle) {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Named are the projects not in skip whose name the text mentions as a
// whole word (case ignored): names of NamedMinLen or more characters, of
// projects with ListMinMemories memories or more, idle or not, in the
// order of Known. Only the text's first maxMatchChars bytes are read.
func (idx Index) Named(text string, skip []string) []string {
	if len(text) > maxMatchChars {
		text = text[:maxMatchChars]
	}
	lower := strings.ToLower(text)
	var out []string
	for _, p := range idx.Known {
		if utf8.RuneCountInString(p.Name) < NamedMinLen || p.Memories < ListMinMemories || contains(skip, p.Name) || contains(out, p.Name) {
			continue
		}
		if mentions(lower, strings.ToLower(p.Name)) {
			out = append(out, p.Name)
		}
	}
	return out
}

// Has: the index knows a project by this exact name. An index without
// projects knows none, and says so with ok false.
func (idx Index) Has(name string) (has, ok bool) {
	if len(idx.Known) == 0 {
		return false, false
	}
	for _, p := range idx.Known {
		if p.Name == name {
			return true, true
		}
	}
	return false, true
}

// Closest are up to n known project names near name, nearest first: one
// contains the other, or the case-insensitive edit distance is at most a
// third of name's length (2 at least); ties in name order. A name far from
// every project gets none.
func (idx Index) Closest(name string, n int) []string {
	type cand struct {
		name string
		d    int
	}
	lower := strings.ToLower(name)
	var cs []cand
	limit := max(2, utf8.RuneCountInString(name)/3)
	for _, p := range idx.Known {
		other := strings.ToLower(p.Name)
		d := editDistance(lower, other)
		if d > limit && !strings.Contains(other, lower) && !strings.Contains(lower, other) {
			continue
		}
		cs = append(cs, cand{p.Name, d})
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].d != cs[j].d {
			return cs[i].d < cs[j].d
		}
		return cs[i].name < cs[j].name
	})
	var out []string
	for _, c := range cs {
		if len(out) == n {
			break
		}
		out = append(out, c.name)
	}
	return out
}

// editDistance is the Levenshtein distance between a and b, in runes.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// LoadIndex reads path; a missing file is an empty index.
func LoadIndex(path string) (Index, error) {
	var idx Index
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return idx, err
	}
	return idx, json.Unmarshal(raw, &idx)
}

// Save writes the index atomically.
func (idx Index) Save(path string) error {
	raw, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, raw, 0o600)
}

// distinctive: a real name (not a kind label, long enough) seen in few
// enough projects to point somewhere.
func distinctive(name string, projects []ProjectCount) bool {
	return len(name) >= minNameLen && !strings.Contains(name, ":") && len(projects) <= maxProjectsPerName
}

// maxMatchChars: only the start of a long text is matched against names
// (prompt recall uses 300 chars; the cost grows with names × text).
const maxMatchChars = 2000

// Projects are the other projects whose distinctive names the text
// mentions, most mentioned first, at most MaxRelated.
func (idx Index) Projects(text, current string, transverse []string) []string {
	if len(text) > maxMatchChars {
		text = text[:maxMatchChars]
	}
	lower := strings.ToLower(text)
	score := map[string]int{}
	for name, ps := range idx.Names {
		if !distinctive(name, ps) || !mentions(lower, strings.ToLower(name)) {
			continue
		}
		for _, p := range ps {
			if p.Project != current && !contains(transverse, p.Project) {
				score[p.Project] += p.Count
			}
		}
	}
	out := make([]string, 0, len(score))
	for p := range score {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if score[out[i]] != score[out[j]] {
			return score[out[i]] > score[out[j]]
		}
		return out[i] < out[j]
	})
	if len(out) > MaxRelated {
		out = out[:MaxRelated]
	}
	return out
}

// Mentioned are the distinctive names the text mentions, most mentioned
// in the bank first, at most n (the quiet-prompt nudge, SPEC §10.2).
func (idx Index) Mentioned(text string, n int) []string {
	if len(text) > maxMatchChars {
		text = text[:maxMatchChars]
	}
	lower := strings.ToLower(text)
	type term struct {
		name  string
		count int
	}
	var terms []term
	for name, ps := range idx.Names {
		if !distinctive(name, ps) || !mentions(lower, strings.ToLower(name)) {
			continue
		}
		c := 0
		for _, p := range ps {
			c += p.Count
		}
		terms = append(terms, term{name, c})
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].count != terms[j].count {
			return terms[i].count > terms[j].count
		}
		return terms[i].name < terms[j].name
	})
	var out []string
	for _, t := range terms {
		if len(out) == n {
			break
		}
		out = append(out, t.name)
	}
	return out
}

// Terms are up to n distinctive names of the current project, most
// mentioned first: what the quiet-prompt nudge can suggest searching.
func (idx Index) Terms(current string, transverse []string, n int) []string {
	type term struct {
		name  string
		count int
	}
	var terms []term
	for name, ps := range idx.Names {
		if !distinctive(name, ps) {
			continue
		}
		for _, p := range ps {
			if p.Project == current {
				terms = append(terms, term{name, p.Count})
			}
		}
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].count != terms[j].count {
			return terms[i].count > terms[j].count
		}
		return terms[i].name < terms[j].name
	})
	var out []string
	for _, t := range terms {
		if len(out) == n {
			break
		}
		out = append(out, t.name)
	}
	return out
}

// mentions: lowercase name appears in lowercase text as a whole word
// (neighbours are not letters, digits, _ or -).
func mentions(text, name string) bool {
	for from := 0; from <= len(text)-len(name); {
		i := strings.Index(text[from:], name)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(name)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (start == 0 || !wordRune(before)) && (end == len(text) || !wordRune(after)) {
			return true
		}
		from = start + 1
	}
	return false
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }
