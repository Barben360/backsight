package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/session"
)

type pauseResult struct {
	Paused  bool   `json:"paused"`
	Session string `json:"session,omitempty"`
	Path    string `json:"path,omitempty"` // --project: the excluded directory
	Changed bool   `json:"changed"`
}

// newPauseCmd is `pause [--project]` and `resume [--project]` (SPEC §13
// opt-out, decision 0056): a session flag every writer reads, or a project
// path in privacy.exclude_paths.
func newPauseCmd(e *env, pause bool) *cobra.Command {
	var sid string
	var proj bool
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
			if proj {
				cwd, _ := os.Getwd()
				cfgPath := d.ConfigFile()
				raw, sha, err := fsutil.ReadHashed(cfgPath)
				if err != nil {
					return blocked(err)
				}
				cfg, err := config.Load(cfgPath, d, home)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return &output.Error{Code: output.Usage, Message: err.Error()}
				}
				res.Path = project.New(home, cfg.Projects.Transverse).Root(cwd)
				paths := slices.Clone(cfg.Privacy.ExcludePaths)
				has := slices.Contains(paths, res.Path)
				switch {
				case pause && !has:
					paths = append(paths, res.Path)
				case !pause && has:
					paths = slices.DeleteFunc(paths, func(p string) bool { return p == res.Path })
				default:
					return e.emit(name, res, func() string { return pauseText(res) })
				}
				if sha == fsutil.Absent {
					raw = []byte("schema_version = 1\n")
				}
				out, err := config.SetExcludePaths(raw, paths)
				if err != nil {
					return &output.Error{Code: output.Usage, Message: err.Error()}
				}
				if _, err := config.Parse(out, cfgPath, d, home); err != nil {
					return &output.Error{Code: output.Usage, Message: "the edited config would not load: " + err.Error()}
				}
				if err := fsutil.ReplaceIfUnchanged(cfgPath, out, 0o600, sha); err != nil {
					return blocked(err)
				}
				res.Changed = true
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
				res.Changed = st.Paused != pause
				st.Paused = pause
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
	return cmd
}

func pauseText(r pauseResult) string {
	state := map[bool]string{true: "paused", false: "recording"}[r.Paused]
	switch {
	case r.Path != "" && r.Changed:
		return fmt.Sprintf("project %s: %s (privacy.exclude_paths updated)\n", r.Path, state)
	case r.Path != "":
		return fmt.Sprintf("project %s: already %s\n", r.Path, state)
	case r.Changed:
		return fmt.Sprintf("session %s: %s\n", r.Session, state)
	}
	return fmt.Sprintf("session %s: already %s\n", r.Session, state)
}
