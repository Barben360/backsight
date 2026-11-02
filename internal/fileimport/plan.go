package fileimport

import (
	"slices"
	"strings"

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

// Plan is what an import would send and cost.
type Plan struct {
	Root                 string          `json:"root"`
	Source               string          `json:"source"`
	Project              string          `json:"project"`
	Files                int             `json:"files"`
	Facts                int64           `json:"facts"`
	Total                retain.Estimate `json:"total"`
	Folders              []FolderPlan    `json:"folders"`
	Skipped              map[string]int  `json:"skipped"`
	SkippedDirs          map[string]int  `json:"skipped_dirs"`
	DatesFromFrontmatter int             `json:"dates_from_frontmatter"`
	DatesFromMtime       int             `json:"dates_from_mtime"`
	RedactedFiles        int             `json:"redacted_files"`
	RedactedKinds        []string        `json:"redacted_kinds"`
	PricesKnown          bool            `json:"prices_known"`
	Currency             string          `json:"currency,omitempty"`
}

// BuildPlan estimates the files of s that toSend accepts (all when nil),
// one document each, per top-level folder, sorted by input tokens.
func BuildPlan(s Scan, toSend func(File) bool, source, project string, p retain.Prices) Plan {
	plan := Plan{Root: s.Root, Source: source, Project: project, Skipped: map[string]int{}, SkippedDirs: map[string]int{},
		RedactedKinds: []string{}, PricesKnown: p.Known(), Currency: p.Currency}
	for k, v := range s.Skipped {
		plan.Skipped[k] = len(v)
	}
	for k, v := range s.SkippedDirs {
		plan.SkippedDirs[k] = len(v)
	}
	folders := map[string]*FolderPlan{}
	for _, f := range s.Files {
		if toSend != nil && !toSend(f) {
			continue
		}
		folder, _, nested := strings.Cut(f.Rel, "/")
		if !nested {
			folder = "."
		}
		fp := folders[folder]
		if fp == nil {
			fp = &FolderPlan{Folder: folder}
			folders[folder] = fp
		}
		e := retain.EstimateDocument(f.Chars, p)
		facts := retain.EstimatedDocumentFacts(f.Chars)
		fp.Files++
		fp.Facts += facts
		fp.Estimate = fp.Add(e)
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
	return plan
}
