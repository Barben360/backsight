package cli

import (
	"fmt"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/retain"
)

// familyMaxAge: a families file older than this is not used, as if its
// job had stopped (the links of a past phase must decay, decision 0072).
const familyMaxAge = 7 * 24 * time.Hour

// linkedProjects are the projects recall searches with current in its
// project scope, without its boost: those cwd's .backsight.toml relates,
// then, with recall.families on, current's family.
func linkedProjects(d paths.Dirs, cfg config.Config, res *project.Resolver, cwd, current string) []string {
	out := res.Related(cwd)
	if !cfg.Recall.Families {
		return out
	}
	for _, p := range familyOf(d, current) {
		if p != current && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// familyOf is project's family from the families file, none when the file
// is missing, unreadable or stale.
func familyOf(d paths.Dirs, project string) []string {
	f, err := retain.LoadFamilies(d.FamiliesFile())
	if err != nil || now().Sub(f.BuiltAt) > familyMaxAge {
		return nil
	}
	return f.Of[project]
}

// refreshFamilies recomputes the families file from the recorded turns
// (whether recall.families is on or not: a replay and doctor read it).
func refreshFamilies(d paths.Dirs, cfg config.Config, res *project.Resolver) string {
	f, err := retain.RefreshFamilies(attributor(d, cfg, res), d.FamiliesFile(), now())
	if err != nil {
		return "failed: " + err.Error()
	}
	return fmt.Sprintf("%d link(s) from %d turn(s)", len(f.Links), f.Turns)
}
