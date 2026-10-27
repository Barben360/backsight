package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/guide"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// renameOps is what bank rename asks of the server; tests replace it.
type renameOps interface {
	Banks(ctx context.Context) ([]string, error)
	Pending(ctx context.Context, banks []string) (int, error)
	Rename(ctx context.Context, from, to string, dryRun bool) error
	Alias(ctx context.Context, bank, alias string) error
}

type dockerRename struct {
	compose server.Compose
	cl      *hindsight.Client
}

func (r dockerRename) Banks(ctx context.Context) ([]string, error) {
	return server.DB{Compose: r.compose}.BankIDs(ctx)
}

func (r dockerRename) Pending(ctx context.Context, banks []string) (int, error) {
	return server.DB{Compose: r.compose}.PendingOperations(ctx, banks)
}

func (r dockerRename) Rename(ctx context.Context, from, to string, dryRun bool) error {
	_, err := r.compose.RenameBank(ctx, from, to, dryRun)
	return err
}

func (r dockerRename) Alias(ctx context.Context, bank, alias string) error {
	return r.cl.CreateBankAlias(ctx, bank, alias)
}

var renameOpsFor = func(cfg config.Config, sec config.Secrets) (renameOps, error) {
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err != nil {
		return nil, err
	}
	return dockerRename{compose: composeFor(cfg), cl: cl}, nil
}

// How long a rename waits for the server's pending operations
// (consolidation after a send), which rename-bank refuses to run with: by
// hand, inside bank apply (update apply holds the hooks meanwhile) and in
// the daily job, which tries again the next day.
var (
	renameWaitHand, renameWaitApply, renameWaitDaily = 10 * time.Minute, 2 * time.Minute, 5 * time.Minute
	renamePoll                                       = 5 * time.Second
)

type bankRenameResult struct {
	From     string        `json:"from"`
	To       string        `json:"to"`
	Status   string        `json:"status"` // done | unchanged | skipped | planned
	Steps    []installStep `json:"steps"`
	Warnings []string      `json:"warnings,omitempty"`
}

type bankPair struct{ from, to string }

// bankPairs are the memory bank and the two banks named after it.
func bankPairs(from, to string) []bankPair {
	return []bankPair{{from, to}, {guide.Bank(from), guide.Bank(to)}, {retain.ControlBank(from), retain.ControlBank(to)}}
}

func newBankRenameCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:   "rename",
		Short: "Rename the legacy banks (claude-memory, -docs, -control) to backsight's names, keeping every memory",
		Long: "rename moves the banks of an install from before v0.1.4-m5 to the new names with Hindsight's own rename " +
			"(hindsight-admin rename-bank: one transaction per bank, no LLM call), leaves each old id as an alias of its bank so that a " +
			"client still using it keeps working, moves this machine's records to the new name and sets server.bank in config.toml. " +
			"It waits for the server's pending operations first, and changes nothing when one bank cannot move. update apply runs it.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, _, err := machine()
			if err != nil {
				return err
			}
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			res, err := runBankRename(cmd.Context(), d, cfg, sec, plan, renameWaitHand)
			if err != nil {
				return redacted(err, sec)
			}
			return e.emit("bank rename", res, func() string { return bankRenameText(res) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would be renamed, without changing anything")
	return cmd
}

func bankRenameText(r bankRenameResult) string {
	var b strings.Builder
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "%-8s %-9s %s\n", s.Name, s.Status, s.Detail)
	}
	for _, w := range r.Warnings {
		b.WriteString("warning: " + w + "\n")
	}
	return b.String()
}

