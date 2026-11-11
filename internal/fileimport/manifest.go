package fileimport

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// Entry is one file of an import.
type Entry struct {
	Rel      string    `json:"rel"`
	Document string    `json:"document"`
	SHA256   string    `json:"sha256"`
	Date     time.Time `json:"date"`
	Sent     bool      `json:"sent"`
	Gone     bool      `json:"gone,omitempty"` // no longer in the folder; stays in memory
	// Project: the project a preset maps this file to ("" is the
	// manifest's Project); SentProject: the project its last send went to,
	// so that a send to another one first clears what the old one drew
	// from it.
	Project     string `json:"project,omitempty"`
	SentProject string `json:"sent_project,omitempty"`
}

// Manifest is what this machine knows of one imported folder.
type Manifest struct {
	Schema  int    `json:"schema"`
	Source  string `json:"source"`
	Root    string `json:"root"`
	Project string `json:"project"`
	// Preset: the reader of this source ("" a plain folder, "codex"
	// Codex's memories), which maps each file to its project and date.
	Preset      string    `json:"preset,omitempty"`
	Ext         []string  `json:"ext,omitempty"`
	Exclude     []string  `json:"exclude,omitempty"`
	QueuedAt    time.Time `json:"queued_at"`
	MaxCost     float64   `json:"max_cost,omitempty"`
	MaxTokens   int64     `json:"max_tokens,omitempty"`
	Currency    string    `json:"currency,omitempty"`
	SpentCost   float64   `json:"spent_cost"`
	SpentTokens int64     `json:"spent_tokens"`
	// PriorCost and PriorTokens: what earlier runs of this import spent;
	// Spent* and the cap are the current run's.
	PriorCost         float64  `json:"prior_cost,omitempty"`
	PriorTokens       int64    `json:"prior_tokens,omitempty"`
	StartDay          string   `json:"start_day"`
	StartBase         [4]int64 `json:"start_base"`
	Paused            string   `json:"paused,omitempty"` // budget | ""
	AllowSubscription bool     `json:"allow_subscription,omitempty"`
	Entries           []Entry  `json:"entries"` // sorted by Rel
}

// MergeResult counts what a new scan changed.
type MergeResult struct {
	New       int `json:"new"`
	Changed   int `json:"changed"`
	Unchanged int `json:"unchanged"`
	Gone      int `json:"gone"`
	// Moved: files sent before whose project changed, to send again to
	// their new one under the same document.
	Moved int `json:"moved,omitempty"`
}

// ProjectOf is the project e's file goes to.
func (m Manifest) ProjectOf(e Entry) string {
	if e.Project != "" {
		return e.Project
	}
	return m.Project
}

// fileProject is f's project as an entry records it: "" for the
// manifest's own.
func (m Manifest) fileProject(f File) string {
	if f.Project == m.Project {
		return ""
	}
	return f.Project
}

func manifestPath(dir, source string) string { return filepath.Join(dir, source+".json") }

// LoadManifest reads a source's manifest; false when there is none.
func LoadManifest(dir, source string) (Manifest, bool, error) {
	raw, err := os.ReadFile(manifestPath(dir, source))
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, false, err
	}
	return m, true, nil
}

// SaveManifest writes m atomically, private to the user.
func SaveManifest(dir string, m Manifest) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WritePrivate(manifestPath(dir, m.Source), append(raw, '\n'))
}

// ListManifests reads every manifest, oldest queued first.
func ListManifests(dir string) ([]Manifest, error) {
	names, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, n := range names {
		src, ok := strings.CutSuffix(n.Name(), ".json")
		if !ok || n.IsDir() {
			continue
		}
		m, found, err := LoadManifest(dir, src)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b Manifest) int { return a.QueuedAt.Compare(b.QueuedAt) })
	return out, nil
}

