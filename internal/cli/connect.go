package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	agelib "filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/token"
)

// What this PC is to the server a token names (decision 0073).
const (
	joinNew   = "new"          // no config.toml yet
	joinSame  = "same-server"  // a client of that server already
	joinOther = "other-server" // a client of another server
	joinOwn   = "own-server"   // a PC with a server of its own
	joinUndo  = "undo"         // connect --undo: back to this PC's own server
)

// tokenArgument is the refusal of a token given on the command line: it
// never quotes the value, and the key it holds is in the shell's history.
const tokenArgument = "the token holds the API key: pass it on stdin (--token -) or in a file (--token-file F); " +
	"it is now in your shell history: run backsight server token --rotate on the host"

type connectFlags struct {
	token, tokenFile string
	plan, undo       bool
	answers          []string
	answersFile      string
	nonInteractive   bool
	configDir        string
	// Not flags: server install --ssh brought the token back over ssh
	// (preread: not read again) and resumes with its own command line.
	preread *token.Token
	resume  string
}

// ownServer is what joining another server does to this PC's own.
type ownServer struct {
	Deployed    bool   `json:"deployed"`
	Sessions    int    `json:"sessions"`               // past sessions a backfill sends to the new server
	SavedNotes  *int   `json:"saved_notes,omitempty"`  // the note documents of its bank; nil: not counted
	OrphanNotes *int   `json:"orphan_notes,omitempty"` // of those, the ones no transcript here sends again
	Backup      string `json:"backup,omitempty"`       // the archive verified before the stop
}

// connectResult is what connect did or would do. It never holds the key
// nor the token.
type connectResult struct {
	URL              string           `json:"url"`
	Bank             string           `json:"bank"`
	ServerID         string           `json:"server_id"`
	HindsightVersion string           `json:"hindsight_version,omitempty"`
	Situation        string           `json:"situation"`
	Skew             doctor.Status    `json:"version_skew,omitempty"`
	Plan             bool             `json:"plan"`
	Declined         bool             `json:"declined,omitempty"`
	Warnings         []string         `json:"warnings,omitempty"`
	Notes            []string         `json:"notes,omitempty"` // information, nothing to do
	Steps            []step           `json:"steps"`
	Questions        []setup.Question `json:"questions,omitempty"`
	OwnServer        *ownServer       `json:"own_server,omitempty"`
	Setup            *setupResult     `json:"setup,omitempty"`
	RestartRequired  bool             `json:"restart_required"`
	NextSteps        []string         `json:"next_steps,omitempty"`
}

func newConnectCmd(e *env) *cobra.Command {
	var f connectFlags
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Join the memory server of another PC with the connection token its host gave out",
		Long: "The token holds the API key: it is read from stdin (--token -, a hidden prompt at a terminal) or a file (--token-file F), never from the command line. " +
			"connect checks the server, shows what it will change, then writes this PC's config and key and runs setup's client steps.",
		// A token given as an argument is refused without being quoted.
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			if slices.ContainsFunc(args, looksLikeToken) {
				return &output.Error{Code: output.Usage, Message: "connect takes no argument: " + tokenArgument}
			}
			return &output.Error{Code: output.Usage, Message: "connect takes no argument: pass the token on stdin (--token -) or in a file (--token-file F)"}
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.undo {
				if f.token != "" || f.tokenFile != "" || len(f.answers) > 0 || f.answersFile != "" {
					return &output.Error{Code: output.Usage, Message: "connect --undo takes no token nor answers: it puts back what connect kept on this PC"}
				}
				res, err := undoConnect(cmd.Context(), e, f.plan)
				if res.URL == "" {
					return err
				}
				return e.report("connect", res, func() string { return connectText(res) }, err)
			}
			res, err := runConnect(cmd.Context(), e, f)
			if res.URL == "" { // refused before the token was read: nothing to show
				return err
			}
			return e.report("connect", res, func() string { return connectText(res) }, err)
		},
	}
	cmd.Flags().StringVar(&f.token, "token", "", "- reads the connection token from stdin (a hidden prompt at a terminal); never the token itself")
	cmd.Flags().StringVar(&f.tokenFile, "token-file", "", "read the connection token from this file (backsight server token --out writes it on the host)")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "check the server and show what connect would change, changing nothing")
	cmd.Flags().BoolVar(&f.undo, "undo", false, "point this PC back at its own server, as it was before connect, and start it")
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "consent=yes to go ahead without a terminal; setup's answers (id=value, repeatable)")
	cmd.Flags().StringVar(&f.answersFile, "answers-file", "", "answers as a JSON object or id=value lines (no secrets)")
	cmd.Flags().BoolVar(&f.nonInteractive, "non-interactive", false, "never prompt: exit 10 when the consent or another answer is missing")
	cmd.Flags().StringVar(&f.configDir, "config-dir", "", "Claude Code's directory (default: $CLAUDE_CONFIG_DIR, else ~/.claude)")
	return cmd
}

// looksLikeToken: a value that is, or starts like, a connection token.
func looksLikeToken(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), token.Prefix) }

// readToken reads and decodes the connection token from stdin (flag "-")
// or from file. Its errors never quote the token.
func readToken(e *env, flag, file string) (token.Token, error) {
	switch {
	case flag != "" && flag != "-", looksLikeToken(file):
		return token.Token{}, &output.Error{Code: output.Usage, Message: tokenArgument}
	case flag != "" && file != "":
		return token.Token{}, &output.Error{Code: output.Usage, Message: "--token - and --token-file read the same token: give one"}
	case flag == "" && file == "":
		return token.Token{}, &output.Error{Code: output.Usage, Message: "connect needs the connection token: --token - reads it from stdin, --token-file F from a file " +
			"(on the server's host, backsight server token --out F writes it)"}
	}
	raw, err := readSecret(e, "connection token", "", file)
	var oe *output.Error
	if errors.As(err, &oe) && oe.Code == output.NeedsUser { // readSecret's advice is secret set's
		return token.Token{}, &output.Error{Code: output.NeedsUser, Message: "no terminal to prompt in and nothing on stdin: put the token in a file only you can read and run backsight connect --token-file F",
			Resume: "backsight connect --token-file F"}
	}
	if err != nil {
		return token.Token{}, err
	}
	tok, err := token.Decode(raw)
	if err != nil {
		return token.Token{}, &output.Error{Code: output.Usage, Message: err.Error() + ": ask the server's host for a new one (backsight server token --out F)"}
	}
	return tok, nil
}

