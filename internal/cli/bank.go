package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

func newBankCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "bank", Short: "Manage the memory bank", Args: cobra.NoArgs}
	cmd.AddCommand(newBankApplyCmd(e))
	return cmd
}

// applyBank runs bank apply and records the template hash in state.json.
func applyBank(cmd *cobra.Command, d paths.Dirs, cfg config.Config, sec config.Secrets, force bool) (server.BankResult, error) {
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err != nil {
		return server.BankResult{}, blocked(err)
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return server.BankResult{}, blocked(err)
	}
	res, err := server.ApplyBank(cmd.Context(), cl, cfg.Server.Bank, st.BankConfigHash, force)
	if err != nil {
		return res, blocked(err)
	}
	if res.Sent || st.BankConfigHash != res.Hash {
		if err := state.Update(d, "bank apply", func(s *state.State) error { s.BankConfigHash = res.Hash; return nil }); err != nil {
			return res, blocked(err)
		}
	}
	return res, nil
}

func bankText(r server.BankResult) string {
	switch {
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
			res, err := applyBank(cmd, d, cfg, sec, force)
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