func runBankRename(ctx context.Context, d paths.Dirs, cfg config.Config, sec config.Secrets, plan bool, wait time.Duration) (bankRenameResult, error) {
	from, to := cfg.Server.Bank, config.DefaultBank
	res := bankRenameResult{From: from, To: to}
	add := func(name, status, detail string) {
		res.Steps = append(res.Steps, installStep{Name: name, Status: status, Detail: detail})
	}
	switch {
	case cfg.Server.Mode != "local":
		res.Status = "skipped"
		add("banks", "skipped", "the server is remote: run backsight bank rename on its host; this client keeps reaching bank "+from+" through its alias")
		return res, nil
	case from != config.LegacyBank:
		res.Status = "unchanged"
		add("banks", "unchanged", "bank "+from+" keeps its name")
		return res, nil
	}
	if !plan {
		if h := state.Inspect(d.MaintenanceLock()); h == nil || h.Stale || !heldByUpdate(h) {
			l, note, err := acquireMaintenance(d, "bank rename")
			if err != nil {
				return res, err
			}
			defer func() { _ = l.Release() }()
			if note != "" {
				res.Warnings = append(res.Warnings, note)
			}
		}
	}
	ops, err := renameOpsFor(cfg, sec)
	if err != nil {
		return res, blocked(err)
	}
	ids, err := ops.Banks(ctx)
	if err != nil {
		return res, blocked(fmt.Errorf("the server's banks: %w", err))
	}
	var moves []bankPair
	for _, p := range bankPairs(from, to) {
		old, now := slices.Contains(ids, p.from), slices.Contains(ids, p.to)
		switch {
		case old && now:
			return res, &output.Error{Code: output.Blocked, Message: fmt.Sprintf("banks %s and %s both exist: nothing was renamed. "+
				"Keep server.bank = %q, or remove the bank you do not use, then run backsight bank rename again", p.from, p.to, from)}
		case old:
			moves = append(moves, p)
		}
	}
	if plan {
		res.Status = "planned"
		for _, p := range moves {
			add("rename", "planned", p.from+" → "+p.to+" (hindsight-admin rename-bank), then "+p.from+" as its alias")
		}
		add("config", "planned", "server.bank = \""+to+"\" in "+d.ConfigFile()+", and this machine's records moved to it")
		return res, nil
	}

	// No operation may be running on a bank that moves: rename-bank
	// refuses then, and its rows would follow the old id.
	if len(moves) > 0 {
		olds := make([]string, len(moves))
		for i, p := range moves {
			olds[i] = p.from
		}
		deadline := time.Now().Add(wait)
		for {
			n, err := ops.Pending(ctx, olds)
			if err != nil {
				return res, blocked(fmt.Errorf("the server's pending operations: %w", err))
			}
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				return res, &output.Error{Code: output.Blocked, Message: fmt.Sprintf("%d operation(s) still running on bank %s after %s: nothing was renamed. "+
					"Run backsight bank rename again later", n, from, wait)}
			}
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(renamePoll):
			}
		}
		for _, p := range moves {
			if err := ops.Rename(ctx, p.from, p.to, true); err != nil {
				return res, blocked(fmt.Errorf("rename %s → %s refused in its dry run, nothing was renamed: %w", p.from, p.to, err))
			}
		}
	}
	var done []bankPair
	undo := func(cause error) error {
		for i := len(done) - 1; i >= 0; i-- {
			if err := ops.Rename(context.WithoutCancel(ctx), done[i].to, done[i].from, false); err != nil {
				return blocked(fmt.Errorf("%w; renaming %s back to %s failed too (%w): run backsight bank rename again", cause, done[i].to, done[i].from, err))
			}
		}
		return blocked(fmt.Errorf("%w (the banks renamed before it were renamed back)", cause))
	}
	for _, p := range moves {
		if err := ops.Rename(ctx, p.from, p.to, false); err != nil {
			return res, undo(fmt.Errorf("rename %s → %s: %w", p.from, p.to, err))
		}
		done = append(done, p)
		add("rename", "done", p.from+" → "+p.to)
	}
	// The old ids keep working: a Claude Code session started before the
	// update, or another machine's client, still calls them.
	for _, p := range moves {
		if err := ops.Alias(ctx, p.to, p.from); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("bank %s: alias %s not created (%v); a client still using the old id gets a new, empty bank", p.to, p.from, err))
			continue
		}
		add("alias", "done", p.from+" reaches "+p.to)
	}
	if err := moveRecords(d, from, to); err != nil {
		return res, blocked(fmt.Errorf("the banks are renamed, this machine's records are not (%w): run backsight bank rename again", err))
	}
	add("records", "done", "state.json and the session cursors name bank "+to)
	raw, err := os.ReadFile(d.ConfigFile())
	if err == nil {
		raw, err = config.SetServerBank(raw, to)
	}
	if err == nil {
		err = fsutil.WriteAtomic(d.ConfigFile(), raw, 0o600)
	}
	if err != nil {
		return res, blocked(fmt.Errorf("the banks are renamed, config.toml is not (%w): set server.bank = %q in %s", err, to, d.ConfigFile()))
	}
	add("config", "done", "server.bank = \""+to+"\" in "+d.ConfigFile())
	res.Status = "done"
	return res, nil
}

// moveRecords points this machine's records of bank from at bank to: the
// tombstones, restore epoch and purge records of state.json, and each
// session's cursor, which would otherwise read as another bank's and send
// its whole session again.
func moveRecords(d paths.Dirs, from, to string) error {
	if err := state.Update(d, "bank rename", func(s *state.State) error {
		for i := range s.Tombstones {
			if s.Tombstones[i].Bank == from {
				s.Tombstones[i].Bank = to
			}
		}
		if s.RestoreEpoch != nil && s.RestoreEpoch.Bank == from {
			s.RestoreEpoch.Bank = to
		}
		if s.PurgeHold != nil && s.PurgeHold.Bank == from {
			s.PurgeHold.Bank = to
		}
		if s.PurgeGuard != nil && s.PurgeGuard.Bank == from {
			s.PurgeGuard.Bank = to
		}
		return nil
	}); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(d.CursorsDir(), "*.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		sid := strings.TrimSuffix(filepath.Base(f), ".json")
		cur, ok, err := retain.LoadCursor(d.CursorsDir(), sid)
		if err != nil {
			return err
		}
		if !ok || cur.Bank != from {
			continue
		}
		cur.Bank = to
		if err := retain.SaveCursor(d.CursorsDir(), sid, cur); err != nil {
			return err
		}
	}
	return nil
}