// publicAddress refuses a token whose server is a public IP literal: the
// key would cross the internet. A host name (MagicDNS, a LAN name) passes;
// doctor warns when the key travels in clear.
func publicAddress(tok token.Token) error {
	u, err := url.Parse(tok.URL)
	if err != nil {
		return &output.Error{Code: output.Usage, Message: "the token's server address does not parse"}
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || config.IsTailnet(ip) || ip.IsLinkLocalUnicast() {
		return nil
	}
	return &output.Error{Code: output.Usage, Message: "the token's server " + u.Host + " is a public address: the API key must not cross the internet. " +
		"On the server's host, open it on a private address instead (backsight server expose --private <ip>, a LAN or tailnet address) and hand out a new token"}
}

// openSessionStep is what a session open across connect (or connect
// --undo) does until Claude Code restarts: hooks read the config on each
// run, the memory server only at its start.
const openSessionStep = "restart Claude Code (or run /reload-plugins): until then an open session's memory tools keep the old server (they write nothing to it); " +
	"its prompt recall already uses the new one, and when the session ends it is sent whole to the new one"

// probe is what connect learnt from the server, read-only.
type probe struct {
	Version string
	Skew    doctor.Status
	Epoch   int
	// RestoredAt: the epoch's identity (state.RestoreEpoch.RestoredAt).
	RestoredAt time.Time
}

// probeToken checks the server tok names, reading only: it answers, its
// bank listing accepts the key and holds tok's bank, its id is tok's. It
// returns its Hindsight version and the bank's restore epoch. Errors mask
// the key.
func probeToken(ctx context.Context, tok token.Token) (probe, error) {
	sec := config.Secrets{APIKey: tok.APIKey}
	cl, err := hindsight.New(tok.URL, tok.APIKey, 30*time.Second)
	if err != nil {
		return probe{}, redacted(err, sec)
	}
	rejected := func(err error) bool {
		var apiErr *hindsight.APIError
		return errors.As(err, &apiErr) && apiErr.Kind == hindsight.Unauthorized
	}
	fail := func(what string, err error) error {
		if rejected(err) {
			return redacted(errors.New("the server rejected this token's key (rotated?): ask the host for a new one (backsight server token)"), sec)
		}
		return redacted(fmt.Errorf("%s at %s: %s", what, tok.URL, err.Error()), sec)
	}
	if _, err := cl.Health(ctx); err != nil && !rejected(err) {
		return probe{}, redacted(fmt.Errorf("the server at %s does not answer (%s): check that it runs on its host (backsight server status there) and that this PC reaches that address", tok.URL, err.Error()), sec)
	}
	// /health may answer without a key: the bank listing proves this one.
	if _, found, err := cl.FindBank(ctx, tok.Bank); err != nil {
		return probe{}, fail("listing the banks", err)
	} else if !found {
		return probe{}, redacted(fmt.Errorf("the server at %s has no bank %s: ask its host for a new token (backsight server token)", tok.URL, tok.Bank), sec)
	}
	id, err := retain.ReadServerID(ctx, cl, tok.Bank)
	if err != nil {
		return probe{}, fail("reading the server id", err)
	}
	if id != tok.ServerID {
		return probe{}, redacted(fmt.Errorf("the server at %s is not the one this token was made for: get a fresh token there", tok.URL), sec)
	}
	ep, err := retain.ReadEpoch(ctx, cl, tok.Bank)
	if err != nil {
		return probe{}, fail("reading the bank's restore epoch", err)
	}
	// A version skew never refuses: an unreadable version is the one the
	// token was made with.
	version := tok.HindsightVersion
	if v, err := cl.Version(ctx); err == nil && v.APIVersion != "" {
		version = v.APIVersion
	}
	p := probe{Version: version, Epoch: ep.Epoch, RestoredAt: ep.RestoredAt}
	if version != "" {
		p.Skew = doctor.VersionSkew(version, server.PinnedVersion())
	}
	return p, nil
}

// situationOf says what this PC is to tok's server, with its config (nil
// when there is none). A server-only host refuses: other PCs connect to it.
func situationOf(d paths.Dirs, home string, tok token.Token) (string, *config.Config, error) {
	cfg, err := config.Load(d.ConfigFile(), d, home)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return joinNew, nil, nil
	case err != nil:
		return "", nil, &output.Error{Code: output.Blocked, Message: err.Error()}
	case cfg.ServerOnly():
		return "", &cfg, &output.Error{Code: output.Usage, Message: "this machine is a server-only host (server.role = \"server\"): the other PCs connect to it, it does not connect to another server"}
	case cfg.Server.Mode == "local":
		return joinOwn, &cfg, nil
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return "", &cfg, blocked(err)
	}
	if st.RemoteServer != nil && st.RemoteServer.ID == tok.ServerID {
		return joinSame, &cfg, nil
	}
	return joinOther, &cfg, nil
}

