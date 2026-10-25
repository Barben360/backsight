package cli

import (
	"context"
	"crypto"
	"encoding/json"
	"encoding/pem"
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
	"golang.org/x/crypto/ssh"
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
		// Under the maintenance lock (the caller holds it): two first runs
		// cannot both generate. A key set meanwhile is kept, never replaced.
		if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error {
			if s.BackupIdentity == "" {
				return s.Set("backup_identity", id.String())
			}
			return nil
		}); err != nil {
			return nil, nil, nil, err
		}
		fresh, err := config.LoadSecrets(d.SecretsFile())
		if err != nil {
			return nil, nil, nil, err
		}
		sec.BackupIdentity = fresh.BackupIdentity
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
	Skipped  string           `json:"skipped,omitempty"` // why no backup was made here
	Warnings []string         `json:"warnings,omitempty"`
	Path     string           `json:"path"`
	Bytes    int64            `json:"bytes"`
	Counts   map[string]int64 `json:"counts"`
	Pruned   []string         `json:"pruned,omitempty"`
	Duration int              `json:"ms"`
}

// acquireMaintenance takes the maintenance lock, breaking one left by a dead
// owner (a crash mid-backup), which is reported.
func acquireMaintenance(d paths.Dirs, command string) (*state.Lock, string, error) {
	l, err := state.TryAcquire(d.MaintenanceLock(), command)
	var held *state.HeldError
	if errors.As(err, &held) && held.Stale && state.BreakStale(held) == nil {
		l, err = state.TryAcquire(d.MaintenanceLock(), command)
		if err == nil {
			return l, "a stale maintenance lock (" + held.Owner.Command + ", process gone) was removed", nil
		}
	}
	if err != nil {
		return nil, "", &output.Error{Code: output.Blocked, Message: "maintenance in progress: " + err.Error()}
	}
	return l, "", nil
}

// safetyCopy makes createBackup take restore's safety archive: the caller
// holds the maintenance lock, which is neither taken nor released here, the
// clear dump is linked at dump to roll back to, and nothing is pruned (the
// archive being restored may be one of the old ones).
type safetyCopy struct {
	dump  string
	image string // the image the server runs, when it is not the pinned one (server upgrade)
}

