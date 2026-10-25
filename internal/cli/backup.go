package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	agelib "filippo.io/age"
	"filippo.io/age/agessh"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// stdoutIsTerminal is replaced by tests.
var stdoutIsTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

func newBackupCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "backup", Short: "Encrypted backups of the memory server (SPEC §11.8)", Args: cobra.NoArgs}
	cmd.AddCommand(newBackupCreateCmd(e), newBackupVerifyCmd(e), newBackupListCmd(e), newBackupIdentityCmd(e))
	return cmd
}

// backupKeys are the archive's recipients (public, in the manifest) and the
// identities this machine can decrypt with. Without a configured
// recipient, an identity is generated once into secrets.toml (decision
// 0023: shown with `backup identity show`, to keep elsewhere).
func backupKeys(d paths.Dirs, cfg config.Config, sec config.Secrets, create bool) ([]agelib.Recipient, []string, []agelib.Identity, error) {
	var lines []string
	if cfg.Backup.Recipient != "" {
		lines = strings.Split(cfg.Backup.Recipient, ",")
	}
	var ids []agelib.Identity
	if sec.BackupIdentity == "" && len(lines) == 0 {
		if !create {
			return nil, nil, nil, &output.Error{Code: output.NeedsUser, Message: "no backup identity on this machine: import it with backsight backup identity import"}
		}
		id, err := agelib.GenerateX25519Identity()
		if err != nil {
			return nil, nil, nil, err
		}
		if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("backup_identity", id.String()) }); err != nil {
			return nil, nil, nil, err
		}
		sec.BackupIdentity = id.String()
	}
	if sec.BackupIdentity != "" {
		id, err := agelib.ParseX25519Identity(sec.BackupIdentity)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("backup_identity in secrets.toml: %w", err)
		}
		ids = append(ids, id)
		if len(lines) == 0 {
			lines = []string{id.Recipient().String()}
		}
	}
	recipients, err := backup.Recipients(lines)
	if err != nil {
		return nil, nil, nil, &output.Error{Code: output.Usage, Message: err.Error()}
	}
	return recipients, lines, ids, nil
}

type backupCreateResult struct {
	Path     string           `json:"path"`
	Bytes    int64            `json:"bytes"`
	Counts   map[string]int64 `json:"counts"`
	Pruned   []string         `json:"pruned,omitempty"`
	Duration int              `json:"ms"`
}

// createBackup takes the maintenance lock for the dump (hooks queue, jobs
// skip), writes the archive and prunes old ones.
func createBackup(ctx context.Context) (backupCreateResult, error) {
	start := time.Now()
	var res backupCreateResult
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return res, err
	}
	d, _, err := machine()
	if err != nil {
		return res, err
	}
	recipients, lines, _, err := backupKeys(d, cfg, sec, true)
	if err != nil {
		return res, err
	}
	l, err := state.TryAcquire(d.MaintenanceLock(), "backup create")
	if err != nil {
		return res, &output.Error{Code: output.Blocked, Message: "maintenance in progress: " + err.Error()}
	}
	defer func() { _ = l.Release() }()
	extras := map[string][]byte{}
	for name, p := range map[string]string{"config.toml": d.ConfigFile(), "secrets.toml": d.SecretsFile(), "state.json": d.StateFile(),
		"compose.override.yaml": filepath.Join(cfg.Server.ComposeDir, "compose.override.yaml")} {
		if raw, err := os.ReadFile(p); err == nil {
			extras[name] = raw
		} else if !errors.Is(err, fs.ErrNotExist) {
			return res, blocked(err)
		}
	}
	if cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second); err == nil {
		if bc, err := cl.BankConfig(ctx, cfg.Server.Bank); err == nil {
			if raw, err := json.MarshalIndent(bc, "", "  "); err == nil {
				extras["bank-config.json"] = raw
			}
		}
	}
	dir := cfg.Backup.Dir
	if dir == "" {
		dir = filepath.Join(d.Data, "backups")
	}
	m := backup.Manifest{CreatedAt: now().UTC(), BinaryVersion: buildinfo.Version, HindsightImage: server.HindsightImage, PostgresImage: server.PostgresImage,
		Embeddings: cfg.Embeddings.Preset, Bank: cfg.Server.Bank, Recipients: lines}
	path, m, err := backup.Create(ctx, server.DB{Compose: composeFor(cfg)}, dir, m, extras, recipients)
	if err != nil {
		return res, blocked(err)
	}
	res.Path, res.Counts = path, m.Counts
	if fi, err := os.Stat(path); err == nil {
		res.Bytes = fi.Size()
	}
	if res.Pruned, err = backup.Prune(dir, cfg.Backup.KeepDaily, cfg.Backup.KeepWeekly); err != nil {
		return res, blocked(err)
	}
	res.Duration = int(time.Since(start).Milliseconds())
	return res, nil
}

func newBackupCreateCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "create",
		Short: "Dump the memory server into an encrypted archive (keeps 7 daily and 4 weekly)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := createBackup(cmd.Context())
			if err != nil {
				return err
			}
			return e.emit("backup create", res, func() string {
				s := fmt.Sprintf("backup %s (%d bytes, %v) in %d ms\n", res.Path, res.Bytes, res.Counts, res.Duration)
				for _, p := range res.Pruned {
					s += "pruned " + p + "\n"
				}
				return s
			})
		},
	}
}

