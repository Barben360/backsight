package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

func newBankCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "bank", Short: "Manage the memory bank", Args: cobra.NoArgs}
	cmd.AddCommand(newBankApplyCmd(e), newBankRenameCmd(e))
	return cmd
}

// applyBank runs bank apply and records the template hash in state.json.
func applyBank(ctx context.Context, d paths.Dirs, cfg config.Config, sec config.Secrets, force bool) (server.BankResult, error) {
	if err := bankBusy(d); err != nil {
		return server.BankResult{}, err
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err != nil {
		return server.BankResult{}, blocked(err)
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return server.BankResult{}, blocked(err)
	}
	// An install from before v0.1.4-m5 moves to the new bank names first
	// (update apply runs bank apply after its commit point, under its lock).
	var renamed, warning string
	if cfg.Server.Mode == "local" && cfg.Server.Bank == config.LegacyBank {
		switch r, err := runBankRename(ctx, d, cfg, sec, false, renameWaitApply); {
		case err != nil && r.Partial:
			// A bank moved and config.toml still names the old id: the
			// template would create an empty bank there.
			return server.BankResult{Bank: cfg.Server.Bank}, err
		case err != nil:
			warning = redacted(err, sec).Error() + " (backsight bank rename, or tonight's jobs maintain, tries again)"
		case r.Status == "done":
			cfg.Server.Bank, renamed = r.To, r.From+" → "+r.To
		}
	}
	res, err := server.ApplyBank(ctx, cl, cfg.Server.Bank, st.BankConfigHash, force)
	res.Renamed, res.RenameWarning = renamed, warning
	if err != nil {
		return res, blocked(err)
	}
	if _, rerr := os.Stat(d.BankTemplateFile()); res.Sent || st.BankConfigHash != res.Hash || rerr != nil {
		if err := recordBank(d, "bank apply", res.Hash); err != nil {
			return res, blocked(err)
		}
	}
	return res, nil
}

// bankBusy is refuseWhileBusy, except for update apply's own maintenance
// lock: update apply runs the new binary's bank apply after its commit
// point, while it still holds the lock.
func bankBusy(d paths.Dirs) error {
	if h := state.Inspect(d.MaintenanceLock()); h != nil && !h.Stale && heldByUpdate(h) {
		if state.RestoreIncomplete(d) {
			return &output.Error{Code: output.Blocked, Message: "a restore did not finish (" + d.RestoreMarker() + "): run backsight doctor for the way back"}
		}
		return nil
	}
	return refuseWhileBusy(d)
}

// recordBank records hash as the template the bank holds: in state.json,
// and the template itself when it is this binary's (doctor compares the
// bank with it once a later binary carries another). "" clears the hash:
// the next client install or update apply sends the template.
func recordBank(d paths.Dirs, command, hash string) error {
	if hash == server.TemplateHash() {
		if err := server.SaveApplied(d.BankTemplateFile()); err != nil {
			return err
		}
	}
	return state.Update(d, command, func(s *state.State) error { s.BankConfigHash = hash; return nil })
}

// bankStep brings the bank to this binary's template when the recorded
// one differs (client install; M5 review rules "Bank template follows the
// binary"): a non-force bank apply. Its failure is reported, never
// fatal: the next client install, update apply or doctor names it again.
// Without a config (Claude Code wired to nothing yet) there is no step.
func bankStep(ctx context.Context, plan bool, add func(name, status, detail string)) {
	d, home, err := machine()
	if err != nil {
		return
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		return
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		add("bank", "failed", err.Error())
		return
	}
	if st.BankConfigHash == server.TemplateHash() {
		add("bank", "unchanged", "bank "+cfg.Server.Bank+" holds this backsight's template")
		return
	}
	if h := state.Inspect(d.MaintenanceLock()); h != nil && !h.Stale && heldByUpdate(h) {
		add("bank", "deferred", "update apply sends this backsight's template to bank "+cfg.Server.Bank+" after its server step")
		return
	}
	if plan {
		add("bank", "planned", "would send this backsight's template to bank "+cfg.Server.Bank+" (backsight bank apply)")
		return
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		add("bank", "failed", err.Error())
		return
	}
	res, err := applyBank(ctx, d, cfg, sec, false)
	if err != nil {
		add("bank", "failed", redacted(err, sec).Error()+" (backsight bank apply retries)")
		return
	}
	add("bank", map[bool]string{true: "done", false: "unchanged"}[res.Sent], strings.TrimSpace(bankText(res)))
}

func bankText(r server.BankResult) string {
	switch {
	case r.Renamed != "":
		return "bank " + r.Renamed + " renamed, then " + strings.TrimSpace(bankText(server.BankResult{Bank: r.Bank, Created: r.Created, Sent: r.Sent})) + "\n"
	case r.RenameWarning != "":
		return strings.TrimSpace(bankText(server.BankResult{Bank: r.Bank, Created: r.Created, Sent: r.Sent})) + "; not renamed: " + r.RenameWarning + "\n"
	case r.Created:
		return fmt.Sprintf("bank %s created and configured\n", r.Bank)
	case r.Sent:
		return fmt.Sprintf("bank %s configured\n", r.Bank)
	}
	return fmt.Sprintf("bank %s unchanged (template already applied)\n", r.Bank)
}

func newBankApplyCmd(e *env) *cobra.Command {
	var plan, force bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create the bank if needed and apply backsight's extraction template",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			if plan {
				st, err := state.Load(d.StateFile())
				if err != nil {
					return blocked(err)
				}
				pending := force || st.BankConfigHash != server.TemplateHash()
				res := map[string]any{"bank": cfg.Server.Bank, "would_send": pending, "template_hash": server.TemplateHash()}
				return e.emit("bank apply", res, func() string {
					if pending {
						return fmt.Sprintf("bank %s: template would be sent\n", cfg.Server.Bank)
					}
					return fmt.Sprintf("bank %s: unchanged\n", cfg.Server.Bank)
				})
			}
			res, err := applyBank(cmd.Context(), d, cfg, sec, force)
			if err != nil {
				return err
			}
			return e.emit("bank apply", res, func() string { return bankText(res) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "say whether the template would be sent, without calling the server")
	cmd.Flags().BoolVar(&force, "force", false, "send the template even if it is already applied")
	return cmd
}

// restoreBank compares the bank's settings as the restored archive left
// them with this binary's template, and applies the template where they
// differ (M5 review rules, "Bank template follows the binary"): the
// archive's settings are older than the hash this client recorded. When
// it cannot, the recorded hash is cleared, so the next client install or
// update apply sends the template; its warning names bank apply. cl runs
// on restore's temporary key, before any client sends again.
func restoreBank(ctx context.Context, cl *hindsight.Client, d paths.Dirs, bank string, add func(name, status, detail string)) (warning string) {
	failed := func(err error) string {
		detail := err.Error()
		if cerr := recordBank(d, "restore", ""); cerr != nil {
			detail += "; the recorded template could not be cleared either (" + cerr.Error() + ")"
		}
		add("bank", "failed", detail)
		return "the restored bank's settings could not be brought to this backsight's template (" + detail + "): run backsight bank apply"
	}
	drift, err := server.BankDrift(ctx, cl, bank)
	if err != nil {
		return failed(err)
	}
	detail := "the archive's bank settings hold this backsight's template"
	if len(drift) > 0 {
		if _, err := server.ApplyBank(ctx, cl, bank, "", true); err != nil {
			return failed(err)
		}
		detail = "the archive's bank settings differed from this backsight's template (" + strings.Join(drift, ", ") + "): template applied"
	}
	if err := recordBank(d, "restore", server.TemplateHash()); err != nil {
		return failed(err)
	}
	add("bank", map[bool]string{true: "done", false: "unchanged"}[len(drift) > 0], detail)
	return ""
}