// connectConfig is config.toml as connect writes it: raw (nil: none yet)
// pointed at tok's server, with the host's LLM for cost estimates and
// never a key. The result is checked to load as a client.
func connectConfig(d paths.Dirs, home string, raw []byte, tok token.Token) ([]byte, config.Config, error) {
	if raw == nil {
		raw = []byte("# Written by backsight connect.\nschema_version = 1\n")
	}
	out, err := config.SetRemote(raw, tok.URL, tok.Bank)
	if err == nil {
		l := tok.LLM
		out, err = config.SetLLMEstimate(out, l.Provider, l.Model, l.Currency, l.PriceInput, l.PriceCached, l.PriceOutput)
	}
	if err != nil {
		return nil, config.Config{}, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	cfg, err := config.Parse(out, d.ConfigFile(), d, home)
	if err != nil {
		return nil, cfg, &output.Error{Code: output.Blocked, Message: "config.toml once connected would not load: " + err.Error()}
	}
	return out, cfg, nil
}

// connectMeta is what resetRetain records next to the files it moved: the
// state they went with, so the move can be read or undone.
type connectMeta struct {
	MovedAt      time.Time           `json:"moved_at"`
	URL          string              `json:"url,omitempty"`
	Mode         string              `json:"mode,omitempty"`
	RetainSince  *time.Time          `json:"retain_since,omitempty"`
	Backfill     *state.Backfill     `json:"backfill,omitempty"`
	RestoreEpoch *state.RestoreEpoch `json:"restore_epoch,omitempty"`
	RemoteServer *state.RemoteServer `json:"remote_server,omitempty"`
	// No sends generation: it is never put back (a rollback raises it
	// again), so meta.json has nothing to record of it.
}

// resetRetain starts this PC's sends over for another server: the session
// cursors, the sent-index and the split sessions move into keep (a new
// directory), with
// meta.json recording the state they went with and the server they were
// for (config.toml as it is now). Live retain then starts at at, the
// backfill record is dropped and the restore epoch is epoch, the new
// server's: resync sends each session's records the server lacks, never
// what it holds. Tombstones stay: a purged session never goes out again.
func resetRetain(d paths.Dirs, keep string, epoch state.RestoreEpoch, at time.Time) error {
	if err := fsutil.MkdirPrivate(filepath.Dir(keep)); err != nil {
		return err
	}
	if err := os.Mkdir(keep, 0o700); err != nil {
		return err
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return err
	}
	meta := connectMeta{MovedAt: at.UTC(), RetainSince: st.RetainSince, Backfill: st.Backfill, RestoreEpoch: st.RestoreEpoch, RemoteServer: st.RemoteServer}
	if home, herr := os.UserHomeDir(); herr == nil {
		if cfg, cerr := config.Load(d.ConfigFile(), d, home); cerr == nil {
			meta.URL, meta.Mode = cfg.Server.URL, cfg.Server.Mode
		}
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(filepath.Join(keep, "meta.json"), append(raw, '\n'), 0o600); err != nil {
		return err
	}
	for _, p := range retainFiles(d) {
		if err := os.Rename(p, filepath.Join(keep, filepath.Base(p))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// A new generation: a flusher built before sends nothing after.
	since := at.UTC().Truncate(time.Second)
	return state.UpdateWithin(d, "connect", 5*time.Second, func(s *state.State) error {
		s.RetainSince, s.Backfill, s.RestoreEpoch = &since, nil, &epoch
		s.SendsGeneration++
		return nil
	})
}

// retainFiles are what this PC's sends to one server left: resetRetain
// moves them aside.
func retainFiles(d paths.Dirs) []string {
	return []string{d.CursorsDir(), d.SentIndexDir(), retain.SplitSessionsFile(d.State)}
}

// undoRetainReset puts back what resetRetain moved into keep and the state
// fields meta.json recorded, then removes keep unless told to keep it
// (it holds a file still to put back). No meta.json: nothing was moved
// yet. rollback: the reset is undone within its own run, so the sends
// generation is raised once more, in the same update, never put back: a
// process that read the generation before the run and the config once
// the run wrote the other server's would pass the gate again, and one
// that read the raised generation could pass it after the next connect's
// reset. A flusher built before the run stops once; the next one sends.
func undoRetainReset(d paths.Dirs, keep string, remove, rollback bool) error {
	if err := connectFault("undo"); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(keep, "meta.json"))
	if errors.Is(err, fs.ErrNotExist) {
		_ = os.Remove(keep)
		return nil
	}
	if err != nil {
		return err
	}
	var meta connectMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return err
	}
	for _, p := range retainFiles(d) {
		moved := filepath.Join(keep, filepath.Base(p))
		if _, err := os.Lstat(moved); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.Rename(moved, p); err != nil {
			return err
		}
	}
	if err := state.UpdateWithin(d, "connect", 5*time.Second, func(s *state.State) error {
		s.RetainSince, s.Backfill, s.RestoreEpoch = meta.RetainSince, meta.Backfill, meta.RestoreEpoch
		if rollback {
			s.SendsGeneration++
		}
		return nil
	}); err != nil {
		return err
	}
	if remove {
		_ = os.RemoveAll(keep)
		_ = os.Remove(d.ConnectDir()) // when empty
	}
	return nil
}

// keyUnread stands for the hash of a secrets.toml connect wrote but could
// not read back: no file matches it, so the rollback leaves the file as
// it is and says so.
const keyUnread = "unread"

// putKeyBack puts secrets.toml back as it was before connect wrote the key
// (was, its hash wasHash; removed when it did not exist), unless it is no
// longer what connect wrote (written): another writer's change is kept
// (fsutil.ErrConflict).
func putKeyBack(path string, was []byte, wasHash, written string) error {
	if wasHash != fsutil.Absent {
		return fsutil.ReplaceIfUnchanged(path, was, 0o600, written)
	}
	got, err := fsutil.HashFile(path)
	if err != nil {
		return err
	}
	if got != written {
		return fmt.Errorf("%s: %w", path, fsutil.ErrConflict)
	}
	return os.Remove(path)
}

// keepOwnConfig copies config.toml (raw, as it was read under the lock)
// and secrets.toml into keep (0600): connect --undo puts them back.
func keepOwnConfig(d paths.Dirs, keep string, raw []byte) error {
	if err := fsutil.WriteAtomic(filepath.Join(keep, "config.toml"), raw, 0o600); err != nil {
		return err
	}
	sec, err := os.ReadFile(d.SecretsFile())
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(keep, "secrets.toml"), sec, 0o600)
}

// ownPreImage is the newest directory connect left when it took this PC
// off its own server (meta.json mode local), "" when there is none.
func ownPreImage(d paths.Dirs) (string, connectMeta, error) {
	ents, err := os.ReadDir(d.ConnectDir())
	if errors.Is(err, fs.ErrNotExist) {
		return "", connectMeta{}, nil
	}
	if err != nil {
		return "", connectMeta{}, blocked(err)
	}
	var newest string
	var meta connectMeta
	for _, ent := range ents { // by name: a later run of the same second sorts after
		raw, err := os.ReadFile(filepath.Join(d.ConnectDir(), ent.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m connectMeta
		if json.Unmarshal(raw, &m) != nil || m.Mode != "local" || m.MovedAt.Before(meta.MovedAt) {
			continue
		}
		newest, meta = filepath.Join(d.ConnectDir(), ent.Name()), m
	}
	return newest, meta, nil
}

// undoConnect points this PC back at its own server, as the newest
// connect from it left it: under the maintenance lock, the sends to the
// shared server move to <ts>-shared, then config.toml, secrets.toml, the
// sends and the state fields kept go back and the binding goes; then the
// server starts. A failed start leaves the switch done (exit 40).
func undoConnect(ctx context.Context, e *env, plan bool) (connectResult, error) {
	res := connectResult{Plan: plan, Situation: joinUndo, Steps: []step{}}
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	if err := updateIncomplete(d, "connect --undo"); err != nil {
		return res, err
	}
	pre, meta, err := ownPreImage(d)
	if err != nil {
		return res, err
	}
	if pre == "" {
		return res, &output.Error{Code: output.Usage, Message: "this PC had no server of its own before connect: backsight uninstall removes the client"}
	}
	if cur, err := config.Load(d.ConfigFile(), d, home); err == nil && cur.Server.Mode == "local" {
		return res, &output.Error{Code: output.Usage, Message: "this PC already uses its own server (" + d.ConfigFile() + "): connect --undo has nothing to put back"}
	}
	raw, err := os.ReadFile(filepath.Join(pre, "config.toml"))
	if err == nil {
		_, err = os.Stat(filepath.Join(pre, "secrets.toml"))
	}
	if err != nil {
		return res, &output.Error{Code: output.Blocked, Message: pre + " does not hold this PC's config.toml and secrets.toml as they were (" + err.Error() + "): nothing was changed"}
	}
	res.URL, res.Bank = meta.URL, config.DefaultBank
	if cfg, err := config.Parse(raw, d.ConfigFile(), d, home); err == nil {
		res.URL, res.Bank = cfg.Server.URL, cfg.Server.Bank
	}
	res.Steps = []step{
		{Name: "history", Status: "planned", Detail: "this PC's sends to the shared server move to " + d.ConnectDir() + " (<time>-shared); those to its own server come back from " + pre},
		{Name: "config", Status: "planned", Detail: "put back " + d.ConfigFile() + " and " + d.SecretsFile() + " as they were before connect: [server] url " + res.URL + ", mode local"},
		{Name: "binding", Status: "planned", Detail: "forget the shared server's id in " + d.StateFile()},
		{Name: "server", Status: "planned", Detail: "backsight server start"},
	}
	res.RestartRequired = true
	if plan {
		return res, nil
	}
	if err := applyUndo(ctx, d, pre, now().UTC()); err != nil {
		return res, err
	}
	for i := range res.Steps[:3] {
		res.Steps[i].Status = "done"
		if !e.json { // before the server's start, which takes a while
			fmt.Fprint(e.stdout, stepLine(res.Steps[i]))
		}
	}
	res.NextSteps = append(res.NextSteps, openSessionStep)
	if sr := subcommand(ctx, "server", "start"); sr.Err != nil {
		res.Steps[3].Status = "failed"
		return res, &output.Error{Code: output.Partial, Resume: "backsight server start",
			Message: "this PC is back on its own server's config, but starting the server failed (" + sr.Err.Message + "): run backsight server start"}
	}
	res.Steps[3].Status = "done"
	return res, nil
}

// applyUndo is connect --undo's switch, under the maintenance lock. A
// failure before config.toml and secrets.toml are both back puts the
// shared server's back (exit 30).
func applyUndo(ctx context.Context, d paths.Dirs, pre string, at time.Time) error {
	unlock, _, err := serverMaintenanceLock(d, "connect --undo")
	if err != nil {
		return err
	}
	defer unlock()
	if err := waitSends(ctx, d, "connect --undo"); err != nil {
		return err
	}
	release, err := holdEpochLock(ctx, d, "connect --undo")
	if err != nil {
		return err
	}
	defer release()
	var keptCfg, keptSec, curCfg, curSec []byte
	for p, dst := range map[string]*[]byte{filepath.Join(pre, "config.toml"): &keptCfg, filepath.Join(pre, "secrets.toml"): &keptSec, d.ConfigFile(): &curCfg, d.SecretsFile(): &curSec} {
		if *dst, err = os.ReadFile(p); err != nil {
			return blocked(err)
		}
	}
	shared := keepDir(d, at, "-shared")
	rollback := func(cause error) error {
		var left []string
		if err := fsutil.WriteAtomic(d.ConfigFile(), curCfg, 0o600); err != nil {
			left = append(left, d.ConfigFile()+" ("+err.Error()+")")
		}
		if err := fsutil.WriteAtomic(d.SecretsFile(), curSec, 0o600); err != nil {
			left = append(left, d.SecretsFile()+" ("+err.Error()+")")
		}
		if err := undoRetainReset(d, shared, true, true); err != nil {
			left = append(left, "the sends kept in "+shared+" ("+err.Error()+")")
		}
		if len(left) > 0 {
			return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("connect --undo stopped (%s), and putting the shared server's setup back failed: %s", cause.Error(), strings.Join(left, "; "))}
		}
		return &output.Error{Code: output.RolledBack, Message: fmt.Sprintf("connect --undo stopped (%s): what it had changed is put back, this PC is as it was", cause.Error())}
	}
	// The restore epoch given here is replaced by the one kept in pre.
	if err := resetRetain(d, shared, state.RestoreEpoch{}, at); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return blocked(err)
		}
		return rollback(fmt.Errorf("moving the shared server's sends aside: %w", err))
	}
	if err := connectFault("undo-config"); err != nil {
		return rollback(err)
	}
	if err := fsutil.WriteAtomic(d.ConfigFile(), keptCfg, 0o600); err != nil {
		return rollback(err)
	}
	if err := connectFault("undo-secrets"); err != nil {
		return rollback(err)
	}
	if err := fsutil.WriteAtomic(d.SecretsFile(), keptSec, 0o600); err != nil {
		return rollback(err)
	}
	if err := undoRetainReset(d, pre, true, false); err != nil {
		return &output.Error{Code: output.Blocked, Message: "config.toml and secrets.toml are back as they were before connect, but this PC's sends to its own server are not (" + err.Error() +
			"): they are in " + pre + ", whose meta.json records the state they went with"}
	}
	// A process that read its config while the files were being put back
	// holds the generation resetRetain raised, with the shared server's
	// config or key: raised again, it sends nothing.
	if err = connectFault("undo-binding"); err == nil {
		err = state.UpdateWithin(d, "connect", 5*time.Second, func(s *state.State) error {
			s.RemoteServer = nil
			s.SendsGeneration++
			return nil
		})
	}
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: "this PC is back on its own server, but forgetting the shared server's id in " + d.StateFile() + " failed (" + err.Error() +
			"): the sends stop until it is removed; a process started during connect --undo may still send to the shared server"}
	}
	return nil
}

