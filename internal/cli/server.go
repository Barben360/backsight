package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
)

func newServerCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "server", Short: "Manage the local Hindsight server", Args: cobra.NoArgs}
	cmd.AddCommand(newServerRenderCmd(e))
	return cmd
}

// loadServerConfig reads config and secrets for server commands.
func loadServerConfig() (config.Config, config.Secrets, error) {
	d, home, err := machine()
	if err != nil {
		return config.Config{}, config.Secrets{}, err
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, config.Secrets{}, &output.Error{Code: output.NeedsUser, Message: "no config at " + d.ConfigFile() + ": run backsight server install"}
	}
	if err != nil {
		return cfg, config.Secrets{}, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return cfg, sec, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return cfg, sec, nil
}

func renderFiles(cfg config.Config, sec config.Secrets) (server.Files, error) {
	host, _ := os.Hostname()
	f, err := server.Render(cfg, sec, host)
	var missing *server.MissingSecretsError
	switch {
	case errors.As(err, &missing):
		return f, &output.Error{Code: output.NeedsUser, Message: err.Error()}
	case err != nil:
		return f, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return f, nil
}

type renderResult struct {
	Dir string `json:"dir"`
	server.Changes
	Plan bool `json:"plan"`
}

func newServerRenderCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Write compose.yaml and .env for the local server from config and secrets",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			f, err := renderFiles(cfg, sec)
			if err != nil {
				return err
			}
			res := renderResult{Dir: cfg.Server.ComposeDir, Plan: plan}
			if plan {
				res.Changes, err = server.Diff(res.Dir, f)
			} else {
				res.Changes, err = server.Write(res.Dir, f)
			}
			if err != nil {
				return &output.Error{Code: output.Blocked, Message: err.Error()}
			}
			return e.emit("server render", res, func() string { return renderText(res) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would change without writing")
	return cmd
}

func renderText(r renderResult) string {
	verb := map[bool]string{true: "written", false: "unchanged"}
	if r.Plan {
		verb = map[bool]string{true: "would be written", false: "unchanged"}
	}
	return fmt.Sprintf("%s: %s\n%s: %s\n",
		filepath.Join(r.Dir, "compose.yaml"), verb[r.Compose], filepath.Join(r.Dir, ".env"), verb[r.Env])
}
