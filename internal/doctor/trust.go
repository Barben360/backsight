package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/project"
)

// checkBacksightToml names the .backsight.toml files git tracks whose
// project or related backsight ignores (decision 0072 M5): a cloned
// repository could otherwise name the project its sessions are stored
// under, or pull other projects' memories into them. It looks at doctor's
// directory and at the live sessions' (by-pid). Info, not a fault: the
// keys are ignored on purpose, and only the file's owner can move them.
func checkBacksightToml(env Env) Check {
	c := Check{ID: "backsight-toml", Status: OK, Summary: "no .backsight.toml tracked by git sets project or related (this directory and live sessions')"}
	dirs := []string{}
	if env.Cwd != "" {
		dirs = append(dirs, env.Cwd)
	}
	live, _ := filepath.Glob(filepath.Join(env.Dirs.SessionsDir(), "by-pid", "*.json"))
	for _, f := range live {
		raw, err := os.ReadFile(f)
		var l struct {
			Cwd string `json:"cwd"`
		}
		if err == nil && json.Unmarshal(raw, &l) == nil && filepath.IsAbs(l.Cwd) && !slices.Contains(dirs, l.Cwd) {
			dirs = append(dirs, l.Cwd)
		}
	}
	res := project.New(env.Home, config.Default(env.Dirs, env.Home).Projects.Transverse)
	var files []string
	for _, d := range dirs {
		for _, f := range res.Ignored(d) {
			if !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 {
		return c
	}
	c.Status = Info
	if len(files) == 1 {
		c.Summary = files[0] + " is tracked by git: its project and related are ignored"
	} else {
		c.Summary = fmt.Sprintf("%d .backsight.toml files are tracked by git: their project and related are ignored", len(files))
		c.Detail = strings.Join(files, "; ") + "; "
	}
	c.Detail += "a cloned repository could set these keys, so only an untracked file is trusted for them; retain = false still applies"
	c.Fix = "if the file is yours and you want its project or related on this machine: in its repository, `git rm --cached .backsight.toml`, " +
		"add `.backsight.toml` to `.git/info/exclude`, and commit the removal (the file stays on disk, untracked, and is trusted). Otherwise nothing to do"
	return c
}