// keepDir is a directory of ConnectDir no earlier run used, named by at
// and suffix (a second run within the same second gets a number).
func keepDir(d paths.Dirs, at time.Time, suffix string) string {
	base := filepath.Join(d.ConnectDir(), at.Format("20060102T150405Z")+suffix)
	keep := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(keep); err != nil { // absent (any other error: Mkdir says it)
			return keep
		}
		keep = fmt.Sprintf("%s-%d", base, i)
	}
}

// connectSendsWait: how long connect, under the maintenance lock, waits
// for the sends that began before it; connectSendsPoll: how often it
// looks.
var connectSendsWait, connectSendsPoll = 2 * time.Minute, time.Second

// waitSends waits for the sends that took their session's lock before
// command took the maintenance lock: each would save its cursor once this
// PC's sends started over, and the new server would skip that session.
// Nothing is changed yet when it refuses (exit 20).
func waitSends(ctx context.Context, d paths.Dirs, command string) error {
	deadline := time.Now().Add(connectSendsWait)
	for running := retain.SendsRunning(d); len(running) > 0; running = retain.SendsRunning(d) {
		if time.Now().After(deadline) {
			return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("a send of session %s is still running after %s: nothing was changed. "+
				"Run backsight %s again later", running[0], connectSendsWait, command)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(connectSendsPoll):
		}
	}
	return nil
}

// holdEpochLock takes epoch.lock for command, waiting up to
// connectSendsWait for a restore-epoch reconcile that holds it: one
// running while the sends start over would drop cursors among the new
// server's. A reconcile that takes it after sees the new generation
// under it and stops. Nothing is changed yet when it refuses (exit 20).
func holdEpochLock(ctx context.Context, d paths.Dirs, command string) (release func(), err error) {
	path := filepath.Join(d.LocksDir(), "epoch.lock")
	deadline := time.Now().Add(connectSendsWait)
	for {
		l, _, err := state.Acquire(path, command) // a dead reconcile's lock is taken over
		if err == nil {
			return func() { _ = l.Release() }, nil
		}
		var held *state.HeldError
		if !errors.As(err, &held) {
			return nil, blocked(err)
		}
		if time.Now().After(deadline) {
			return nil, &output.Error{Code: output.Blocked, Message: "a restore-epoch reconcile is running: nothing was changed. Run backsight " + command + " again later"}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(connectSendsPoll):
		}
	}
}

