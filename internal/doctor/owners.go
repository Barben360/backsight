package doctor

import (
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/attribution"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/retain"
)

// checkOwners lists the remote owners whose repositories count as the
// user's because a retained session started in a repository the user
// committed to: a subagent's report may follow its work into them. One the
// user does not own is denied in config.toml.
func checkOwners(env Env) Check {
	c := Check{ID: "own-owners", Status: Info}
	learnt := (retain.Attributor{Dir: env.Dirs.AttributionDir()}).Learnt()
	var deny []string
	if cfg, err := config.Load(env.Dirs.ConfigFile(), env.Dirs, env.Home); err == nil {
		for _, o := range cfg.Projects.DenyOwners {
			deny = append(deny, attribution.OwnerOf(o))
		}
	}
	learnt = slices.DeleteFunc(learnt, func(o string) bool { return slices.Contains(deny, o) })
	if len(learnt) == 0 {
		c.Summary = "no repository owner learnt yet from your sessions"
		return c
	}
	c.Summary = "repositories of these owners count as yours (learnt from repositories you committed to): " + strings.Join(learnt, ", ")
	c.Fix = `one that is not yours: projects.deny_owners = ["<host/owner>"] in config.toml`
	c.Projects = learnt // report hides them
	return c
}