// createBackup holds the maintenance lock for the database work only (hooks
// queue, jobs skip), then encrypts, writes the archive and prunes old ones.
func createBackup(ctx context.Context, safety *safetyCopy) (backupCreateResult, error) {
	start := time.Now()
	var res backupCreateResult
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return res, err
	}
	if cfg.Server.Mode != "local" {
		res.Skipped = "the server is remote: back it up on its host (backsight backup create there)"
		return res, nil
	}
	d, _, err := machine()
	if err != nil {
		return res, err
	}
	// The bank config is read before the lock: a slow server must not
	// stretch the window where hooks only queue.
	var bankConfig []byte
	if cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second); err == nil {
		bc, err := cl.BankConfig(ctx, cfg.Server.Bank)
		if err == nil {
			bankConfig, _ = json.MarshalIndent(bc, "", "  ")
		} else {
			res.Warnings = append(res.Warnings, "bank config not saved: "+err.Error())
		}
	}
	afterDump := func(dump string) error { return os.Link(dump, safety.dump) }
	if safety == nil {
		l, note, err := acquireMaintenance(d, "backup create")
		if err != nil {
			return res, err
		}
		if note != "" {
			res.Warnings = append(res.Warnings, note)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = l.Release()
			}
		}
		defer release()
		afterDump = func(string) error { release(); return nil }
	}
	recipients, lines, _, err := backupKeys(d, cfg, sec, true)
	if err != nil {
		return res, err
	}
	// Under the maintenance lock: a dump left in the container by a killed
	// backup or restore holds the memory in clear.
	db := runtimeFor(cfg).Database()
	_ = db.RemoveStaleDumps(ctx)
	extras := map[string][]byte{}
	if bankConfig != nil {
		extras["bank-config.json"] = bankConfig
	}
	for name, p := range map[string]string{"config.toml": d.ConfigFile(), "secrets.toml": d.SecretsFile(), "state.json": d.StateFile(),
		"compose.override.yaml": filepath.Join(cfg.Server.ComposeDir, "compose.override.yaml")} {
		if raw, err := os.ReadFile(p); err == nil {
			extras[name] = raw
		} else if !errors.Is(err, fs.ErrNotExist) {
			return res, blocked(err)
		}
	}
	dir := cfg.Backup.Dir
	if dir == "" {
		dir = filepath.Join(d.Data, "backups")
	}
	image := server.ImageFor(cfg)
	if safety != nil && safety.image != "" {
		image = safety.image
	}
	m := backup.Manifest{CreatedAt: now().UTC(), BinaryVersion: buildinfo.Version, HindsightImage: image, PostgresImage: server.PostgresImage,
		Embeddings: cfg.Embeddings.Preset + ": " + server.EmbeddingModels, Bank: cfg.Server.Bank, Recipients: lines}
	path, m, err := backup.Create(ctx, db, dir, filepath.Join(d.State, "staging"), m, extras, recipients, afterDump)
	if err != nil {
		return res, blocked(err)
	}
	res.Path, res.Counts = path, m.Counts
	if fi, err := os.Stat(path); err == nil {
		res.Bytes = fi.Size()
	}
	if safety == nil {
		if res.Pruned, err = backup.Prune(dir, cfg.Backup.KeepDaily, cfg.Backup.KeepWeekly); err != nil {
			return res, blocked(err)
		}
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
			res, err := createBackup(cmd.Context(), nil)
			if err != nil {
				return err
			}
			return e.emit("backup create", res, func() string {
				if res.Skipped != "" {
					return "no backup: " + res.Skipped + "\n"
				}
				s := fmt.Sprintf("backup %s (%d bytes, %v) in %d ms\n", res.Path, res.Bytes, res.Counts, res.Duration)
				for _, w := range res.Warnings {
					s += "warning: " + w + "\n"
				}
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
				if ids, err = identitiesFrom(e, identityFile, !e.json, "backsight backup verify "+args[0]+" --identity "+identityFile); err != nil {
					return err
				}
			} else if _, _, ids, err = backupKeys(d, cfg, sec, false); err != nil {
				return err
			}
			// Under the state directory (0700), not $TMPDIR; only the
			// manifest and the dump are written: the archive's secrets
			// and config are not needed to verify.
			work, done, err := backup.VerifyDir(filepath.Join(d.State, "staging"))
			if err != nil {
				return blocked(err)
			}
			defer done()
			m, err := backup.Open(args[0], ids, work, func(string) bool { return false })
			if err != nil {
				return blocked(err)
			}
			counts, err := runtimeFor(cfg).VerifyDump(cmd.Context(), filepath.Join(work, "db.dump"), backup.Tables)
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
			if err := recordVerify(d, cfg.Backup.Dir, args[0], m, res.OK); err != nil && failed == nil {
				failed = &output.Error{Code: output.Partial, Message: "verified, but the result was not recorded (doctor reads it): " + err.Error(), Resume: "backsight backup verify " + args[0]}
			}
			return e.report("backup verify", res, func() string {
				return fmt.Sprintf("%s: restored %v, manifest %v: %s\n", res.Path, res.Restored, res.Expected, map[bool]string{true: "ok", false: "MISMATCH"}[res.OK])
			}, failed)
		},
	}
	cmd.Flags().StringVar(&identityFile, "identity", "", "age identity file or SSH private key (default: the identity in secrets.toml)")
	return cmd
}

// errPassphrase: the SSH identity is passphrase-protected and there is no
// terminal to ask for it.
var errPassphrase = errors.New("the SSH key is protected by a passphrase")

// readIdentities reads an age identity file or an SSH private key. A
// passphrase-protected key asks prompt for its passphrase when it is first
// needed; with no prompt it is errPassphrase.
func readIdentities(path string, prompt func() ([]byte, error)) ([]agelib.Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(raw), "PRIVATE KEY") {
		id, err := agessh.ParseIdentity(raw)
		var missing *ssh.PassphraseMissingError
		switch {
		case errors.As(err, &missing) && prompt == nil:
			return nil, errPassphrase
		case errors.As(err, &missing) && missing.PublicKey != nil:
			eid, err := agessh.NewEncryptedSSHIdentity(missing.PublicKey, raw, prompt)
			if err != nil {
				return nil, err
			}
			return []agelib.Identity{eid}, nil
		case errors.As(err, &missing): // an old PEM key: no public key inside, decrypt now
			pass, err := prompt()
			if err != nil {
				return nil, err
			}
			k, err := ssh.ParseRawPrivateKeyWithPassphrase(raw, pass)
			if err != nil {
				return nil, err
			}
			pemKey, err := ssh.MarshalPrivateKey(k.(crypto.Signer), "")
			if err != nil {
				return nil, err
			}
			id, err := agessh.ParseIdentity(pem.EncodeToMemory(pemKey))
			if err != nil {
				return nil, err
			}
			return []agelib.Identity{id}, nil
		case err != nil:
			return nil, err
		}
		return []agelib.Identity{id}, nil
	}
	return agelib.ParseIdentities(strings.NewReader(string(raw)))
}