// updateIncomplete refuses connect while an update did not finish: its
// way back writes state.json and config.toml back as they were before it,
// which would undo the switch but for the key and the moved sends.
func updateIncomplete(d paths.Dirs, command string) error {
	if state.UpdateIncomplete(d) {
		return &output.Error{Code: output.Blocked, Message: "an update did not finish (" + d.UpdateMarker() + "): run backsight doctor for the way back, then backsight " + command + " again; nothing was changed"}
	}
	return nil
}

// connectFault fails a step of applyConnect in tests; nil in the binary.
var connectFault = func(string) error { return nil }

// runConnect joins tok's server: read and check the token, probe the
// server, show the plan, ask, write, then run setup's client steps.
// Nothing is written before the go.
func runConnect(ctx context.Context, e *env, f connectFlags) (connectResult, error) {
	res := connectResult{Plan: f.plan, Steps: []step{}}
	// A token in any other value would be quoted by an error, or kept in
	// setup's saved answers.
	values := []string{f.answersFile, f.configDir}
	for _, a := range f.answers {
		_, v, _ := strings.Cut(a, "=")
		values = append(values, a, v)
	}
	if slices.ContainsFunc(values, looksLikeToken) {
		return res, &output.Error{Code: output.Usage, Message: tokenArgument}
	}
	var tok token.Token
	var err error
	if f.preread != nil {
		tok = *f.preread
	} else if tok, err = readToken(e, f.token, f.tokenFile); err != nil {
		return res, err
	}
	if err := publicAddress(tok); err != nil {
		return res, err
	}
	res.URL, res.Bank, res.ServerID = tok.URL, tok.Bank, tok.ServerID
	answers, err := parseAnswers(f.answers, f.answersFile)
	if err != nil {
		return res, err
	}
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	if err := updateIncomplete(d, "connect"); err != nil {
		return res, err
	}
	situation, cfg, err := situationOf(d, home, tok)
	if err != nil {
		return res, err
	}
	res.Situation = situation
	var localSec config.Secrets // this PC's own server's
	if situation == joinOwn {
		if localSec, err = config.LoadSecrets(d.SecretsFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, blocked(err)
		}
		// A deployed server must answer: connect backs it up before it
		// stops it. One not deployed holds nothing to compare.
		deployed, derr := runtimeFor(*cfg).Deployed()
		var id string
		cl, err := hindsight.New(cfg.Server.URL, localSec.APIKey, 10*time.Second)
		if err == nil {
			id, err = retain.ReadServerID(ctx, cl, tok.Bank)
		}
		switch {
		case err != nil && (derr != nil || deployed != ""):
			return res, redacted(&output.Error{Code: output.Blocked, Message: "this PC's server at " + cfg.Server.URL + " does not answer (" + err.Error() +
				"): connect checks it is not the token's server and backs it up before stopping it; start it (backsight server start) and run connect again: nothing was changed"}, localSec)
		case err == nil && id == tok.ServerID:
			return res, &output.Error{Code: output.Usage, Message: "this token is for this machine's own server: it is already connected"}
		}
	}
	p, err := probeToken(ctx, tok)
	if err != nil {
		return res, err
	}
	res.HindsightVersion, res.Skew = p.Version, p.Skew
	switch p.Skew {
	case doctor.Warn:
		res.Warnings = append(res.Warnings, fmt.Sprintf("the server runs Hindsight %s and this backsight pins %s: on the server's host, backsight update apply; or update this PC to the host's backsight", p.Version, server.PinnedVersion()))
	case doctor.Info:
		res.Notes = append(res.Notes, fmt.Sprintf("the server runs Hindsight %s and this backsight pins %s: only the patch differs, nothing to do", p.Version, server.PinnedVersion()))
	case "":
		res.Warnings = append(res.Warnings, "the server's Hindsight version could not be read: backsight doctor compares it with this backsight's pin once connected")
	}
	var raw []byte // nil: no config.toml yet
	if cfg != nil {
		if raw, err = os.ReadFile(d.ConfigFile()); err != nil {
			return res, blocked(err)
		}
	}
	_, newCfg, err := connectConfig(d, home, raw, tok)
	if err != nil {
		return res, err
	}
	var sessions int
	res.Steps, sessions = connectPlan(d, home, f, tok, situation, newCfg)
	if situation == joinOwn {
		own, warning := ownServerPlan(ctx, *cfg, localSec, sessions, transcriptFiles(home, f.configDir))
		res.OwnServer = own
		if warning != "" {
			res.Warnings = append(res.Warnings, warning)
		}
		res.Steps = append([]step{ownServerStep(own.Deployed)}, res.Steps...)
	}
	res.RestartRequired = cfg == nil || cfg.Server.URL != tok.URL
	// A host extracting with a Claude subscription extracts this PC's
	// sessions on it too: the caveat is asked here, before any switch. A
	// reconnect to the same kind of host, already accepted, is not asked
	// again.
	askAck := tok.LLM.Provider == "claude-code" && answers["ack_subscription"] != "yes"
	if askAck && cfg != nil && cfg.LLM.Provider == "claude-code" {
		if rec, _ := setup.Load(d.SetupFile()); rec != nil && rec.Answers["ack_subscription"] == "yes" {
			askAck = false
		}
	}
	ack := setup.SubscriptionQuestion(true)
	if askAck {
		res.Questions = []setup.Question{ack}
		res.Steps = append(res.Steps, step{Name: "ack_subscription", Status: "planned", Detail: "ask: " + ack.Prompt})
	}
	if f.plan {
		return res, nil
	}

	resume, consentResume, planCmd := "backsight connect --token-file F --answer consent=yes", "backsight connect --token-file F --answer consent=yes", "backsight connect --token-file F --plan"
	switch {
	case f.resume != "":
		resume, consentResume, planCmd = f.resume, f.resume+" --answer consent=yes", f.resume+" --plan"
	case f.tokenFile != "":
		if abs, err := filepath.Abs(f.tokenFile); err == nil {
			resume = "backsight connect --token-file " + resumeWord(abs) + " --answer consent=yes"
		}
	}
	interactive := !e.json && !f.nonInteractive && stdinIsTerminal() && stdoutIsTerminal()
	switch consent := answers["consent"]; {
	case consent == "" && interactive:
		fmt.Fprint(e.stdout, connectPlanText(res))
		if askAck {
			fmt.Fprintf(e.stdout, "\n%s\n", ack.Prompt)
			if !confirm(e, "Accept? [y/N] ") {
				res.Declined = true
				return res, nil
			}
			answers["ack_subscription"], askAck = "yes", false
		}
		if !confirm(e, "Join this server? [y/N] ") {
			res.Declined = true
			return res, nil
		}
	case consent == "":
		return res, &output.Error{Code: output.NeedsUser, Message: "connect changes this PC's config and key: read the plan (" + planCmd + "), then rerun with --answer consent=yes",
			Resume: consentResume}
	case consent != "yes":
		res.Declined = true
		return res, nil
	}
	if askAck && interactive { // the consent was given as an answer
		fmt.Fprintf(e.stdout, "\n%s\n", ack.Prompt)
		if !confirm(e, "Accept? [y/N] ") {
			res.Declined = true
			return res, nil
		}
		answers["ack_subscription"], askAck = "yes", false
	}
	if askAck {
		return res, &output.Error{Code: output.NeedsUser, Message: "the server's host extracts with a Claude subscription: connect asks you to accept its terms before it changes anything (nothing was changed); rerun with --answer ack_subscription=yes",
			Questions: res.Questions, Resume: resume + " --answer ack_subscription=yes"}
	}

	var own server.Runtime // this PC's server, stopped once its backup verified
	if res.OwnServer != nil && res.OwnServer.Deployed {
		archive, err := backupOwnServer(ctx, d, *cfg, localSec)
		if err != nil {
			return res, redacted(err, config.Secrets{APIKey: tok.APIKey})
		}
		res.OwnServer.Backup = archive
		if err := connectFault("backed-up"); err != nil {
			return res, err
		}
		own = runtimeFor(*cfg)
	}
	at := now().UTC()
	if err := applyConnect(ctx, d, home, tok, p, situation, at, resume, own); err != nil {
		if res.OwnServer != nil && res.OwnServer.Backup != "" {
			err = afterOwnBackup(err, res.OwnServer.Backup, d, localSec)
		}
		return res, redacted(redacted(err, localSec), config.Secrets{APIKey: tok.APIKey})
	}
	for i := range res.Steps {
		if res.Steps[i].Status == "planned" && res.Steps[i].Name != "setup" {
			res.Steps[i].Status = "done"
		}
	}
	if f.tokenFile != "" {
		file := f.tokenFile
		if abs, err := filepath.Abs(file); err == nil {
			file = abs
		}
		res.NextSteps = append(res.NextSteps, "delete "+file+": it holds the API key")
	}
	if !e.json {
		for _, s := range res.Steps {
			if s.Status == "done" {
				fmt.Fprint(e.stdout, stepLine(s))
			}
		}
	}

	// Setup's client steps, with the consent already given; its other
	// questions as usual. Its errors resume with backsight setup: the
	// config is adopted now.
	var setupAnswers []string
	for _, a := range f.answers {
		if k, _, _ := strings.Cut(a, "="); strings.TrimSpace(k) != "consent" {
			setupAnswers = append(setupAnswers, a)
		}
	}
	if answers["ack_subscription"] == "yes" && !slices.ContainsFunc(setupAnswers, func(a string) bool { k, _, _ := strings.Cut(a, "="); return strings.TrimSpace(k) == "ack_subscription" }) {
		setupAnswers = append(setupAnswers, "ack_subscription=yes") // given at the prompt
	}
	sr, err := runSetup(ctx, e, setupFlags{answers: append(setupAnswers, "consent=yes"), answersFile: f.answersFile, nonInteractive: f.nonInteractive, configDir: f.configDir})
	res.Setup = &sr
	if res.RestartRequired && !sr.RestartRequired { // else setup says it
		res.NextSteps = append(res.NextSteps, openSessionStep)
	}
	for i := range res.Steps {
		if res.Steps[i].Name == "setup" {
			res.Steps[i].Status = "done"
			if err != nil {
				res.Steps[i].Status = "failed"
			}
		}
	}
	res.RestartRequired = res.RestartRequired || sr.RestartRequired
	return res, err
}

