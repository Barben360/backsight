package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
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
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/target"
)

// Seams replaced by tests.
var (
	composeRunner server.Runner = server.DockerRunner{}
	healthPoll                  = 2 * time.Second
	healthTimeout               = 10 * time.Minute // first start downloads models
)

func composeFor(cfg config.Config) server.Compose {
	return server.Compose{Dir: cfg.Server.ComposeDir, Runner: composeRunner, Project: cfg.Server.Project}
}

func blocked(err error) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		return err
	}
	return &output.Error{Code: output.Blocked, Message: err.Error()}
}

// waitHealthy polls /health until it answers or healthTimeout passes, and
// fails early with the last log lines when the hindsight container exits or
// restarts (a crash loop has no health to wait for).
func waitHealthy(ctx context.Context, cfg config.Config, sec config.Secrets) (hindsight.Version, error) {
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 5*time.Second)
	if err != nil {
		return hindsight.Version{}, err
	}
	c := composeFor(cfg)
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	for {
		if _, err := cl.Health(ctx); err == nil {
			return cl.Version(ctx)
		}
		if list, err := c.PS(ctx); err == nil {
			for _, ct := range list {
				if ct.Service == "hindsight" && (ct.State == "exited" || ct.State == "restarting" || ct.State == "dead") {
					logs, _ := c.Logs(ctx, "hindsight", 20)
					return hindsight.Version{}, fmt.Errorf("the hindsight container is %s; last log lines:\n%s", ct.State, strings.TrimSpace(logs))
				}
			}
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
	Steps    []step   `json:"steps"`
	Warnings []string `json:"warnings,omitempty"`
	URL      string   `json:"url,omitempty"`
	UI       string   `json:"ui,omitempty"`
	Version  string   `json:"version,omitempty"`
}

func stepLine(s step) string {
	line := fmt.Sprintf("%-8s %s", s.Status, s.Name)
	if s.Detail != "" {
		line += ": " + s.Detail
	}
	return line + "\n"
}

// installSummary is what text mode prints after the streamed steps.
func installSummary(r installResult) string {
	var b strings.Builder
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning  %s\n", w)
	}
	if r.Version != "" {
		fmt.Fprintf(&b, "\nHindsight %s healthy at %s (UI %s)\n", r.Version, r.URL, r.UI)
	}
	return b.String()
}

// uiURL is the control plane address on the interface it is bound to.
func uiURL(cfg config.Config) string {
	host := "127.0.0.1"
	if cfg.Network.Exposure == "private" {
		host = cfg.Network.BindIP
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Server.UIPort))
}