type backupVerifyResult struct {
	Path     string           `json:"path"`
	Expected map[string]int64 `json:"expected"`
	Restored map[string]int64 `json:"restored"`
	OK       bool             `json:"ok"`
}

func newBackupVerifyCmd(e *env) *cobra.Command {
	var identityFile string
	cmd := &cobra.Command{
		Use:   "verify <file>",
		Short: "Decrypt a backup, restore it into a throwaway database and compare its row counts",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			var ids []agelib.Identity
			if identityFile != "" {
				if ids, err = readIdentities(identityFile); err != nil {
					return &output.Error{Code: output.Usage, Message: err.Error()}
				}
			} else if _, _, ids, err = backupKeys(d, cfg, sec, false); err != nil {
				return err
			}
			work, err := os.MkdirTemp("", "backsight-verify-")
			if err != nil {
				return blocked(err)
			}
			defer func() { _ = os.RemoveAll(work) }()
			m, err := backup.Open(args[0], ids, work)
			if err != nil {
				return blocked(err)
			}
			counts, err := server.RestoreCounts(cmd.Context(), composeRunner, filepath.Join(work, "db.dump"), backup.Tables)
			if err != nil {
				return blocked(err)
			}
			res := backupVerifyResult{Path: args[0], Expected: m.Counts, Restored: counts, OK: true}
			for _, t := range backup.Tables {
				if counts[t] != m.Counts[t] {
					res.OK = false
				}
			}
			var failed error
			if !res.OK {
				failed = &output.Error{Code: output.Blocked, Message: fmt.Sprintf("restored counts %v differ from the manifest's %v", counts, m.Counts)}
			}
			return e.report("backup verify", res, func() string {
				return fmt.Sprintf("%s: restored %v, manifest %v: %s\n", res.Path, res.Restored, res.Expected, map[bool]string{true: "ok", false: "MISMATCH"}[res.OK])
			}, failed)
		},
	}
	cmd.Flags().StringVar(&identityFile, "identity", "", "age identity file or SSH private key (default: the identity in secrets.toml)")
	return cmd
}

func readIdentities(path string) ([]agelib.Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(raw), "PRIVATE KEY") {
		id, err := agessh.ParseIdentity(raw)
		if err != nil {
			return nil, err
		}
		return []agelib.Identity{id}, nil
	}
	return agelib.ParseIdentities(strings.NewReader(string(raw)))
}

func newBackupListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the backups, newest first",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			cfg, _, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			dir := cfg.Backup.Dir
			if dir == "" {
				dir = filepath.Join(d.Data, "backups")
			}
			list, err := backup.List(dir)
			if err != nil {
				return blocked(err)
			}
			if list == nil {
				list = []backup.Entry{}
			}
			return e.emit("backup list", list, func() string {
				if len(list) == 0 {
					return "no backup in " + dir + "\n"
				}
				var b strings.Builder
				for _, en := range list {
					fmt.Fprintf(&b, "%s  %s  %d bytes  %v\n", en.Manifest.CreatedAt.Format("2006-01-02 15:04"), filepath.Base(en.Path), en.Bytes, en.Manifest.Counts)
				}
				return b.String()
			})
		},
	}
}

func newBackupIdentityCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "identity", Short: "The age identity that decrypts this install's backups", Args: cobra.NoArgs}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the backup identity, to keep somewhere else (terminal only)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			// A secret: printed to a person at a terminal, never to an agent
			// reading the output (SPEC §11.2 secrets rule).
			if e.json || !stdoutIsTerminal() {
				return &output.Error{Code: output.NeedsUser, Message: "the backup identity is a secret: run backsight backup identity show in a terminal yourself",
					Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight backup identity show", Why: "save the key that decrypts the backups"}}}
			}
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			if _, _, ids, err := backupKeys(d, cfg, sec, true); err != nil || len(ids) == 0 {
				if err == nil {
					err = &output.Error{Code: output.NeedsUser, Message: "backups go to backup.recipient: its private key is yours, not on this machine"}
				}
				return err
			}
			sec, _ = config.LoadSecrets(d.SecretsFile())
			fmt.Fprintf(e.stdout, "%s\nKeep it somewhere safe and apart from this machine: without it the backups cannot be read.\n", sec.BackupIdentity)
			return nil
		},
	}, &cobra.Command{
		Use:   "import [file|-]",
		Short: "Store a backup identity (age) in secrets.toml, from a file or stdin",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, _, err := machine()
			if err != nil {
				return err
			}
			var raw []byte
			if len(args) == 0 || args[0] == "-" {
				raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
			} else {
				raw, err = os.ReadFile(args[0])
			}
			if err != nil {
				return blocked(err)
			}
			var key string
			for _, l := range strings.Split(string(raw), "\n") {
				if l = strings.TrimSpace(l); strings.HasPrefix(l, "AGE-SECRET-KEY-") {
					key = l
				}
			}
			if _, err := agelib.ParseX25519Identity(key); err != nil {
				return &output.Error{Code: output.Usage, Message: "no age identity (AGE-SECRET-KEY-…) in the input"}
			}
			if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("backup_identity", key) }); err != nil {
				return blocked(err)
			}
			return e.emit("backup identity import", map[string]bool{"imported": true}, func() string { return "backup identity stored in secrets.toml\n" })
		},
	})
	return cmd
}
