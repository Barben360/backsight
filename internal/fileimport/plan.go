package fileimport

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/retain"
)

// FolderPlan is the plan of one top-level folder ("." for files directly
// in the root).
type FolderPlan struct {
	Folder string `json:"folder"`
	Files  int    `json:"files"`
	Facts  int64  `json:"facts"`
	retain.Estimate
}

// ProjectPlan is the plan of one project the files go to.
type ProjectPlan struct {
	Project string `json:"project"`
	Files   int    `json:"files"`
	Facts   int64  `json:"facts"`
	retain.Estimate
}

// PlanFile is one file a preset's plan sends: its project and its date.
type PlanFile struct {
	Rel     string    `json:"rel"`
	Project string    `json:"project"`
	Date    time.Time `json:"date"`
	// DateFrom: "header" (the file's own header) or "mtime".
	DateFrom string `json:"date_from"`
}

// Plan is what an import would send and cost.
type Plan struct {
	Root                 string          `json:"root"`
	Source               string          `json:"source"`
	Project              string          `json:"project"`
	Files                int             `json:"files"`
	Facts                int64           `json:"facts"`
	Total                retain.Estimate `json:"total"`
	Folders              []FolderPlan    `json:"folders"`
	Projects             []ProjectPlan   `json:"projects"`
	Skipped              map[string]int  `json:"skipped"`
	SkippedDirs          map[string]int  `json:"skipped_dirs"`
	DatesFromFrontmatter int             `json:"dates_from_frontmatter"`
	DatesFromMtime       int             `json:"dates_from_mtime"`
	RedactedFiles        int             `json:"redacted_files"`
	RedactedKinds        []string        `json:"redacted_kinds"`
	PricesKnown          bool            `json:"prices_known"`
	Currency             string          `json:"currency,omitempty"`
	// A preset's plan names each file: Preset, the stores it read, the
	// files sent with their project and date, the files skipped by reason.
	Preset       string              `json:"preset,omitempty"`
	Stores       []string            `json:"stores,omitempty"`
	Sends        []PlanFile          `json:"sends,omitempty"`
	SkippedFiles map[string][]string `json:"skipped_files,omitempty"`
	// MixesExcludedBy: the kept-out folders that keep the consolidated
	// files out (skip reason mixes_excluded).
	MixesExcludedBy []string `json:"mixes_excluded_by,omitempty"`
}

// BuildPlan estimates the files of s that toSend accepts (all when nil),
// one document each, per top-level folder (a preset's: its store's
// folders) and per project, sorted by input tokens.
func BuildPlan(s Scan, toSend func(File) bool, source, project string, p retain.Prices) Plan {
	plan := Plan{Root: s.Root, Source: source, Project: project, Skipped: map[string]int{}, SkippedDirs: map[string]int{},
		RedactedKinds: []string{}, PricesKnown: p.Known(), Currency: p.Currency, Preset: s.Preset, Stores: s.Stores}
	if s.Preset != "" {
		plan.MixesExcludedBy = slices.Clone(s.KeptOut)
		plan.Sends, plan.SkippedFiles = []PlanFile{}, map[string][]string{}
		for k, v := range s.Skipped {
			plan.SkippedFiles[k] = slices.Clone(v)
		}
	}
	for k, v := range s.Skipped {
		plan.Skipped[k] = len(v)
	}
	for k, v := range s.SkippedDirs {
		plan.SkippedDirs[k] = len(v)
	}
	folders := map[string]*FolderPlan{}
	projects := map[string]*ProjectPlan{}
	for _, f := range s.Files {
		if toSend != nil && !toSend(f) {
			continue
		}
		folder := folderOf(f.Rel, s.Preset != "")
		fp := folders[folder]
		if fp == nil {
			fp = &FolderPlan{Folder: folder}
			folders[folder] = fp
		}
		proj := f.Project
		if proj == "" {
			proj = project
		}
		pp := projects[proj]
		if pp == nil {
			pp = &ProjectPlan{Project: proj}
			projects[proj] = pp
		}
		e := retain.EstimateDocument(f.Chars, p)
		facts := retain.EstimatedDocumentFacts(f.Chars)
		fp.Files++
		fp.Facts += facts
		fp.Estimate = fp.Add(e)
		pp.Files++
		pp.Facts += facts
		pp.Estimate = pp.Add(e)
		if plan.Sends != nil {
			from := "header"
			if f.DateFromMtime {
				from = "mtime"
			}
			plan.Sends = append(plan.Sends, PlanFile{Rel: f.Rel, Project: proj, Date: f.Date, DateFrom: from})
		}
		plan.Files++
		plan.Facts += facts
		plan.Total = plan.Total.Add(e)
		if f.DateFromMtime {
			plan.DatesFromMtime++
		} else {
			plan.DatesFromFrontmatter++
		}
		if len(f.Redacted) > 0 {
			plan.RedactedFiles++
			for _, k := range f.Redacted {
				if !slices.Contains(plan.RedactedKinds, k) {
					plan.RedactedKinds = append(plan.RedactedKinds, k)
				}
			}
		}
	}
	slices.Sort(plan.RedactedKinds)
	plan.Folders = []FolderPlan{}
	for _, fp := range folders {
		plan.Folders = append(plan.Folders, *fp)
	}
	slices.SortFunc(plan.Folders, func(a, b FolderPlan) int {
		if a.InputTokens != b.InputTokens {
			return int(b.InputTokens - a.InputTokens)
		}
		return strings.Compare(a.Folder, b.Folder)
	})
	plan.Projects = []ProjectPlan{}
	for _, k := range slices.Sorted(maps.Keys(projects)) {
		plan.Projects = append(plan.Projects, *projects[k])
	}
	slices.SortStableFunc(plan.Projects, func(a, b ProjectPlan) int { return int(b.InputTokens - a.InputTokens) })
	return plan
}

// folderOf is the folder a file counts under in the plan: its top-level
// folder ("." for a file directly in the root); for a preset, whose root
// holds stores, the folder below the store too (memories_v2/rollout_summaries).
func folderOf(rel string, preset bool) string {
	parts := strings.Split(rel, "/")
	depth := 1
	if preset {
		depth = 2
	}
	if len(parts) == 1 {
		return "."
	}
	return strings.Join(parts[:min(depth, len(parts)-1)], "/")
}