// connectGOOS is the OS connect's messages describe; tests replace it.
var connectGOOS = runtime.GOOS

// privateNote says how a secret file backsight keeps private is protected:
// its mode where mode bits apply, its access list (fsutil.MakePrivate) on
// Windows.
func privateNote(goos string) string {
	if goos == "windows" {
		return "(private: you, SYSTEM and Administrators only)"
	}
	return "(mode 0600)"
}

// connectPlan lists what connect will do, in order, and counts the past
// sessions setup offers to send.
func connectPlan(d paths.Dirs, home string, f connectFlags, tok token.Token, situation string, cfg config.Config) ([]step, int) {
	verb := "edit "
	if situation == joinNew {
		verb = "write "
	}
	llm := "the host's LLM for cost estimates"
	if tok.LLM.Model != "" {
		llm = "the host's LLM (" + tok.LLM.Provider + "/" + tok.LLM.Model + ") for cost estimates"
	}
	steps := []step{{Name: "config", Status: "planned", Detail: verb + d.ConfigFile() + ": [server] url " + tok.URL + ", bank " + tok.Bank + ", mode remote; [llm] " + llm + ", never a key"}}
	steps = append(steps, step{Name: "secrets", Status: "planned", Detail: "api_key in " + d.SecretsFile() + " " + privateNote(connectGOOS)})
	since, idx := now().UTC(), (*retain.SentIndex)(nil)
	restart := situation == joinOther || situation == joinOwn
	switch situation {
	case joinOther:
		steps = append(steps, step{Name: "history", Status: "planned", Detail: "this PC's sends start over on that server: what it already holds is not sent again " +
			"(the session cursors, sent-index and split sessions move to " + d.ConnectDir() + ")"})
	case joinOwn:
		steps = append(steps, step{Name: "history", Status: "planned", Detail: "this PC's sends start over on that server: what it already holds is not sent again " +
			"(the session cursors, sent-index and split sessions move to " + d.ConnectDir() + ", with config.toml and secrets.toml as they are: connect --undo puts them back)"})
	}
	if situation == joinSame {
		if st, err := state.Load(d.StateFile()); err == nil && st.RetainSince != nil {
			since = *st.RetainSince
		}
		idx, _ = retain.LoadSentIndex(d.SentIndexDir())
	}
	if idx == nil { // a new start: nothing sent to that server from here
		idx, _ = retain.LoadSentIndex(filepath.Join(d.ConnectDir(), "none"))
	}
	steps = append(steps, step{Name: "binding", Status: "planned", Detail: "record server " + tok.ServerID + " in " + d.StateFile() + ": another server answering at that address stops the sends"})
	detail := "then backsight setup: client install, retention, scheduler, verify"
	sessions := 0
	if idx != nil {
		// A new start counts as the flusher will once the cursors moved:
		// what the old server holds is sent again.
		n, cost := backfillSurvey(d, home, projectsDir(home, f.configDir), cfg, since, idx, restart)
		if n > 0 {
			bound := ""
			if restart {
				bound = "at most "
			}
			detail += fmt.Sprintf(", and the offer to send %s%d past session(s) (%s)", bound, n, cost)
		}
		sessions = n
	}
	return append(steps, step{Name: "setup", Status: "planned", Detail: detail}), sessions
}

// projectsDir is Claude Code's transcripts directory, configDir (the
// --config-dir flag) first.
func projectsDir(home, configDir string) string {
	getenv := os.Getenv
	if configDir != "" {
		getenv = func(k string) string {
			if k == "CLAUDE_CONFIG_DIR" {
				return configDir
			}
			return os.Getenv(k)
		}
	}
	return claudecode.Locate(home, getenv).Projects
}

