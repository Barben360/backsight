package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/server"
)

// nativeProc is the /proc the native guard reads; tests replace it.
var nativeProc = "/proc"

type nativeCheckResult struct {
	Instance string `json:"instance"`
	Status   string `json:"status"` // recorded | first-install
	Stopped  []int  `json:"stopped,omitempty"`
}

// newServerNativeCheckCmd is the native db unit's ExecStartPre (decision
// 0078), so it runs at every start, at boot and on Restart=: it refuses a
// missing, empty, foreign or unrecorded instance (requirement 2), then
// stops any postgres on the instance's data outside the db unit, never
// adopting it (requirement 3). It reads only the config and the files, and
// runs only the bundled pg_controldata and pg_ctl; its output goes to the
// journal.
func newServerNativeCheckCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:    "native-check",
		Short:  "Check the native database before its unit starts it (the db unit's ExecStartPre)",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			// Built whatever server.runtime says: a switch to native starts
			// the units before config.toml names native.
			n := server.Native{Cfg: cfg, Dirs: d, Sys: nativeSys, Tools: nativeTools, Proc: nativeProc}
			ctx := cmd.Context()
			res := nativeCheckResult{Instance: n.InstanceDir(), Status: "recorded"}
			if err := n.CheckInstance(ctx); err != nil {
				return blocked(err)
			}
			if _, err := os.Lstat(n.InstanceDir()); err != nil {
				res.Status = "first-install"
			}
			strays, err := n.Strays()
			if err != nil {
				return blocked(fmt.Errorf("looking for a postgres on %s outside %s: %w", n.DataDir(), n.DBUnit(), err))
			}
			for _, s := range strays {
				fmt.Fprintf(e.stderr, "a postgres (pid %d) runs on %s outside %s: stopping it, never adopting it\n", s.PID, n.DataDir(), n.DBUnit())
				res.Stopped = append(res.Stopped, s.PID)
			}
			if err := n.StopStrays(ctx, strays); err != nil {
				return blocked(err)
			}
			return e.emit("server native-check", res, func() string {
				if res.Status == "first-install" {
					return "no native database yet: its first start creates it\n"
				}
				return "the native database " + res.Instance + " is the one recorded\n"
			})
		},
	}
}
