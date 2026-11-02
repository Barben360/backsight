package fileimport

import (
	"reflect"
	"testing"

	"github.com/Barben360/backsight/internal/retain"
)

func TestBuildPlanSumsPerFile(t *testing.T) {
	p := retain.Prices{Input: 1, Output: 2, Currency: "USD"}
	s := Scan{Root: "/r", Files: []File{
		{Rel: "A/x.md", Chars: 30001},
		{Rel: "A/y.md", Chars: 30001},
		{Rel: "top.md", Chars: 10},
	}}
	plan := BuildPlan(s, nil, "src", "home", p)
	want := retain.EstimateSession(30001, p).Add(retain.EstimateSession(30001, p)).Add(retain.EstimateSession(10, p))
	if plan.Total != want {
		t.Errorf("total = %+v, want %+v (one estimate per file)", plan.Total, want)
	}
	if plan.Files != 3 || plan.Facts != 100 {
		t.Errorf("files %d facts %d, want 3 and 100", plan.Files, plan.Facts)
	}
	if len(plan.Folders) != 2 || plan.Folders[0].Folder != "A" || plan.Folders[1].Folder != "." {
		t.Fatalf("folders = %+v", plan.Folders)
	}
	if plan.Folders[0].Files != 2 || plan.Folders[0].Facts != 100 || plan.Folders[1].Facts != 0 {
		t.Errorf("folder counts = %+v", plan.Folders)
	}
	if !plan.PricesKnown || plan.Currency != "USD" || plan.Source != "src" || plan.Project != "home" || plan.Root != "/r" {
		t.Errorf("header = %+v", plan)
	}
}

func TestBuildPlanCountsSkipsDatesRedactions(t *testing.T) {
	s := Scan{
		Files: []File{
			{Rel: "a.md", Chars: 5, DateFromMtime: true, Redacted: []string{"pem"}},
			{Rel: "b.md", Chars: 5, Redacted: []string{"anthropic_key", "pem"}},
			{Rel: "c.md", Chars: 5},
		},
		Skipped:     map[string][]string{"too_large": {"big.md"}, "extension": {"x.pdf", "y.pdf"}},
		SkippedDirs: map[string][]string{"hidden": {".git", ".obsidian"}},
	}
	plan := BuildPlan(s, nil, "s", "p", retain.Prices{})
	if !reflect.DeepEqual(plan.Skipped, map[string]int{"too_large": 1, "extension": 2}) {
		t.Errorf("skipped = %v", plan.Skipped)
	}
	if !reflect.DeepEqual(plan.SkippedDirs, map[string]int{"hidden": 2}) {
		t.Errorf("skipped dirs = %v", plan.SkippedDirs)
	}
	if plan.DatesFromMtime != 1 || plan.DatesFromFrontmatter != 2 {
		t.Errorf("dates: mtime %d frontmatter %d", plan.DatesFromMtime, plan.DatesFromFrontmatter)
	}
	if plan.RedactedFiles != 2 || !reflect.DeepEqual(plan.RedactedKinds, []string{"anthropic_key", "pem"}) {
		t.Errorf("redactions: %d %v", plan.RedactedFiles, plan.RedactedKinds)
	}
	if plan.PricesKnown {
		t.Error("prices known with zero prices")
	}
}

func TestBuildPlanEstimatesOnlyUnsent(t *testing.T) {
	p := retain.Prices{Input: 1, Output: 2}
	s := Scan{Files: []File{{Rel: "a.md", Chars: 6000}, {Rel: "b.md", Chars: 600}}}
	plan := BuildPlan(s, func(f File) bool { return f.Rel != "a.md" }, "s", "p", p)
	if plan.Files != 1 || plan.Total != retain.EstimateSession(600, p) || plan.Facts != 1 {
		t.Errorf("plan = files %d total %+v facts %d", plan.Files, plan.Total, plan.Facts)
	}
}

func TestBuildPlanSortsFoldersByTokens(t *testing.T) {
	s := Scan{Files: []File{{Rel: "small/a.md", Chars: 10}, {Rel: "big/a.md", Chars: 90000}}}
	plan := BuildPlan(s, nil, "s", "p", retain.Prices{})
	if plan.Folders[0].Folder != "big" {
		t.Errorf("folders = %+v, want big first", plan.Folders)
	}
}