// transcriptFiles are the session transcripts a backfill reads.
func transcriptFiles(home, configDir string) []string {
	files, _ := filepath.Glob(filepath.Join(projectsDir(home, configDir), "*", "*.jsonl"))
	return files
}

// ownServerPlan is what joining another server does to this PC's own
// (cfg, sec): sessions are sent again, and the saved notes of its bank no
// transcript left here holds stay there. warning says how many, or why
// they could not be counted.
func ownServerPlan(ctx context.Context, cfg config.Config, sec config.Secrets, sessions int, transcripts []string) (*ownServer, string) {
	own := &ownServer{Sessions: sessions}
	deployed, err := runtimeFor(cfg).Deployed()
	own.Deployed = err != nil || deployed != "" // unreadable: the backup says
	if !own.Deployed {
		return own, ""
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err != nil {
		return own, server.Redact("the saved notes of this PC's server could not be counted: "+err.Error(), sec)
	}
	orphans, total, err := retain.CountOrphanNotes(ctx, cl, cfg.Server.Bank, transcripts)
	if err != nil {
		return own, server.Redact("the saved notes of this PC's server could not be counted ("+err.Error()+"): start it (backsight server start) and run the plan again to see them", sec)
	}
	own.SavedNotes, own.OrphanNotes = &total, &orphans
	if orphans == 0 {
		return own, ""
	}
	return own, fmt.Sprintf("%d of %d saved notes have no transcript left on this PC: they stay in this PC's old bank only", orphans, total)
}

// ownServerStep is the plan's step for this PC's own server.
func ownServerStep(deployed bool) step {
	if !deployed {
		return step{Name: "server", Status: "skipped", Detail: "this PC's server is not deployed: nothing to back up or stop"}
	}
	return step{Name: "server", Status: "planned", Detail: "back up and verify this PC's server, then stop it (data and backups kept; connect --undo starts it again)"}
}

// backupOwnServer backs up this PC's server and verifies the archive
// with the install's identities, before applyConnect stops it. A failed
// backup or verify changes nothing but the backup identity, generated
// into secrets.toml by a first backup (an archive made is kept). sec is
// secrets.toml as it was before.
func backupOwnServer(ctx context.Context, d paths.Dirs, cfg config.Config, sec config.Secrets) (archive string, err error) {
	unchanged := func(what string, err error) error {
		changed := "nothing was changed"
		if fresh, ferr := config.LoadSecrets(d.SecretsFile()); ferr == nil && sec.BackupIdentity == "" && fresh.BackupIdentity != "" {
			changed = "nothing else was changed than the backup identity added to secrets.toml (backsight backup identity show)"
		}
		return redacted(&output.Error{Code: output.Blocked, Message: "connect stopped: " + what + " (" + err.Error() + "): " + changed}, sec)
	}
	b, err := createBackup(ctx, nil) // takes the maintenance lock
	if err == nil && b.Skipped != "" {
		err = errors.New(b.Skipped)
	}
	if err != nil {
		return "", unchanged("backing up this PC's server failed", err)
	}
	// The first backup may have made the identity: read the secrets again.
	fresh, err := config.LoadSecrets(d.SecretsFile())
	var v backupVerifyResult
	if err == nil {
		var ids []agelib.Identity
		if _, _, ids, err = backupKeys(d, cfg, fresh, false); err == nil {
			v, err = verifyArchive(ctx, d, cfg, ids, b.Path)
		}
	}
	if !v.OK { // a result that could not be recorded still verified
		if err == nil {
			err = errors.New("its counts differ from its manifest")
		}
		return b.Path, unchanged("the backup "+b.Path+" did not verify", err)
	}
	return b.Path, nil
}

// afterOwnBackup is applyConnect's error once this PC's own server was
// backed up to archive: the backup was made and stays (with the backup
// identity it may have added to secrets.toml), so "nothing was changed"
// says so, and any other message names it too.
func afterOwnBackup(err error, archive string, d paths.Dirs, sec config.Secrets) error {
	made := "this PC's server was backed up to " + archive
	if fresh, ferr := config.LoadSecrets(d.SecretsFile()); ferr == nil && sec.BackupIdentity == "" && fresh.BackupIdentity != "" {
		made += ", and the backup identity it needed added to secrets.toml (backsight backup identity show)"
	}
	oe, ok := err.(*output.Error) //nolint:errorlint // its own message only, not one it wraps
	if !ok {
		return fmt.Errorf("%w; before that, %s", err, made)
	}
	out := *oe
	if strings.Contains(out.Message, "nothing was changed") {
		out.Message = strings.Replace(out.Message, "nothing was changed", "nothing else was changed than the backup ("+made+")", 1)
	} else {
		out.Message += "; before that, " + made
	}
	return &out
}

// applyConnect writes what the plan said, under the maintenance lock
// (hooks queue, jobs skip): the retain reset first (it records the server
// config.toml still names), then config.toml, the key, and the binding
// last; leaving this PC's own server (own, nil otherwise), it stops that
// server first, once the plan is read again, and keeps its config.toml
// and secrets.toml with the moved retain state. Once the key is written,
// the sends generation is raised again: a process that read its config
// while connect ran holds the old config or key with the generation
// resetRetain raised, and sends nothing. A failure up to that raise puts
// back what was done (the key too, unless another writer changed it
// since; the sends generation is raised once more, never put back),
// starting a stopped server again
// (exit 30; 20 when that fails too, saying what is left); a failure of the
// binding leaves the rest done, for resume to finish (exit 40).
func applyConnect(ctx context.Context, d paths.Dirs, home string, tok token.Token, p probe, situation string, at time.Time, resume string, own server.Runtime) error {
	unlock, _, err := serverMaintenanceLock(d, "connect")
	if err != nil {
		return err
	}
	defer unlock()
	if situation == joinOther || situation == joinOwn { // the sends start over
		if err := waitSends(ctx, d, "connect"); err != nil {
			return err
		}
		release, err := holdEpochLock(ctx, d, "connect")
		if err != nil {
			return err
		}
		defer release()
	}
	// Read again under the lock: the plan was made from what was there.
	want, raw := fsutil.Absent, []byte(nil)
	if situation != joinNew {
		if raw, err = os.ReadFile(d.ConfigFile()); err != nil {
			return blocked(err)
		}
		if want, err = fsutil.HashFile(d.ConfigFile()); err != nil {
			return blocked(err)
		}
	}
	out, _, err := connectConfig(d, home, raw, tok)
	if err != nil {
		return err
	}
	keep, wrote, stopped := "", false, false
	// secrets.toml before the key was written, and as connect wrote it
	// (keyWritten, fsutil.Absent until then).
	keyWas, keyWasHash, keyWritten := []byte(nil), fsutil.Absent, fsutil.Absent
	rollback := func(cause error) error {
		var left []string
		if keyWritten != fsutil.Absent {
			if err := putKeyBack(d.SecretsFile(), keyWas, keyWasHash, keyWritten); errors.Is(err, fsutil.ErrConflict) {
				left = append(left, d.SecretsFile()+" is no longer as connect wrote it (another writer changed it, or it could not be read back): it is kept as it is, and may still hold the API key of "+tok.URL)
			} else if err != nil {
				left = append(left, d.SecretsFile()+" still holds the API key of "+tok.URL+" ("+err.Error()+")")
			}
		}
		keepCopy := false // keep holds the config.toml to put back by hand
		if wrote {
			err := connectFault("putback")
			switch {
			case err != nil:
			case raw == nil:
				err = os.Remove(d.ConfigFile())
			default:
				err = fsutil.WriteAtomic(d.ConfigFile(), raw, 0o600)
			}
			if err != nil {
				msg := d.ConfigFile() + " still names " + tok.URL + " (" + err.Error() + ")"
				if keep != "" && raw != nil {
					copied := filepath.Join(keep, "config.toml")
					if _, serr := os.Stat(copied); serr == nil || fsutil.WriteAtomic(copied, raw, 0o600) == nil {
						keepCopy = true
						msg += ": copy " + copied + ", the file as it was, over it"
					}
				}
				left = append(left, msg)
			}
		}
		if stopped {
			if err := own.Start(ctx); err != nil {
				left = append(left, "this PC's server is stopped ("+err.Error()+"): backsight server start starts it")
			}
		}
		if keep != "" {
			if err := undoRetainReset(d, keep, !keepCopy, true); err != nil {
				msg := "this PC's session cursors, sent-index and split sessions may still be in " + keep +
					", and state.json's retain_since, backfill and restore_epoch changed; " + filepath.Join(keep, "meta.json") + " records them as they were (" + err.Error() + ")"
				if wrote {
					// The generation stays at the reset's: a process that
					// read config.toml or the key as connect wrote them
					// passes its gate once the maintenance lock is released.
					msg += "; a process started during connect may still send to " + tok.URL + " until state.json's sends_generation is raised (any later connect does)"
				}
				left = append(left, msg)
			}
		}
		if len(left) > 0 {
			return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("connect stopped (%s), and putting this PC back failed: %s", cause.Error(), strings.Join(left, "; "))}
		}
		return &output.Error{Code: output.RolledBack, Message: fmt.Sprintf("connect stopped (%s): what it had changed is put back, this PC is as it was", cause.Error())}
	}
	if own != nil {
		stopped = true // a failed stop may have stopped part of it
		if err := own.Stop(ctx); err != nil {
			return rollback(fmt.Errorf("stopping this PC's server: %w", err))
		}
	}
	if situation == joinOther || situation == joinOwn {
		keep = keepDir(d, at, "")
		if err := resetRetain(d, keep, state.RestoreEpoch{Bank: tok.Bank, Epoch: p.Epoch, ReconciledAt: at, RestoredAt: p.RestoredAt.UTC()}, at); err != nil {
			if errors.Is(err, fs.ErrExist) { // another run's directory: not this one's to undo
				keep = ""
			}
			return rollback(fmt.Errorf("moving this PC's sends aside: %w", err))
		}
	}
	if situation == joinOwn {
		if err := keepOwnConfig(d, keep, raw); err != nil {
			return rollback(fmt.Errorf("keeping this PC's config.toml and secrets.toml in %s: %w", keep, err))
		}
	}
	if err := connectFault("config"); err != nil {
		return rollback(err)
	}
	if err := fsutil.MkdirPrivate(d.Config); err != nil {
		return rollback(err)
	}
	if !bytes.Equal(out, raw) {
		if err := fsutil.ReplaceIfUnchanged(d.ConfigFile(), out, 0o600, want); err != nil {
			return rollback(fmt.Errorf("%s changed while connect wrote it: %w", d.ConfigFile(), err))
		}
		wrote = true
	}
	if err := connectFault("secrets"); err != nil {
		return rollback(err)
	}
	if keyWas, keyWasHash, err = fsutil.ReadHashed(d.SecretsFile()); err != nil {
		return rollback(err)
	}
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("api_key", tok.APIKey) }); err != nil {
		return rollback(fmt.Errorf("writing the API key to %s: %w", d.SecretsFile(), err))
	}
	if keyWritten, err = fsutil.HashFile(d.SecretsFile()); err != nil || keyWritten == fsutil.Absent {
		keyWritten = keyUnread
		if err == nil {
			err = fs.ErrNotExist
		}
		return rollback(fmt.Errorf("reading %s back after writing the API key: %w", d.SecretsFile(), err))
	}
	if situation == joinOther || situation == joinOwn {
		// Raised with the reset already (atomic with dropping backfill),
		// raised again now config.toml and the key name the new server.
		err := connectFault("generation")
		if err == nil {
			err = state.UpdateWithin(d, "connect", 5*time.Second, func(s *state.State) error { s.SendsGeneration++; return nil })
		}
		if err != nil {
			return rollback(fmt.Errorf("raising this PC's sends generation in %s: %w", d.StateFile(), err))
		}
	}
	err = connectFault("binding")
	if err == nil {
		err = state.UpdateWithin(d, "connect", 5*time.Second, func(s *state.State) error {
			s.RemoteServer = &state.RemoteServer{ID: tok.ServerID, URL: tok.URL, ConnectedAt: at}
			return nil
		})
	}
	if err != nil {
		return &output.Error{Code: output.Partial, Resume: resume,
			Message: fmt.Sprintf("config.toml and the API key now name %s, but recording its server id in %s failed (%s): run %s to finish", tok.URL, d.StateFile(), err.Error(), resume)}
	}
	return nil
}

