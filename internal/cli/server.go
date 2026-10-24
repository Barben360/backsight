package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
)

// Seams replaced by tests.
var (
	composeRunner server.Runner = server.DockerRunner{}
	healthPoll                  = 2 * time.Second
	healthTimeout               = 10 * time.Minute // first start downloads models
)

func composeFor(cfg config.Config) server.Compose {
	return server.Compose{Dir: cfg.Server.ComposeDir, Runner: composeRunner}
}

func blocked(err error) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		return err
	}
	return &output.Error{Code: output.Blocked, Message: err.Error()}
}

// waitHealthy polls /health until it answers or healthTimeout passes.
func waitHealthy(ctx context.Context, cfg config.Config, sec config.Secrets) (hindsight.Version, error) {
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 5*time.Second)
	if err != nil {
		return hindsight.Version{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	for {
		if _, err := cl.Health(ctx); err == nil {
			return cl.Version(ctx)
		}
		select {
		case <-ctx.Done():
			return hindsight.Version{}, fmt.Errorf("the server did not report health at %s within %s: check backsight server status and docker compose logs", cfg.Server.URL, healthTimeout)
		case <-time.After(healthPoll):
		}
	}
}

type step struct {
	Name   string `json:"name"`
	Status string `json:"status"` // done | skipped | planned
	Detail string `json:"detail,omitempty"`
}

type installResult struct {
	Steps   []step `json:"steps"`
	URL     string `json:"url,omitempty"`
	UI      string `json:"ui,omitempty"`
	Version string `json:"version,omitempty"`
}

func stepsText(r installResult) string {
	var b strings.Builder
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "%-8s %s", s.Status, s.Name)
		if s.Detail != "" {
			fmt.Fprintf(&b, ": %s", s.Detail)
		}
		b.WriteString("\n")
	}
	if r.Version != "" {
		fmt.Fprintf(&b, "\nHindsight %s healthy at %s (UI %s)\n", r.Version, r.URL, r.UI)
	}
	return b.String()
}

type installFlags struct {
	apiPort, uiPort       int
	provider, model, base string
	plan                  bool
}

// initialConfig is the config.toml server install writes when none exists.
func initialConfig(f installFlags) []byte {
	var b strings.Builder
	b.WriteString("# Written by backsight server install.\nschema_version = 1\n\n[server]\n")
	fmt.Fprintf(&b, "url = \"http://127.0.0.1:%d\"\nui_port = %d\n", f.apiPort, f.uiPort)
	if f.provider != "" || f.model != "" || f.base != "" {
		b.WriteString("\n[llm]\n")
		if f.provider != "" {
			fmt.Fprintf(&b, "provider = %s\n", strconv.Quote(f.provider))
		}
		if f.model != "" {
			fmt.Fprintf(&b, "model = %s\n", strconv.Quote(f.model))
		}
		if f.base != "" {
			fmt.Fprintf(&b, "base_url = %s\n", strconv.Quote(f.base))
		}
	}
	return []byte(b.String())
}

// conflicts lists flags that disagree with an existing config.
func conflicts(cmd *cobra.Command, cfg config.Config, f installFlags) []string {
	var out []string
	if cmd.Flags().Changed("api-port") {
		if p, _ := cfg.APIPort(); p != f.apiPort {
			out = append(out, fmt.Sprintf("server.url has port %d, --api-port says %d", p, f.apiPort))
		}
	}
	if cmd.Flags().Changed("ui-port") && cfg.Server.UIPort != f.uiPort {
		out = append(out, fmt.Sprintf("server.ui_port is %d, --ui-port says %d", cfg.Server.UIPort, f.uiPort))
	}
	for flag, pair := range map[string][2]string{"provider": {cfg.LLM.Provider, f.provider}, "model": {cfg.LLM.Model, f.model}, "base-url": {cfg.LLM.BaseURL, f.base}} {
		if cmd.Flags().Changed(flag) && pair[0] != pair[1] {
			out = append(out, fmt.Sprintf("llm %s is %q, --%s says %q", flag, pair[0], flag, pair[1]))
		}
	}
	return out
}

