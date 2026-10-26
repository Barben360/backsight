package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/session"
)

type pauseResult struct {
	Paused  bool   `json:"paused"`
	Session string `json:"session,omitempty"`
	Path    string `json:"path,omitempty"` // --project: the excluded directory
	Changed bool   `json:"changed"`
	Planned bool   `json:"planned,omitempty"` // --plan: nothing written
	// StillExcluded: after resume --project, why the directory is still
	// not recorded (another entry, a parent path, .backsight.toml).
	StillExcluded string `json:"still_excluded,omitempty"`
}

// newPauseCmd is `pause [--project]` and `resume [--project]` (SPEC §13
// opt-out, decision 0056): a session flag every writer reads, or a project
// path in privacy.exclude_paths.
func newPauseCmd(e *env, pause bool) *cobra.Command {
	var sid string
	var proj, plan bool
	name, short := "resume", "Let memory record this session (or project) again"
	if pause {
		name, short = "pause", "Stop memory from recording this session (or, with --project, this project)"
	}
	cmd := &cobra.Command{
		Use:   name + " [--project]",
		Short: short,
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			res := pauseResult{Paused: pause}
			if plan && !proj {
				return &output.Error{Code: output.Usage, Message: "--plan goes with --project (a session pause changes no configuration)"}
			}
			if proj {
				cwd, _ := os.Getwd()
				cfgPath := d.ConfigFile()
				raw, sha, err := fsutil.ReadHashed(cfgPath)
				if err != nil {
					return blocked(err)
				}
				if sha == fsutil.Absent {
					// Writing one here would make server install refuse its
					// own settings later.
					return &output.Error{Code: output.Usage, Message: "no config.toml: backsight is not set up, so nothing records yet"}
				}
				cfg, err := config.Load(cfgPath, d, home)
				if err != nil {
					return &output.Error{Code: output.Usage, Message: err.Error()}
				}
				res.Path = project.New(home, cfg.Projects.Transverse).Root(cwd)
				if rel, err := filepath.Rel(res.Path, home); err == nil && !strings.HasPrefix(rel, "..") {
					// Outside a repository the project is the directory: in the
					// home directory (or above) it would stop all recording.
					return &output.Error{Code: output.Usage, Message: res.Path + " holds your home directory: pausing it would stop memory everywhere. Run it inside the project's directory, or pause this session with backsight pause"}
				}
				paths := slices.Clone(cfg.Privacy.ExcludePaths)
				has := slices.Contains(paths, res.Path)
				switch {
				case pause && !has:
					paths = append(paths, res.Path)
					res.Changed = true
				case !pause && has:
					paths = slices.DeleteFunc(paths, func(p string) bool { return p == res.Path })
					res.Changed = true
				}
				if res.Changed {
					out, err := config.SetExcludePaths(raw, paths)
					if err != nil {
						return &output.Error{Code: output.Usage, Message: err.Error()}
					}
					if _, err := config.Parse(out, cfgPath, d, home); err != nil {
						return &output.Error{Code: output.Usage, Message: "the edited config would not load: " + err.Error()}
					}
					res.Planned = plan
					if !plan {
						if err := fsutil.ReplaceIfUnchanged(cfgPath, out, 0o600, sha); err != nil {
							return blocked(err)
						}
					}
				}
				// The real state, as the writers will decide it: removing
				// this entry does not lift another one.
				if out, why := project.New(home, cfg.Projects.Transverse).OptedOut(cwd, paths); !pause && out {
					res.StillExcluded = why
				}
				return e.emit(name, res, func() string { return pauseText(res) })
			}
			if sid == "" {
				sid = os.Getenv("BACKSIGHT_SESSION")
			}
			if sid == "" {
				return &output.Error{Code: output.Usage, Message: "no session: run it inside a Claude Code session (the /backsight:pause skill), or give --session"}
			}
			if !session.ValidID(sid) {
				return &output.Error{Code: output.Usage, Message: "invalid session id " + sid}
			}
			res.Session = sid
			store := session.Store{Dir: d.SessionsDir(), Locks: d.LocksDir(), Retry: time.Second}
			err = store.Update(sid, func(st *session.State) error {
				res.Changed = st.SetPaused(pause, pauseStart(*st, pause, now()))
				return nil
			})
			if err != nil {
				return blocked(err)
			}
			return e.emit(name, res, func() string { return pauseText(res) })
		},
	}
	cmd.Flags().StringVar(&sid, "session", "", "session id (default: $BACKSIGHT_SESSION)")
	cmd.Flags().BoolVar(&proj, "project", false, "the whole project: add (pause) or remove (resume) its directory in privacy.exclude_paths")
	cmd.Flags().BoolVar(&plan, "plan", false, "with --project: show the config change without writing it")
	return cmd
}

func pauseText(r pauseResult) string {
	state := map[bool]string{true: "paused", false: "recording"}[r.Paused]
	// The state's emoji leads, as in the session banner; a plan changes
	// nothing and carries none.
	emoji := map[bool]string{true: instructions.EmojiNotRecorded, false: instructions.EmojiOn}[r.Paused]
	already := "already "
	if r.StillExcluded != "" {
		state, emoji, already = "still not recorded: "+r.StillExcluded, instructions.EmojiNotRecorded, ""
	}
	switch {
	case r.Planned:
		return fmt.Sprintf("project %s: would be %s (privacy.exclude_paths change not written: --plan)\n", r.Path, map[bool]string{true: "paused", false: "recording"}[r.Paused])
	case r.Path != "" && r.Changed:
		return fmt.Sprintf("%s project %s: %s (privacy.exclude_paths updated)\n", emoji, r.Path, state)
	case r.Path != "":
		return fmt.Sprintf("%s project %s: %s%s\n", emoji, r.Path, already, state)
	case r.Changed:
		return fmt.Sprintf("%s session %s: %s\n", emoji, r.Session, state)
	}
	return fmt.Sprintf("%s session %s: already %s\n", emoji, r.Session, state)
}

// pauseStart is where a pause's stretch begins: at the prompt that asked
// for it ("don't record this: …" is itself private) when that prompt is
// recent, with a margin for the transcript's own timestamp; else now. A
// resume ends the stretch now.
func pauseStart(st session.State, pause bool, at time.Time) time.Time {
	if pause && !st.LastPromptAt.IsZero() && at.Sub(st.LastPromptAt) < 30*time.Minute {
		return st.LastPromptAt.Add(-5 * time.Second)
	}
	return at
}
