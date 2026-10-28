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
	"slices"
	"strings"
	"time"

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
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/token"
)

// What this PC is to the server a token names (decision 0073).
const (
	joinNew   = "new"          // no config.toml yet
	joinSame  = "same-server"  // a client of that server already
	joinOther = "other-server" // a client of another server
	joinOwn   = "own-server"   // a PC with a server of its own
)

// tokenArgument is the refusal of a token given on the command line: it
// never quotes the value, and the key it holds is in the shell's history.
const tokenArgument = "the token holds the API key: pass it on stdin (--token -) or in a file (--token-file F); " +
	"it is now in your shell history: run backsight server token --rotate on the host"

type connectFlags struct {
	token, tokenFile string
	plan             bool
	answers          []string
	answersFile      string
	nonInteractive   bool
	configDir        string
}

// connectResult is what connect did or would do. It never holds the key
// nor the token.
type connectResult struct {
	URL              string        `json:"url"`
	Bank             string        `json:"bank"`
	ServerID         string        `json:"server_id"`
	HindsightVersion string        `json:"hindsight_version,omitempty"`
	Situation        string        `json:"situation"`
	Skew             doctor.Status `json:"version_skew,omitempty"`
	Plan             bool          `json:"plan"`
	Declined         bool          `json:"declined,omitempty"`
	Warnings         []string      `json:"warnings,omitempty"`
	Steps            []step        `json:"steps"`
	Setup            *setupResult  `json:"setup,omitempty"`
	RestartRequired  bool          `json:"restart_required"`
	NextSteps        []string      `json:"next_steps,omitempty"`
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

// probe is what connect learnt from the server, read-only.
type probe struct {
	Version string
	Skew    doctor.Status
	Epoch   int
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
	v, err := cl.Version(ctx)
	if err != nil {
		return probe{}, fail("reading the server version", err)
	}
	ep, err := retain.ReadEpoch(ctx, cl, tok.Bank)
	if err != nil {
		return probe{}, fail("reading the bank's restore epoch", err)
	}
	return probe{Version: v.APIVersion, Skew: doctor.VersionSkew(v.APIVersion, server.PinnedVersion()), Epoch: ep.Epoch}, nil
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
}

// resetRetain starts this PC's sends over for another server: the session
// cursors and the sent-index move into keep (a new directory), with
// meta.json recording the state they went with and the server they were
// for (config.toml as it is now). Live retain then starts at at, the
// backfill record is dropped and the restore epoch is epoch, the new
// server's: resync sends each session's records the server lacks, never
// what it holds. Tombstones stay: a purged session never goes out again.
func resetRetain(d paths.Dirs, keep string, epoch state.RestoreEpoch, at time.Time) error {
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
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
	for _, dir := range []string{d.CursorsDir(), d.SentIndexDir()} {
		if err := os.Rename(dir, filepath.Join(keep, filepath.Base(dir))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	since := at.UTC().Truncate(time.Second)
	return state.UpdateWithin(d, "connect", 5*time.Second, func(s *state.State) error {
		s.RetainSince, s.Backfill, s.RestoreEpoch = &since, nil, &epoch
		return nil
	})
}

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
	tok, err := readToken(e, f.token, f.tokenFile)
	if err != nil {
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
	situation, cfg, err := situationOf(d, home, tok)
	if err != nil {
		return res, err
	}
	res.Situation = situation
	if situation == joinOwn {
		return res, &output.Error{Code: output.Usage, Message: "this PC runs its own server: connect lands in the next release step"}
	}
	p, err := probeToken(ctx, tok)
	if err != nil {
		return res, err
	}
	res.HindsightVersion, res.Skew = p.Version, p.Skew
	if p.Skew == doctor.Warn {
		res.Warnings = append(res.Warnings, fmt.Sprintf("the server runs Hindsight %s and this backsight pins %s: on the server's host, backsight update apply; or update this PC to the host's backsight", p.Version, server.PinnedVersion()))
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
	res.Steps = connectPlan(d, home, f, tok, situation, newCfg)
	res.RestartRequired = cfg == nil || cfg.Server.URL != tok.URL
	if f.plan {
		return res, nil
	}

	interactive := !e.json && !f.nonInteractive && stdinIsTerminal() && stdoutIsTerminal()
	switch consent := answers["consent"]; {
	case consent == "" && interactive:
		fmt.Fprint(e.stdout, connectPlanText(res))
		if !confirm(e, "Join this server? [y/N] ") {
			res.Declined = true
			return res, nil
		}
	case consent == "":
		return res, &output.Error{Code: output.NeedsUser, Message: "connect changes this PC's config and key: read the plan (backsight connect --token-file F --plan), then rerun with --answer consent=yes",
			Resume: "backsight connect --token-file F --answer consent=yes"}
	case consent != "yes":
		res.Declined = true
		return res, nil
	}

	at := now().UTC()
	if err := applyConnect(d, home, tok, p, situation, at); err != nil {
		return res, redacted(err, config.Secrets{APIKey: tok.APIKey})
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
	sr, err := runSetup(ctx, e, setupFlags{answers: append(setupAnswers, "consent=yes"), answersFile: f.answersFile, nonInteractive: f.nonInteractive, configDir: f.configDir})
	res.Setup = &sr
	if res.RestartRequired && !sr.RestartRequired { // else setup says it
		res.NextSteps = append(res.NextSteps, "restart Claude Code (or run /reload-plugins): an open session keeps the server it started with")
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

// connectPlan lists what connect will do, in order.
func connectPlan(d paths.Dirs, home string, f connectFlags, tok token.Token, situation string, cfg config.Config) []step {
	verb := "edit "
	if situation == joinNew {
		verb = "write "
	}
	llm := "the host's LLM for cost estimates"
	if tok.LLM.Model != "" {
		llm = "the host's LLM (" + tok.LLM.Provider + "/" + tok.LLM.Model + ") for cost estimates"
	}
	steps := []step{{Name: "config", Status: "planned", Detail: verb + d.ConfigFile() + ": [server] url " + tok.URL + ", bank " + tok.Bank + ", mode remote; [llm] " + llm + ", never a key"}}
	steps = append(steps, step{Name: "secrets", Status: "planned", Detail: "api_key in " + d.SecretsFile() + " (mode 0600)"})
	since, idx := now().UTC(), (*retain.SentIndex)(nil)
	if situation == joinOther {
		steps = append(steps, step{Name: "history", Status: "planned", Detail: "this PC's sends start over on that server: what it already holds is not sent again " +
			"(the session cursors and sent-index move to " + d.ConnectDir() + ")"})
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
	getenv := os.Getenv
	if f.configDir != "" {
		getenv = func(k string) string {
			if k == "CLAUDE_CONFIG_DIR" {
				return f.configDir
			}
			return os.Getenv(k)
		}
	}
	detail := "then backsight setup: client install, retention, scheduler, verify"
	if idx != nil {
		if n, cost := backfillSurvey(d, home, claudecode.Locate(home, getenv).Projects, cfg, since, idx); n > 0 {
			bound := ""
			if situation == joinOther {
				bound = "at most "
			}
			detail += fmt.Sprintf(", and the offer to send %s%d past session(s) (%s)", bound, n, cost)
		}
	}
	return append(steps, step{Name: "setup", Status: "planned", Detail: detail})
}

// applyConnect writes what the plan said, under the maintenance lock
// (hooks queue, jobs skip): the retain reset first (it records the server
// config.toml still names), then config.toml, the key, and the binding
// last, so a failure part way leaves a rerun of connect to finish it.
func applyConnect(d paths.Dirs, home string, tok token.Token, p probe, situation string, at time.Time) error {
	unlock, _, err := serverMaintenanceLock(d, "connect")
	if err != nil {
		return err
	}
	defer unlock()
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
	if situation == joinOther {
		keep := filepath.Join(d.ConnectDir(), at.Format("20060102T150405Z"))
		if err := resetRetain(d, keep, state.RestoreEpoch{Bank: tok.Bank, Epoch: p.Epoch, ReconciledAt: at}, at); err != nil {
			return blocked(fmt.Errorf("moving this PC's sends aside: %w", err))
		}
	}
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		return blocked(err)
	}
	if !bytes.Equal(out, raw) {
		if err := fsutil.ReplaceIfUnchanged(d.ConfigFile(), out, 0o600, want); err != nil {
			return blocked(fmt.Errorf("%s changed while connect wrote it: %w", d.ConfigFile(), err))
		}
	}
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("api_key", tok.APIKey) }); err != nil {
		return blocked(fmt.Errorf("writing the API key to %s: %w", d.SecretsFile(), err))
	}
	return state.UpdateWithin(d, "connect", 5*time.Second, func(s *state.State) error {
		s.RemoteServer = &state.RemoteServer{ID: tok.ServerID, URL: tok.URL, ConnectedAt: at}
		return nil
	})
}

func connectPlanText(r connectResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "server %s at %s (bank %s, Hindsight %s)\n", r.ServerID, r.URL, r.Bank, r.HindsightVersion)
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning  %s\n", w)
	}
	b.WriteString("\nconnect will:\n")
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
	}
	if r.Setup == nil { // stopped before setup: what it would have done
		return connectPlanText(r)
	}
	var b strings.Builder
	b.WriteString(setupText(*r.Setup))
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning  %s\n", w)
	}
	for _, n := range r.NextSteps {
		b.WriteString("- " + n + "\n")
	}
	return b.String()
}