// askPassphrase reads an SSH key's passphrase from the terminal; tests
// replace it.
var askPassphrase = func(stderr io.Writer, path string) func() ([]byte, error) {
	return func() ([]byte, error) {
		fmt.Fprintf(stderr, "passphrase for %s (input hidden): ", path)
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(stderr)
		return raw, err
	}
}

// identitiesFrom reads --identity for command (the command line to rerun
// in a terminal): a passphrase is asked only when stdin is a terminal and
// the run is interactive, otherwise the user runs it (needs_user).
func identitiesFrom(e *env, path string, interactive bool, rerun string) ([]agelib.Identity, error) {
	var prompt func() ([]byte, error)
	if interactive && stdinIsTerminal() {
		prompt = askPassphrase(e.stderr, path)
	}
	ids, err := readIdentities(path, prompt)
	switch {
	case errors.Is(err, errPassphrase):
		return nil, &output.Error{Code: output.NeedsUser, Message: path + ": " + err.Error() + ": run the command in a terminal to type it",
			Actions: []output.Action{{Kind: "run_as_user", Cmd: rerun, Why: "the passphrase of " + path + " is typed in a terminal, never passed to an agent"}}}
	case err != nil:
		return nil, &output.Error{Code: output.Usage, Message: err.Error()}
	}
	return ids, nil
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
	importCmd := newBackupIdentityImportCmd(e)
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
			if cfg.Backup.Recipient != "" {
				return &output.Error{Code: output.NeedsUser, Message: "backups are encrypted to backup.recipient: the key that reads them is yours, not on this machine"}
			}
			l, _, err := acquireMaintenance(d, "backup identity show")
			if err != nil {
				return err
			}
			_, _, ids, err := backupKeys(d, cfg, sec, true)
			_ = l.Release()
			if err != nil || len(ids) == 0 {
				return err
			}
			sec, _ = config.LoadSecrets(d.SecretsFile())
			fmt.Fprintf(e.stdout, "%s\nKeep it somewhere safe and apart from this machine: without it the backups cannot be read.\n", sec.BackupIdentity)
			return nil
		},
	}, importCmd)
	return cmd
}

func newBackupIdentityImportCmd(e *env) *cobra.Command {
	var force bool
	c := &cobra.Command{
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
			err = config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error {
				if s.BackupIdentity != "" && s.BackupIdentity != key && !force {
					return &output.Error{Code: output.Usage, Message: "another backup identity is set: the backups made with it could no longer be read here. Give --force to replace it (keep a copy of the old one)"}
				}
				return s.Set("backup_identity", key)
			})
			var oe *output.Error
			if errors.As(err, &oe) {
				return oe
			}
			if err != nil {
				return blocked(err)
			}
			return e.emit("backup identity import", map[string]bool{"imported": true}, func() string { return "backup identity stored in secrets.toml\n" })
		},
	}
	c.Flags().BoolVar(&force, "force", false, "replace a different identity already set")
	return c
}

// recordVerify keeps the outcome of a verify in state.json, replacing the
// previous one: doctor offers to stop a reference server only after a
// passed verify of backsight's newest archive (0067 block 4).
func recordVerify(d paths.Dirs, dir, archive string, m backup.Manifest, ok bool) error {
	abs, err := filepath.Abs(archive)
	if err != nil {
		return err
	}
	newest := false
	if list, err := backup.List(dir); err == nil && len(list) > 0 {
		newest = sameFile(list[0].Path, abs)
	}
	rec := &state.BackupVerify{Archive: abs, CreatedAt: m.CreatedAt, Newest: newest, OK: ok, VerifiedAt: now().UTC()}
	return state.UpdateWithin(d, "backup verify", 10*time.Second, func(s *state.State) error {
		s.BackupVerify = rec
		return nil
	})
}

// sameFile compares two paths after resolving symlinks (a backup.dir
// behind a link names the same archives).
func sameFile(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}
