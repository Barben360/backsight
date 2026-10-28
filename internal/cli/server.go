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

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
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

// runtimeFor is the server runtime of cfg (decision 0070): Docker Compose.
func runtimeFor(cfg config.Config) server.Runtime {
	return server.Docker{Compose: composeFor(cfg), Cfg: cfg}
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
func waitHealthy(ctx context.Context, rt server.Runtime, cfg config.Config, sec config.Secrets) (hindsight.Version, error) {
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
		if list, err := rt.Status(ctx); err == nil {
			for _, ct := range list {
				if ct.Service == "hindsight" && (ct.State == "exited" || ct.State == "restarting" || ct.State == "dead") {
					logs, _ := rt.Logs(ctx, "hindsight", 20)
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
	// Written out: a config.toml without it means the legacy bank.
	fmt.Fprintf(&b, "bank = %s\n", strconv.Quote(config.DefaultBank))
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

// sshFlags are server install's flags for another machine: without
// --ssh they mean nothing.
var sshFlags = []string{"private", "source", "llm-key-from-env", "llm-key-from-file", "answer", "non-interactive", "config-dir"}

func newServerInstallCmd(e *env) *cobra.Command {
	var f installFlags
	var sf sshInstallFlags
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the local Hindsight server (idempotent), or with --ssh one on another machine",
		Long: "--ssh user@host installs the memory server on that machine over the system ssh, with your keys and known_hosts, " +
			"and joins it from this PC; it works from a client too. The LLM key goes from --llm-key-from-env, --llm-key-from-file " +
			"or a hidden prompt to the host on the stdin of secret set; the connection token comes back over ssh and is never shown. " +
			"Every step can run again: a rerun goes on from where a run stopped. --print-commands prints those steps for a host you have no ssh access to, and runs nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if printOnly {
				sf.install, sf.plan, sf.changed = f, f.plan, cmd.Flags().Changed
				res, err := printCommands(sf, buildinfo.Version)
				if err != nil {
					return err
				}
				return e.emit("server install", res, func() string { return printedCommandsText(res) })
			}
			if cmd.Flags().Changed("ssh") {
				sf.install, sf.plan, sf.changed = f, f.plan, cmd.Flags().Changed
				res, err := runSSHInstall(cmd.Context(), e, sf)
				if len(res.Steps) == 0 {
					return err
				}
				return e.report("server install", res, func() string { return sshInstallText(res) }, err)
			}
			for _, fl := range sshFlags {
				if cmd.Flags().Changed(fl) {
					return &output.Error{Code: output.Usage, Message: "--" + fl + " goes with --ssh user@host (it installs the server on another machine) or --print-commands"}
				}
			}
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
			if err := requireLocal(cfg, "server install"); err != nil {
				return err
			}
			rt := runtimeFor(cfg)
			if err := pinGuard(cfg, rt); err != nil {
				return err
			}

			kept, err := server.KeptSecrets(cfg.Server.ComposeDir)
			if err != nil {
				return blocked(err)
			}
			if f.plan {
				prev, err := config.LoadSecrets(d.SecretsFile())
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return blocked(err)
				}
				adopted := adoptKept(&prev, kept)
				gen := config.GenerateMissing(&prev)
				var plans []string
				if len(adopted) > 0 {
					plans = append(plans, "would adopt "+strings.Join(adopted, ", ")+" from the kept "+keptEnv(cfg))
				}
				if len(gen) > 0 {
					plans = append(plans, "would generate "+strings.Join(gen, ", "))
				}
				add("secrets", "planned", map[bool]string{true: strings.Join(plans, "; "), false: "all server keys present"}[len(plans) > 0])
				if req := config.RequiredSecrets(cfg); req["llm_api_key"] && prev.LLMAPIKey == "" {
					return needSecrets([]string{"llm_api_key"}, "backsight server install")
				}
				for _, s := range []string{"render", "pull", "start", "health", "bank", "server-id", "provider"} {
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
			// lost db_password cannot be regenerated against it. A kept
			// .env (uninstall --purge-local keeps the server) gives back the
			// keys secrets.toml lacks; a secrets.toml value always wins.
			ctx := cmd.Context()
			if prev, err := config.LoadSecrets(d.SecretsFile()); (err == nil || errors.Is(err, fs.ErrNotExist)) && prev.DBPassword == "" && !adoptable(kept, "db_password") {
				exists, err := rt.DataExists(ctx)
				if err != nil {
					return blocked(err)
				}
				if exists {
					return &output.Error{Code: output.Blocked, Message: rt.DataName() +
						" exists but neither secrets.toml nor " + keptEnv(cfg) + " holds its db_password: restore the secrets.toml it was created with, " +
						"or set the password it was created with (backsight secret set db_password); last, if its memories may go, delete it (" +
						rt.DataRemoveCmd() + ") before installing again"}
				}
			}
			var made, adopted []string
			if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error {
				adopted = adoptKept(s, kept)
				made = config.GenerateMissing(s)
				return nil
			}); err != nil {
				return blocked(err)
			}
			var did []string
			if len(adopted) > 0 {
				did = append(did, "adopted "+strings.Join(adopted, ", ")+" from the kept "+keptEnv(cfg))
			}
			if len(made) > 0 {
				did = append(did, "generated "+strings.Join(made, ", "))
			}
			if len(did) > 0 {
				add("secrets", "done", strings.Join(did, "; "))
			} else {
				add("secrets", "skipped", "all present")
			}
			sec, err := config.LoadSecrets(d.SecretsFile())
			if err != nil {
				return blocked(err)
			}

			// 3. Render.
			files, err := renderOn(rt, sec, "backsight server install")
			if err != nil {
				return err
			}
			ch, err := rt.Write(files)
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
			if err := rt.FetchAll(ctx); err != nil {
				return fail(redacted(err, sec))
			}
			add("pull", "done", "")
			if err := rt.Start(ctx); err != nil {
				return fail(redacted(err, sec))
			}
			detail := "project " + cfg.Server.Project
			if _, err := os.Stat(filepath.Join(cfg.Server.ComposeDir, "compose.override.yaml")); err == nil {
				detail += ", with your compose.override.yaml"
			}
			add("start", "done", detail)
			v, err := waitHealthy(ctx, rt, cfg, sec)
			if err != nil {
				return fail(redacted(err, sec))
			}
			add("health", "done", "healthy")
			br, err := applyBank(cmd.Context(), d, cfg, sec, false)
			if err != nil {
				return fail(err)
			}
			add("bank", map[bool]string{true: "done", false: "skipped"}[br.Sent], strings.TrimSpace(bankText(br)))
			// After the bank step (it may rename a legacy bank). A failure is a
			// warning: the first connection token made writes the id.
			if status, detail := ensureServerID(cmd.Context(), cfg, sec); status == "warning" {
				res.Warnings = append(res.Warnings, "server id: "+detail+" (backsight server install again writes it; "+
					"so does the first connection token, from backsight server expose --private or backsight server token once the server is exposed)")
				add("server-id", "warning", detail)
			} else {
				add("server-id", status, detail)
			}
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
	cmd.Flags().StringVar(&f.model, "model", "", "LLM model (default deepseek-flash)")
	cmd.Flags().StringVar(&f.base, "base-url", "", "LLM base URL for compatible endpoints")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "list the steps without acting")
	cmd.Flags().StringVar(&sf.target, "ssh", "", "install the server on this machine instead ([user@]host, as ssh takes it) and join it from this PC")
	cmd.Flags().BoolVar(&printOnly, "print-commands", false, "print the commands that install the server on another machine by hand (a host you have no ssh access to), and run nothing")
	cmd.Flags().StringVar(&sf.private, "private", "", "with --ssh or --print-commands: open the server on this LAN or tailnet address of the host (--ssh default: the address ssh reached)")
	cmd.Flags().StringVar(&sf.source, "source", "", "with --ssh: read the release from this local release directory")
	cmd.Flags().StringVar(&sf.keyEnv, "llm-key-from-env", "", "with --ssh: read the host's LLM API key from this environment variable")
	cmd.Flags().StringVar(&sf.keyFile, "llm-key-from-file", "", "with --ssh: read the host's LLM API key from the first line of this file")
	cmd.Flags().StringArrayVar(&sf.answers, "answer", nil, "with --ssh: consent=yes to go ahead without a terminal; with --ssh or --print-commands: setup's answers for the host or this PC (id=value, repeatable)")
	cmd.Flags().BoolVar(&sf.nonInteractive, "non-interactive", false, "with --ssh: never prompt: exit 10 when the consent, the key or another answer is missing")
	cmd.Flags().StringVar(&sf.configDir, "config-dir", "", "with --ssh: this PC's Claude Code directory (default: $CLAUDE_CONFIG_DIR, else ~/.claude)")
	return cmd
}

// ensureServerID gives the server its id (retain.EnsureServerID) and
// reports it as an install step: done (written now), unchanged (already
// there) or warning (the detail says why; the install goes on).
func ensureServerID(ctx context.Context, cfg config.Config, sec config.Secrets) (status, detail string) {
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err == nil {
		var had, id string
		if had, err = retain.ReadServerID(ctx, cl, cfg.Server.Bank); err == nil {
			if id, err = retain.EnsureServerID(ctx, cl, cfg.Server.Bank); err == nil {
				if had != "" {
					return "unchanged", id
				}
				return "done", id
			}
		}
	}
	return "warning", redacted(err, sec).Error()
}

// keptEnv is the server's .env, as messages name it.
func keptEnv(cfg config.Config) string { return filepath.Join(cfg.Server.ComposeDir, ".env") }

// adoptable: kept holds a value for name that secrets.toml would take.
func adoptable(kept map[string]string, name string) bool {
	var s config.Secrets
	return kept[name] != "" && s.Set(name, kept[name]) == nil
}

// adoptKept sets each server key s lacks from kept (a kept .env), never
// one s holds, and returns the names it set, sorted. A value secrets.toml
// would refuse (a db_password outside its alphabet) is left out.
func adoptKept(s *config.Secrets, kept map[string]string) []string {
	var out []string
	for _, name := range []string{"api_key", "db_password", "ui_key"} {
		if s.Names()[name] || !adoptable(kept, name) {
			continue
		}
		_ = s.Set(name, kept[name])
		out = append(out, name)
	}
	return out
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
			if err := requireLocal(cfg, "server start"); err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			// A restore that wrote its epoch but did not start the server
			// on its API key again is finished here, on the image its data
			// is for (a server upgrade's rollback put back the version
			// before, which this binary's pin would refuse): the marker
			// goes once the server is healthy on that key (issue 285).
			m, _ := state.ReadRestoreMarker(d)
			purgeStep := m != nil && m.Step == state.PurgeStep
			keyStep := m != nil && (m.Step == state.KeyStep || purgeStep)
			// The marker's image counts only when it is the one the server's
			// files deploy (what restore or its rollback rendered last);
			// anything else goes through the pin guard (issue 285 review
			// LOW-2).
			rt := runtimeFor(cfg)
			deployed, _ := rt.Deployed()
			if keyStep && m.Image != "" && m.Image == deployed {
				rt = rt.WithVersion(m.Image)
			} else if err := pinGuard(cfg, rt); err != nil {
				return err
			}
			files, err := renderOn(rt, sec, "backsight server start")
			if err != nil {
				return err
			}
			if plan {
				ch, err := rt.Diff(files)
				if err != nil {
					return blocked(err)
				}
				return e.emit("server start", map[string]any{"project": cfg.Server.Project, "plan": true, "compose_changed": ch.Compose, "env_changed": ch.Env},
					func() string {
						return renderText(renderResult{Dir: cfg.Server.ComposeDir, Changes: ch, Plan: true}) + "would run docker compose up\n"
					})
			}
			// A maintenance lock whose owner is dead (its PID gone, or reused
			// by a process started after the lock) is taken over: nothing
			// else removes it (issue 285).
			var notes []string
			if h := state.Inspect(d.MaintenanceLock()); h != nil && h.Stale && state.BreakStale(h) == nil {
				notes = append(notes, fmt.Sprintf("removed a maintenance lock left by %q (pid %d, since %s), whose process is gone",
					h.Owner.Command, h.Owner.PID, h.Owner.Since.Format(time.RFC3339)))
			}
			unlock, err := serverLockFor(d, "server start", keyStep)
			if err != nil {
				return err
			}
			defer unlock()
			// The marker again, under the locks: what it says now decides
			// (a restore may have moved on since the first read).
			if m, _ = state.ReadRestoreMarker(d); m == nil || (m.Step != state.PurgeStep && m.Step != state.KeyStep) {
				if keyStep && m != nil {
					return refuseWhileBusy(d)
				}
				purgeStep, keyStep = false, false
			} else {
				purgeStep, keyStep = m.Step == state.PurgeStep, true
			}
			if purgeStep {
				note, err := resumePurge(cmd.Context(), e, cfg, sec, rt, d, *m)
				if err != nil {
					return err
				}
				notes = append(notes, note)
			}
			if _, err := rt.Write(files); err != nil {
				return blocked(err)
			}
			if err := rt.Start(cmd.Context()); err != nil {
				return redacted(err, sec)
			}
			if keyStep {
				if _, err := waitHealthy(cmd.Context(), rt, cfg, sec); err != nil {
					return redacted(fmt.Errorf("started, but not healthy on its API key (%w): the "+m.Command+" stays unfinished (%s)", err, d.RestoreMarker()), sec)
				}
				if err := state.ClearRestoreMarker(d); err != nil {
					return blocked(err)
				}
				notes = append(notes, "the "+m.Command+" of "+m.Archive+" is finished: the server runs on its API key again")
			}
			out := map[string]any{"project": cfg.Server.Project}
			if len(notes) > 0 {
				out["notes"] = notes
			}
			return e.emit("server start", out, func() string {
				text := "started\n"
				for _, n := range notes {
					text += "note: " + n + "\n"
				}
				return text
			})
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
			if err := requireLocal(cfg, "server stop"); err != nil {
				return err
			}
			if plan {
				return e.emit("server stop", map[string]any{"project": cfg.Server.Project, "plan": true},
					func() string { return "would stop the containers of project " + cfg.Server.Project + " (data kept)\n" })
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			unlock, err := serverLock(d, "server stop")
			if err != nil {
				return err
			}
			defer unlock()
			if err := runtimeFor(cfg).Stop(cmd.Context()); err != nil {
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
			if err := requireLocal(cfg, "server status"); err != nil {
				return err
			}
			var res statusResult
			res.Containers, err = runtimeFor(cfg).Status(cmd.Context())
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
	cmd.AddCommand(newServerRenderCmd(e), newServerInstallCmd(e), newServerStartCmd(e), newServerStopCmd(e), newServerStatusCmd(e), newServerLLMCmd(e), newServerUpgradeCmd(e), newServerTokenCmd(e), newServerExposeCmd(e))
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
		return cfg, config.Secrets{}, &output.Error{Code: output.NeedsUser, Message: "no config at " + d.ConfigFile() + ": run backsight setup"}
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

// pinGuard refuses to deploy this binary's pin over another deployed
// version (decision 0067 block review): server start, install and render
// would move the server without a safety backup or a rollback. server
// upgrade moves it to a newer pin, update apply brings a binary pinning a
// newer deployed version. A digest set in config.toml is the user's own
// choice (restore names it for an archive of another version), and a
// project not deployed yet has nothing to protect.
func pinGuard(cfg config.Config, rt server.Runtime) error {
	if cfg.Server.Digest != "" {
		return nil
	}
	deployed, err := rt.Deployed()
	if err != nil {
		return blocked(err)
	}
	pin := rt.Version()
	if deployed == "" {
		return nil
	}
	cmp, ok := rt.ComparePins(deployed, pin)
	var msg string
	switch {
	case ok && cmp == 0:
		return nil
	case ok && cmp < 0:
		msg = fmt.Sprintf("the server runs %s, older than this binary's %s: run backsight server upgrade (a verified backup first, rolled back on any failure)", deployed, pin)
	case ok:
		msg = fmt.Sprintf("the server runs %s, newer than this binary's %s: run backsight update apply", deployed, pin)
	default:
		msg = fmt.Sprintf("the server runs %s and this binary pins %s, which cannot be ordered: run backsight server upgrade --allow-unordered to deploy this binary's pin, or backsight update apply if the server's is the newer", deployed, pin)
	}
	return &output.Error{Code: output.Blocked, Message: msg + ": nothing was changed"}
}

// renderOn renders the server's files for rt and the version it deploys;
// missing secrets become a needs_user error telling the user what to run
// and which command resumes.
func renderOn(rt server.Runtime, sec config.Secrets, resume string) (server.Files, error) {
	host, _ := os.Hostname()
	f, err := rt.Render(sec, host)
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

// requireLocal refuses a server command on a client of a remote server,
// naming the server and the command to run on its host.
func requireLocal(cfg config.Config, command string) error {
	if cfg.Server.Mode != "local" {
		return &output.Error{Code: output.Usage, Message: "this PC is a client of the server at " + cfg.Server.URL + ": run backsight " + command + " on that server's host"}
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
			if err := requireLocal(cfg, "server render"); err != nil {
				return err
			}
			rt := runtimeFor(cfg)
			if err := pinGuard(cfg, rt); err != nil {
				return err
			}
			f, err := renderOn(rt, sec, "backsight server render")
			if err != nil {
				return err
			}
			res := renderResult{Dir: cfg.Server.ComposeDir, Plan: plan}
			if plan {
				res.Changes, err = rt.Diff(f)
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
				res.Changes, err = rt.Write(f)
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
	var pe *server.ProviderError
	if errors.As(err, &pe) && pe.KeyRejected {
		// The user's to fix: the key, then the same command again.
		return res, &output.Error{Code: output.NeedsUser, Secrets: []string{"llm_api_key"},
			Message: pe.Error() + ": set the right key with backsight secret set llm_api_key",
			Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight secret set llm_api_key", Why: "the provider refused the key the server was given"}}}
	}
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
			if err := requireLocal(cfg, "server llm check"); err != nil {
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

// serverLock serialises commands that rewrite the compose project; none
// runs while maintenance or a restore is in progress (restore takes the
// lock itself).
func serverLock(d paths.Dirs, command string) (func(), error) {
	return serverLockFor(d, command, false)
}

// serverLockFor is serverLock; keyStep lets a restore's marker at its key
// step through (server start finishes that restore).
func serverLockFor(d paths.Dirs, command string, keyStep bool) (func(), error) {
	if held := state.Inspect(d.MaintenanceLock()); held != nil && !held.Stale {
		return nil, &output.Error{Code: output.Blocked, Message: "maintenance in progress: " + held.Error()}
	}
	if err := refuseWhileBusy(d); err != nil && !keyStep {
		return nil, err
	}
	l, _, err := state.Acquire(filepath.Join(d.LocksDir(), "server.lock"), command)
	if err != nil {
		return nil, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return func() { _ = l.Release() }, nil
}

// serverMaintenanceLock takes the maintenance lock and server.lock, as
// server upgrade does, for a command that restarts the server on another
// key or address: hooks queue and jobs skip until it is done or rolled
// back. note names a stale maintenance lock it broke.
func serverMaintenanceLock(d paths.Dirs, command string) (unlock func(), note string, err error) {
	if restoreIncomplete(d) {
		return nil, "", restoreIncompleteError(d)
	}
	ml, note, err := acquireMaintenance(d, command)
	if err != nil {
		return nil, "", err
	}
	sl, _, err := state.Acquire(filepath.Join(d.LocksDir(), "server.lock"), command)
	if err != nil {
		_ = ml.Release()
		return nil, "", &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return func() { _ = sl.Release(); _ = ml.Release() }, note, nil
}

// redacted is blocked() with every known secret value masked: Docker and
// provider output can quote configuration.
func redacted(err error, sec config.Secrets) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		out := *oe
		out.Message = server.Redact(oe.Message, sec)
		return &out
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