func newServerInstallCmd(e *env) *cobra.Command {
	var f installFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the local Hindsight server (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			var res installResult
			add := func(name, status, detail string) { res.Steps = append(res.Steps, step{name, status, detail}) }
			if f.plan {
				for _, s := range []string{"config", "secrets", "render", "pull", "start", "health"} {
					add(s, "planned", "")
				}
				return e.emit("server install", res, func() string { return stepsText(res) })
			}

			// 1. Config.
			cfg, err := config.Load(d.ConfigFile(), d, home)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				if err := fsutil.ReplaceIfUnchanged(d.ConfigFile(), initialConfig(f), 0o600, fsutil.Absent); err != nil {
					return blocked(err)
				}
				if cfg, err = config.Load(d.ConfigFile(), d, home); err != nil {
					_ = os.Remove(d.ConfigFile())
					return &output.Error{Code: output.Usage, Message: err.Error()}
				}
				add("config", "done", "written to "+d.ConfigFile())
			case err != nil:
				return blocked(err)
			default:
				if c := conflicts(cmd, cfg, f); len(c) > 0 {
					return &output.Error{Code: output.Usage, Message: "config " + d.ConfigFile() + " already exists and differs: " + strings.Join(c, "; ")}
				}
				add("config", "skipped", "kept "+d.ConfigFile())
			}

			// 2. Secrets: server keys generated, the LLM key is the user's.
			var made []string
			if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { made = config.GenerateMissing(s); return nil }); err != nil {
				return blocked(err)
			}
			if len(made) > 0 {
				add("secrets", "done", "generated "+strings.Join(made, ", "))
			} else {
				add("secrets", "skipped", "all present")
			}
			sec, err := config.LoadSecrets(d.SecretsFile())
			if err != nil {
				return blocked(err)
			}

			// 3. Render.
			files, err := renderFiles(cfg, sec)
			if err != nil {
				var oe *output.Error
				if errors.As(err, &oe) && oe.Code == output.NeedsUser {
					oe.Message += "; then rerun backsight server install"
				}
				return err
			}
			ch, err := server.Write(cfg.Server.ComposeDir, files)
			if err != nil {
				return blocked(err)
			}
			add("render", map[bool]string{true: "done", false: "skipped"}[ch.Compose || ch.Env], cfg.Server.ComposeDir)

			// 4-6. Pull, start, health.
			c := composeFor(cfg)
			ctx := cmd.Context()
			if err := c.Pull(ctx); err != nil {
				return blocked(err)
			}
			add("pull", "done", "")
			if err := c.Up(ctx); err != nil {
				return blocked(err)
			}
			add("start", "done", "project "+server.Project)
			v, err := waitHealthy(ctx, cfg, sec)
			if err != nil {
				return blocked(err)
			}
			add("health", "done", "healthy")
			res.URL, res.UI, res.Version = cfg.Server.URL, fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.UIPort), v.APIVersion
			return e.emit("server install", res, func() string { return stepsText(res) })
		},
	}
	cmd.Flags().IntVar(&f.apiPort, "api-port", 8888, "API port published on the host")
	cmd.Flags().IntVar(&f.uiPort, "ui-port", 9999, "control plane port published on the host")
	cmd.Flags().StringVar(&f.provider, "provider", "", "Hindsight LLM provider (default deepseek)")
	cmd.Flags().StringVar(&f.model, "model", "", "LLM model (default deepseek-v4-flash)")
	cmd.Flags().StringVar(&f.base, "base-url", "", "LLM base URL for compatible endpoints")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "list the steps without acting")
	return cmd
}

func newServerStartCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Render if needed and start the local server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			files, err := renderFiles(cfg, sec)
			if err != nil {
				return err
			}
			if _, err := server.Write(cfg.Server.ComposeDir, files); err != nil {
				return blocked(err)
			}
			if err := composeFor(cfg).Up(cmd.Context()); err != nil {
				return blocked(err)
			}
			return e.emit("server start", map[string]string{"project": server.Project}, func() string { return "started\n" })
		},
	}
}

func newServerStopCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the local server (containers and data are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadServerConfig()
			if err != nil {
				return err
			}
			if err := composeFor(cfg).Stop(cmd.Context()); err != nil {
				return blocked(err)
			}
			return e.emit("server stop", map[string]string{"project": server.Project}, func() string { return "stopped\n" })
		},
	}
}

type statusResult struct {
	Containers []server.Container `json:"containers"`
	API        struct {
		URL     string `json:"url"`
		Healthy bool   `json:"healthy"`
		Version string `json:"version,omitempty"`
		Error   string `json:"error,omitempty"`
	} `json:"api"`
}

func newServerStatusCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the local server's containers and API health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			var res statusResult
			res.Containers, err = composeFor(cfg).PS(cmd.Context())
			if err != nil {
				return blocked(err)
			}
			res.API.URL = cfg.Server.URL
			if cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 5*time.Second); err == nil {
				if _, err := cl.Health(cmd.Context()); err != nil {
					res.API.Error = err.Error()
				} else if v, err := cl.Version(cmd.Context()); err == nil {
					res.API.Healthy, res.API.Version = true, v.APIVersion
				}
			}
			var failed error
			if !res.API.Healthy {
				failed = &output.Error{Code: output.Blocked, Message: "the server is not running or not healthy"}
			}
			return e.report("server status", res, func() string { return statusText(res) }, failed)
		},
	}
}

func statusText(r statusResult) string {
	var b strings.Builder
	if len(r.Containers) == 0 {
		b.WriteString("no container: the server is not running (backsight server start)\n")
	}
	for _, c := range r.Containers {
		fmt.Fprintf(&b, "%-10s %s %s\n", c.Service, c.State, c.Health)
	}
	if r.API.Healthy {
		fmt.Fprintf(&b, "API %s healthy, Hindsight %s\n", r.API.URL, r.API.Version)
	} else {
		fmt.Fprintf(&b, "API %s not running or not healthy\n", r.API.URL)
	}
	return b.String()
}

func newServerCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "server", Short: "Manage the local Hindsight server", Args: cobra.NoArgs}
	cmd.AddCommand(newServerRenderCmd(e), newServerInstallCmd(e), newServerStartCmd(e), newServerStopCmd(e), newServerStatusCmd(e))
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
