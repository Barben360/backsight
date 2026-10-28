package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
)

type pathsInfo struct {
	paths.Dirs
	ConfigFile    string `json:"config_file"`
	ConfigExists  bool   `json:"config_exists"`
	SecretsFile   string `json:"secrets_file"`
	SecretsExists bool   `json:"secrets_exists"`
	// What the install is, from config.toml: empty when it is absent or
	// does not load.
	Role string `json:"server_role,omitempty"`
	Mode string `json:"server_mode,omitempty"`
}

func newPathsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "paths",
		Short: "Show where backsight keeps its binaries, config and state",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			info := pathsInfo{Dirs: d, ConfigFile: d.ConfigFile(), SecretsFile: d.SecretsFile()}
			info.ConfigExists = exists(info.ConfigFile)
			info.SecretsExists = exists(info.SecretsFile)
			if cfg, err := config.Load(info.ConfigFile, d, home); err == nil {
				info.Role, info.Mode = cfg.Server.Role, cfg.Server.Mode
			}
			return e.emit("paths", info, func() string {
				var b strings.Builder
				for _, row := range [][2]string{{"data", d.Data}, {"config", d.Config}, {"state", d.State}, {"bin", d.Bin},
					{"config.toml", present(info.ConfigFile, info.ConfigExists)}, {"secrets.toml", present(info.SecretsFile, info.SecretsExists)}} {
					fmt.Fprintf(&b, "%-13s %s\n", row[0], row[1])
				}
				return b.String()
			})
		},
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func present(p string, ok bool) string {
	if ok {
		return p
	}
	return p + " (missing)"
}
