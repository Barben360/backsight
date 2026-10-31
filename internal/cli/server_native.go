package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/supervise"
)

// nativeProc is the /proc the native guard reads; nativeStrayWait bounds
// its wait for a stray to stop (0: the runtime's 30 s). Tests replace both.
var (
	nativeProc      = "/proc"
	nativeStrayWait time.Duration
)

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
			n := server.Native{Cfg: cfg, Dirs: d, Sys: nativeSys, Tools: nativeTools, Proc: nativeProc, StrayWait: nativeStrayWait}
			res, err := nativeGuard(cmd.Context(), n, e.stderr)
			if err != nil {
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

// nativeGuard is native-check's guard, which the supervisor also runs
// before every start of the database: the identity check (a refusal is a
// *server.GuardError, final for the supervisor), then the strays on the
// instance's data stopped, never adopted. Its errors are raw: the caller
// wraps them.
func nativeGuard(ctx context.Context, n server.Native, stderr io.Writer) (nativeCheckResult, error) {
	res := nativeCheckResult{Instance: n.InstanceDir(), Status: "recorded"}
	if err := n.CheckInstance(ctx); err != nil {
		return res, err
	}
	if _, err := os.Lstat(n.InstanceDir()); err != nil {
		res.Status = "first-install"
	}
	strays, err := n.Strays()
	if err != nil {
		return res, fmt.Errorf("looking for a postgres on %s outside %s: %w", n.DataDir(), n.DBUnit(), err)
	}
	for _, s := range strays {
		fmt.Fprintf(stderr, "a postgres (pid %d) runs on %s outside %s: stopping it, never adopting it\n", s.PID, n.DataDir(), n.DBUnit())
		res.Stopped = append(res.Stopped, s.PID)
	}
	return res, n.StopStrays(ctx, strays)
}

// superviseGuard is the supervisor's guard before every start of the
// database: native-check's. A testhooks build on Windows replaces it
// until Task 10 (supervise_guard_testhooks_windows.go).
var superviseGuard = func(ctx context.Context, n server.Native, stderr io.Writer) error {
	_, err := nativeGuard(ctx, n, stderr)
	return err
}

// newServerSuperviseCmd is the Windows task's action (decision 0080):
// conhost.exe --headless <shim> server supervise. It runs the native
// server from <Root>\supervise.json until a stop on its pipe; a second
// one of the project exits 0 at once. --backsight-home carries
// BACKSIGHT_HOME, which a task cannot set.
func newServerSuperviseCmd(e *env) *cobra.Command {
	var noAPI bool
	var home string
	cmd := &cobra.Command{
		Use:    "supervise",
		Short:  "Run the native server's processes (the Windows task's action)",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if home != "" {
				if err := os.Setenv("BACKSIGHT_HOME", home); err != nil {
					return blocked(err)
				}
			}
			cfg, _, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			n := server.Native{Cfg: cfg, Dirs: d, Sys: nativeSys, Tools: nativeTools, Proc: nativeProc, StrayWait: nativeStrayWait}
			path := n.SuperviseFile()
			raw, err := os.ReadFile(path)
			if err != nil {
				return blocked(fmt.Errorf("the supervisor's configuration: %w (run backsight server render)", err))
			}
			var sc supervise.Config
			if err := json.Unmarshal(raw, &sc); err != nil {
				return blocked(fmt.Errorf("%s: %w (run backsight server render)", path, err))
			}
			want := cfg.Server.Project
			if want == "" {
				want = server.Project
			}
			if sc.Project != want {
				return blocked(fmt.Errorf("%s supervises project %q, config.toml names %q: run backsight server render", path, sc.Project, want))
			}
			opts := supervise.Options{
				Check:   func(ctx context.Context) error { return superviseGuard(ctx, n, e.stderr) },
				NoAPI:   noAPI,
				Secrets: localSecretValues(),
				Version: buildinfo.Version,
			}
			if err := supervise.Run(cmd.Context(), sc, opts); err != nil {
				return blocked(err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noAPI, "no-api", false, "start the database alone (the API on api-start)")
	cmd.Flags().StringVar(&home, "backsight-home", "", "backsight's directory (BACKSIGHT_HOME), which a scheduled task cannot set")
	return cmd
}
