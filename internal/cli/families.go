package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/retain"
)

// familyMaxAge: a families file older than this is not used, as if its
// job had stopped (the links of a past phase must decay, decision 0072).
const familyMaxAge = 7 * 24 * time.Hour

// linkedProjects are the projects recall searches with current in its
// project scope, without its boost: those cwd's .backsight.toml relates,
// then, with recall.families on, current's family, at most
// project.MaxRelated in all. A family member kept out of memory since the
// families file was built is left out.
func linkedProjects(d paths.Dirs, cfg config.Config, res *project.Resolver, cwd, current string) []string {
	out := res.Related(cwd)
	if !cfg.Recall.Families {
		return out
	}
	f, ok := familiesFile(d)
	if !ok {
		return out
	}
	for _, p := range f.Of[current] {
		if len(out) >= project.MaxRelated {
			break
		}
		if p == current || slices.Contains(out, p) {
			continue
		}
		if root := f.Roots[p]; root != "" {
			if kept, _ := res.OptedOut(root, cfg.Privacy.ExcludePaths); kept {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// familiesFile is the families file when it is readable and fresh.
func familiesFile(d paths.Dirs) (retain.Families, bool) {
	f, err := retain.LoadFamilies(d.FamiliesFile())
	if err != nil || now().Sub(f.BuiltAt) > familyMaxAge {
		return retain.Families{}, false
	}
	return f, true
}

// familyOf is project's family from the families file, none when the file
// is missing, unreadable or stale.
func familyOf(d paths.Dirs, project string) []string {
	f, _ := familiesFile(d)
	return f.Of[project]
}

// refreshFamilies recomputes the families file from the recorded turns
// (whether recall.families is on or not: a replay and doctor read it).
// tags are the bank's project tags (nil when they could not be read): a
// project forgotten since is no member.
func refreshFamilies(d paths.Dirs, cfg config.Config, res *project.Resolver, tags []hindsight.TagCount) string {
	var known func(string) bool
	if tags != nil {
		set := map[string]bool{}
		for _, t := range tags {
			set[strings.TrimPrefix(t.Tag, "project:")] = true
		}
		known = func(p string) bool { return set[p] }
	}
	f, err := retain.RefreshFamilies(attributor(d, cfg, res), d.FamiliesFile(), now(), known)
	if err != nil {
		return "failed: " + err.Error()
	}
	return fmt.Sprintf("%d link(s) from %d turn(s)", len(f.Links), f.Turns)
}