func connectPlanText(r connectResult) string {
	var b strings.Builder
	will := "\nconnect will:\n"
	if r.Situation == joinUndo {
		fmt.Fprintf(&b, "this PC's own server at %s (bank %s)\n", r.URL, r.Bank)
		will = "\nconnect --undo will:\n"
	} else {
		fmt.Fprintf(&b, "server %s at %s (bank %s, Hindsight %s)\n", r.ServerID, r.URL, r.Bank, r.HindsightVersion)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning  %s\n", w)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note     %s\n", n)
	}
	b.WriteString(will)
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "  %-8s %s\n", s.Name, s.Detail)
	}
	return b.String()
}

func connectText(r connectResult) string {
	switch {
	case len(r.Steps) == 0:
		return ""
	case r.Declined:
		return "nothing changed\n"
	case r.Plan:
		return connectPlanText(r)
	case r.Situation == joinUndo: // the switch's steps are printed as they are done
		var b strings.Builder
		for _, s := range r.Steps {
			if s.Name == "server" && s.Status != "planned" {
				b.WriteString(stepLine(s))
			}
		}
		for _, n := range r.NextSteps {
			b.WriteString("- " + n + "\n")
		}
		return b.String()
	}
	if r.Setup == nil { // stopped before setup: what it would have done
		return connectPlanText(r)
	}
	var b strings.Builder
	b.WriteString(setupText(*r.Setup))
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning  %s\n", w)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note     %s\n", n)
	}
	for _, n := range r.NextSteps {
		b.WriteString("- " + n + "\n")
	}
	return b.String()
}