// DeleteManifest removes a source's manifest; an absent one is no error.
func DeleteManifest(dir, source string) error {
	err := os.Remove(manifestPath(dir, source))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Merge applies a new scan: an unchanged file keeps its state, a changed
// one is to send again under the same document, a new one is added under
// a document no tombstone names, and a file that disappeared is marked
// gone and stays in memory.
func (m *Manifest) Merge(s Scan, tombs []state.Tombstone) MergeResult {
	var res MergeResult
	seen := map[string]bool{}
	byRel := map[string]int{}
	for i, e := range m.Entries {
		byRel[e.Rel] = i
	}
	for _, f := range s.Files {
		seen[f.Rel] = true
		i, ok := byRel[f.Rel]
		if !ok {
			m.Entries = append(m.Entries, Entry{Rel: f.Rel, Document: DocumentID(m.Source, f.Rel, tombs), SHA256: f.SHA256, Date: f.Date,
				Project: m.fileProject(f)})
			res.New++
			continue
		}
		e := &m.Entries[i]
		e.Gone = false
		moved := false
		if to := m.fileProject(f); to != e.Project {
			// The document goes again, to the new project; its send first
			// clears what the old one drew from it (SentProject).
			if e.Sent && e.SentProject == "" {
				e.SentProject = m.ProjectOf(*e)
			}
			e.Project = to
			if e.SentProject != "" && e.SentProject != m.ProjectOf(*e) {
				e.Sent, moved = false, true
			}
		}
		if e.SHA256 == f.SHA256 {
			if moved {
				res.Moved++
			} else {
				res.Unchanged++
			}
			continue
		}
		e.SHA256, e.Date, e.Sent = f.SHA256, f.Date, false
		res.Changed++
	}
	for i := range m.Entries {
		if e := &m.Entries[i]; !seen[e.Rel] && !e.Gone {
			e.Gone = true
			res.Gone++
		}
	}
	slices.SortFunc(m.Entries, func(a, b Entry) int { return strings.Compare(a.Rel, b.Rel) })
	return res
}

// Unsent are the entries still to send.
func (m Manifest) Unsent() []Entry {
	var out []Entry
	for _, e := range m.Entries {
		if !e.Sent && !e.Gone {
			out = append(out, e)
		}
	}
	return out
}

// within reports whether p is root or below it, by whole components.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// HeldByOthers are the absolute paths under root that another source,
// whose root contains root or lies inside it, already holds.
func HeldByOthers(all []Manifest, self, root string) map[string]bool {
	root = filepath.Clean(filepath.FromSlash(root))
	held := map[string]bool{}
	for _, m := range all {
		if m.Source == self {
			continue
		}
		other := filepath.Clean(filepath.FromSlash(m.Root))
		if !within(root, other) && !within(other, root) {
			continue
		}
		for _, e := range m.Entries {
			p := filepath.Join(other, filepath.FromSlash(e.Rel))
			if within(root, p) {
				held[p] = true
			}
		}
	}
	return held
}

// Drop moves the files held by other sources out of s.Files, as skipped
// other_source.
func (s *Scan) Drop(held map[string]bool) {
	if len(held) == 0 {
		return
	}
	kept := s.Files[:0]
	for _, f := range s.Files {
		if held[f.Abs] {
			if s.Skipped == nil {
				s.Skipped = map[string][]string{}
			}
			s.Skipped[SkipOtherSource] = append(s.Skipped[SkipOtherSource], f.Rel)
			continue
		}
		kept = append(kept, f)
	}
	s.Files = kept
}

// SeedFromServer rebuilds a lost manifest from the source's documents on
// the server: a scanned file whose document holds the same hash is sent;
// one whose document holds another hash is to send again, same document.
func SeedFromServer(m *Manifest, s Scan, docs []hindsight.DocumentItem) {
	files := map[string]File{}
	for _, f := range s.Files {
		files[f.Rel] = f
	}
	prefix := DocumentPrefix + m.Source + "/"
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, prefix) {
			continue
		}
		f, ok := files[d.Metadata["path"]]
		if !ok || slices.ContainsFunc(m.Entries, func(e Entry) bool { return e.Rel == f.Rel }) {
			continue
		}
		e := Entry{Rel: f.Rel, Document: d.ID, SHA256: f.SHA256, Date: f.Date, Project: m.fileProject(f)}
		// The document's project is the one its tags name: it counts as
		// sent when it holds the same content for the same project.
		e.SentProject = m.ProjectOf(e)
		for _, t := range d.Tags {
			if p, ok := strings.CutPrefix(t, "project:"); ok {
				e.SentProject = p
				break
			}
		}
		e.Sent = d.Metadata["sha256"] == f.SHA256 && e.SentProject == m.ProjectOf(e)
		m.Entries = append(m.Entries, e)
	}
	slices.SortFunc(m.Entries, func(a, b Entry) int { return strings.Compare(a.Rel, b.Rel) })
}