// configFromFlags validates the config install would write, without writing it.
func configFromFlags(f installFlags, d paths.Dirs, home string) (config.Config, error) {
	tmp, err := os.MkdirTemp("", "backsight-plan-")
	if err != nil {
		return config.Config{}, blocked(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	p := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(p, initialConfig(f), 0o600); err != nil {
		return config.Config{}, blocked(err)
	}
	cfg, err := config.Load(p, d, home)
	if err != nil {
		return cfg, &output.Error{Code: output.Usage, Message: err.Error()}
	}
	return cfg, nil
}

type installFlags struct {
	apiPort, uiPort       int
	provider, model, base string
	project               string // "" or "backsight": the default
	plan                  bool
}

// initialConfig is the config.toml server install writes when none exists.
func initialConfig(f installFlags) []byte {
	var b strings.Builder
	b.WriteString("# Written by backsight server install.\nschema_version = 1\n\n[server]\n")
	fmt.Fprintf(&b, "url = \"http://127.0.0.1:%d\"\nui_port = %d\n", f.apiPort, f.uiPort)
	if f.project != "" && f.project != "backsight" {
		fmt.Fprintf(&b, "project = %s\n", strconv.Quote(f.project))
	}
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
			// Text mode shows each step as it completes; JSON gets them all at the end.
			add := func(name, status, detail string) {
				st := step{name, status, detail}
				res.Steps = append(res.Steps, st)
				if !e.json {
					fmt.Fprint(e.stdout, stepLine(st))
				}
			}

			// 1. Config.
			cfg, err := config.Load(d.ConfigFile(), d, home)
			switch {
			case errors.Is(err, fs.ErrNotExist) && f.plan:
				if cfg, err = configFromFlags(f, d, home); err != nil {
					return err
				}
				add("config", "planned", "would write "+d.ConfigFile())
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
				add("config", map[bool]string{true: "planned", false: "skipped"}[f.plan], "keep "+d.ConfigFile())
			}
			if err := requireLocal(cfg); err != nil {
				return err
			}

			if f.plan {
				prev, err := config.LoadSecrets(d.SecretsFile())
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return blocked(err)
				}
				gen := config.GenerateMissing(&prev)
				add("secrets", "planned", map[bool]string{true: "would generate " + strings.Join(gen, ", "), false: "all server keys present"}[len(gen) > 0])
				if req := config.RequiredSecrets(cfg); req["llm_api_key"] && prev.LLMAPIKey == "" {
					return needSecrets([]string{"llm_api_key"}, "backsight server install")
				}
				for _, s := range []string{"render", "pull", "start", "health", "bank", "provider"} {
					add(s, "planned", "")
				}
				return e.emit("server install", res, func() string { return installSummary(res) })
			}

			// One install, render or start at a time.
			unlock, err := serverLock(d, "server install")
			if err != nil {
				return err
			}
			defer unlock()

			// 2. Secrets: server keys generated, the LLM key is the user's. A
			// database volume keeps the password it was created with, so a
			// lost db_password cannot be regenerated against it.
			c := composeFor(cfg)
			ctx := cmd.Context()
			if prev, err := config.LoadSecrets(d.SecretsFile()); (err == nil || errors.Is(err, fs.ErrNotExist)) && prev.DBPassword == "" {
				exists, err := c.VolumeExists(ctx)
				if err != nil {
					return blocked(err)
				}
				if exists {
					return &output.Error{Code: output.Blocked, Message: "the database volume " + c.DataVolume() +
						" exists but secrets.toml has no db_password: restore the secrets.toml it was created with, or delete the volume and its memories (docker volume rm " +
						c.DataVolume() + ") before installing again"}
				}
			}
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
			files, err := renderFiles(cfg, sec, "backsight server install")
			if err != nil {
				return err
			}
			ch, err := server.Write(cfg.Server.ComposeDir, files)
			if err != nil {
				return blocked(err)
			}
			add("render", map[bool]string{true: "done", false: "skipped"}[ch.Compose || ch.Env], cfg.Server.ComposeDir)

			// 4-6. Pull, start, health. A failure from here keeps the steps
			// done in the envelope (text mode already printed them).
			fail := func(err error) error {
				return e.report("server install", res, func() string { return "" }, err)
			}
			res.Warnings = installWarnings(cfg)
			if err := c.Pull(ctx); err != nil {
				return fail(redacted(err, sec))
			}
			add("pull", "done", "")
			if err := c.Up(ctx); err != nil {
				return fail(redacted(err, sec))
			}
			detail := "project " + cfg.Server.Project
			if _, err := os.Stat(filepath.Join(cfg.Server.ComposeDir, "compose.override.yaml")); err == nil {
				detail += ", with your compose.override.yaml"
			}
			add("start", "done", detail)
			v, err := waitHealthy(ctx, cfg, sec)
			if err != nil {
				return fail(redacted(err, sec))
			}
			add("health", "done", "healthy")
			br, err := applyBank(cmd, d, cfg, sec, false)
			if err != nil {
				return fail(err)
			}
			add("bank", map[bool]string{true: "done", false: "skipped"}[br.Sent], strings.TrimSpace(bankText(br)))
			pr, err := checkProvider(cmd.Context(), cfg, sec)
			if err != nil {
				return fail(err)
			}
			add("provider", "done", fmt.Sprintf("%s/%s extracted %d English fact(s)", cfg.LLM.Provider, cfg.LLM.Model, pr.Facts))
			res.URL, res.UI, res.Version = cfg.Server.URL, uiURL(cfg), v.APIVersion
			return e.emit("server install", res, func() string { return installSummary(res) })
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
	var plan bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Render if needed and start the local server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			if err := requireLocal(cfg); err != nil {
				return err
			}
			files, err := renderFiles(cfg, sec, "backsight server start")
			if err != nil {
				return err
			}
			if plan {
				ch, err := server.Diff(cfg.Server.ComposeDir, files)
				if err != nil {
					return blocked(err)
				}
				return e.emit("server start", map[string]any{"project": cfg.Server.Project, "plan": true, "compose_changed": ch.Compose, "env_changed": ch.Env},
					func() string {
						return renderText(renderResult{Dir: cfg.Server.ComposeDir, Changes: ch, Plan: true}) + "would run docker compose up\n"
					})
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			unlock, err := serverLock(d, "server start")
			if err != nil {
				return err
			}
			defer unlock()
			if _, err := server.Write(cfg.Server.ComposeDir, files); err != nil {
				return blocked(err)
			}
			if err := composeFor(cfg).Up(cmd.Context()); err != nil {
				return redacted(err, sec)
			}
			return e.emit("server start", map[string]string{"project": cfg.Server.Project}, func() string { return "started\n" })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would be rendered and started without doing it")
	return cmd
}

func newServerStopCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the local server (containers and data are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			if err := requireLocal(cfg); err != nil {
				return err
			}
			if plan {
				return e.emit("server stop", map[string]any{"project": cfg.Server.Project, "plan": true},
					func() string { return "would stop the containers of project " + cfg.Server.Project + " (data kept)\n" })
			}
			if err := composeFor(cfg).Stop(cmd.Context()); err != nil {
				return redacted(err, sec)
			}
			return e.emit("server stop", map[string]string{"project": cfg.Server.Project}, func() string { return "stopped\n" })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would be stopped without doing it")
	return cmd
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
			if err := requireLocal(cfg); err != nil {
				return err
			}
			var res statusResult
			res.Containers, err = composeFor(cfg).PS(cmd.Context())
			if err != nil {
				return redacted(err, sec)
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
	cmd.AddCommand(newServerRenderCmd(e), newServerInstallCmd(e), newServerStartCmd(e), newServerStopCmd(e), newServerStatusCmd(e), newServerLLMCmd(e))
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

// renderFiles renders the project; missing secrets become a needs_user
// error telling the user what to run and which command resumes.
func renderFiles(cfg config.Config, sec config.Secrets, resume string) (server.Files, error) {
	host, _ := os.Hostname()
	f, err := server.Render(cfg, sec, host)
	var missing *server.MissingSecretsError
	switch {
	case errors.As(err, &missing):
		return f, needSecrets(missing.Names, resume)
	case err != nil:
		return f, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return f, nil
}

var secretWhy = map[string]string{
	"llm_api_key": "the server's extraction LLM needs its key",
	"api_key":     "clients authenticate to the server with it",
	"ui_key":      "the control plane UI login",
	"db_password": "the database password",
}

func needSecrets(names []string, resume string) error {
	e := &output.Error{Code: output.NeedsUser, Secrets: names, Resume: resume}
	for _, n := range names {
		e.Actions = append(e.Actions, output.Action{Kind: "run_as_user", Cmd: "backsight secret set " + n, Why: secretWhy[n]})
	}
	e.Message = (&server.MissingSecretsError{Names: names}).Error() + "; then rerun " + resume
	return e
}

// requireLocal refuses server commands on a client of a remote server.
func requireLocal(cfg config.Config) error {
	if cfg.Server.Mode != "local" {
		return &output.Error{Code: output.Usage, Message: "server.mode is " + cfg.Server.Mode + ": this command manages a local server (run it on the server machine)"}
	}
	return nil
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
			if err := requireLocal(cfg); err != nil {
				return err
			}
			f, err := renderFiles(cfg, sec, "backsight server render")
			if err != nil {
				return err
			}
			res := renderResult{Dir: cfg.Server.ComposeDir, Plan: plan}
			if plan {
				res.Changes, err = server.Diff(res.Dir, f)
			} else {
				d, _, merr := machine()
				if merr != nil {
					return merr
				}
				unlock, lerr := serverLock(d, "server render")
				if lerr != nil {
					return lerr
				}
				defer unlock()
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

func checkProvider(ctx context.Context, cfg config.Config, sec config.Secrets) (server.ProviderResult, error) {
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 3*time.Minute) // a slow model still answers
	if err != nil {
		return server.ProviderResult{}, blocked(err)
	}
	res, err := server.CheckProvider(ctx, cl, cfg.Server.Bank, cfg.LLM.Provider)
	if err != nil {
		return res, redacted(err, sec) // provider errors may echo a key
	}
	return res, nil
}

func newServerLLMCmd(e *env) *cobra.Command {
	llm := &cobra.Command{Use: "llm", Short: "Check the server's extraction LLM", Args: cobra.NoArgs}
	llm.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Extract facts from a fixed sample without storing them (auth, model, output, language)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			res, err := checkProvider(cmd.Context(), cfg, sec)
			if err != nil {
				return err
			}
			return e.emit("server llm check", res, func() string {
				var b strings.Builder
				fmt.Fprintf(&b, "%s/%s: %d fact(s) in English, %d input and %d output tokens\n", cfg.LLM.Provider, cfg.LLM.Model, res.Facts, res.InputTokens, res.OutputTokens)
				for _, f := range res.Sample {
					fmt.Fprintf(&b, "  - %s\n", f)
				}
				return b.String()
			})
		},
	})
	return llm
}

// serverLock serialises commands that rewrite the compose project.
func serverLock(d paths.Dirs, command string) (func(), error) {
	l, _, err := state.Acquire(filepath.Join(d.LocksDir(), "server.lock"), command)
	if err != nil {
		return nil, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return func() { _ = l.Release() }, nil
}

// redacted is blocked() with every known secret value masked: Docker and
// provider output can quote configuration.
func redacted(err error, sec config.Secrets) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		return &output.Error{Code: oe.Code, Message: server.Redact(oe.Message, sec)}
	}
	return &output.Error{Code: output.Blocked, Message: server.Redact(err.Error(), sec)}
}

func installWarnings(cfg config.Config) []string {
	var w []string
	if h := os.Getenv("DOCKER_HOST"); h != "" && !strings.HasPrefix(h, "unix://") && !strings.HasPrefix(h, "npipe://") {
		w = append(w, "DOCKER_HOST="+h+": the containers and their .env secrets go to that engine, while health checks use "+cfg.Server.URL)
	}
	if (target.Target{URL: cfg.Server.URL}).Cleartext() {
		w = append(w, "the API key travels unencrypted to "+cfg.Server.URL)
	}
	return w
}
