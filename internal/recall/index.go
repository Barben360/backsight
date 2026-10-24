package recall

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

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
)

// BuildIndex reads every project's entities from the server.
func BuildIndex(ctx context.Context, c *hindsight.Client, bank string, now time.Time) (Index, error) {
	idx := Index{BuiltAt: now.UTC(), Names: map[string][]ProjectCount{}}
	tags, err := c.Tags(ctx, bank, "project:")
	if err != nil {
		return idx, err
	}
	for _, t := range tags {
		project := strings.TrimPrefix(t.Tag, "project:")
		ents, err := c.Entities(ctx, bank, t.Tag, entitiesPerProject)
		if err != nil {
			return idx, err
		}
		for _, e := range ents {
			idx.Names[e.Name] = append(idx.Names[e.Name], ProjectCount{project, e.Mentions})
		}
	}
	return idx, nil
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

// Projects are the other projects whose distinctive names the text
// mentions, most mentioned first, at most MaxRelated.
func (idx Index) Projects(text, current string, transverse []string) []string {
	score := map[string]int{}
	for name, ps := range idx.Names {
		if !distinctive(name, ps) || !mentions(text, name) {
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

// mentions: name appears in text as a whole word, any case.
func mentions(text, name string) bool {
	re, err := regexp.Compile(`(?i)(?:^|[^\pL\pN_-])` + regexp.QuoteMeta(name) + `(?:[^\pL\pN_-]|$)`)
	return err == nil && re.MatchString(text)
}
