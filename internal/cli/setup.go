package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/guide"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

const setupResume = "backsight setup --resume"

type setupFlags struct {
	answers                      []string
	answersFile                  string
	nonInteractive, plan, resume bool
	questions, undo, force       bool
	reconfigure                  bool
	configDir                    string
}

// subResult is a backsight command's envelope.
type subResult struct {
	Data json.RawMessage
	Err  *output.Error
}

// subcommand runs a backsight command in-process with --json: each setup
// step is a command the user or an agent can also run alone. Tests
// replace it.
var subcommand func(ctx context.Context, args ...string) subResult

func init() { subcommand = runSubcommand } // a var initialiser would loop through newRoot

func runSubcommand(ctx context.Context, args ...string) subResult {
	var out, errOut bytes.Buffer
	code := ExecuteContext(ctx, append(append([]string{}, args...), "--json"), &out, &errOut)
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *output.Error   `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		return subResult{Err: &output.Error{Code: output.Blocked, Message: fmt.Sprintf("backsight %s: unreadable output (exit %d): %s", strings.Join(args, " "), code, strings.TrimSpace(errOut.String()))}}
	}
	if code != output.OK && env.Error == nil {
		env.Error = &output.Error{Code: code, Message: "backsight " + strings.Join(args, " ") + " failed"}
	}
	return subResult{Data: env.Data, Err: env.Error}
}

type setupResult struct {
	Run             string            `json:"run,omitempty"`
	Plan            bool              `json:"plan"`
	Questions       []setup.Question  `json:"questions,omitempty"`
	Answers         map[string]string `json:"answers,omitempty"`
	Preflight       []setup.Check     `json:"preflight,omitempty"`
	Steps           []setup.Step      `json:"steps"`
	Declined        bool              `json:"declined,omitempty"`
	RestartRequired bool              `json:"restart_required"`
	NextSteps       []string          `json:"next_steps,omitempty"`
	TryFirst        string            `json:"try_first,omitempty"`
	listing         bool              // --questions
}

func newSetupCmd(e *env) *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up backsight end to end: server, bank, Claude Code wiring, jobs (journaled, resumable)",
		Long: "Asks its questions at a terminal; agents list them with --questions --json and answer with --answer id=value. " +
			"Secrets never travel in answers: missing ones end with exit 10 and the backsight secret set command to run.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.undo {
				if len(f.answers) > 0 || f.answersFile != "" || f.resume || f.questions {
					return &output.Error{Code: output.Usage, Message: "--undo takes only --plan and --force"}
				}
				res, err := runSetupUndo(cmd.Context(), f, f.force)
				return e.report("setup", res, func() string { return undoText(res) }, err)
			}
			if f.force {
				return &output.Error{Code: output.Usage, Message: "--force goes with --undo"}
			}
			res, err := runSetup(cmd.Context(), e, f)
			return e.report("setup", res, func() string { return setupText(res) }, err)
		},
	}
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "answer a question: id=value (repeatable)")
	cmd.Flags().StringVar(&f.answersFile, "answers-file", "", "answers as a JSON object or id=value lines (no secrets)")
	cmd.Flags().BoolVar(&f.nonInteractive, "non-interactive", false, "never prompt: take defaults, exit 10 when a required answer or secret is missing")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "show the checks, answers and steps without changing anything")
	cmd.Flags().BoolVar(&f.resume, "resume", false, "continue the last run with its saved answers")
	cmd.Flags().BoolVar(&f.questions, "questions", false, "list the questions and exit")
	cmd.Flags().BoolVar(&f.reconfigure, "reconfigure", false, "at a terminal, ask every question again, the last run's answers as defaults")
	cmd.Flags().BoolVar(&f.undo, "undo", false, "reverse what setup changed (config, secrets and data are kept)")
	cmd.Flags().BoolVar(&f.force, "force", false, "with --undo: put the backup copies back over files edited since")
	cmd.Flags().StringVar(&f.configDir, "config-dir", "", "Claude Code's directory (default: $CLAUDE_CONFIG_DIR, else ~/.claude)")
	return cmd
}

// secretNames may never be answers (SPEC §11.2).
var secretNames = []string{"llm_api_key", "api_key", "ui_key", "db_password", "backup_identity"}

func parseAnswers(list []string, file string) (map[string]string, error) {
	out := map[string]string{}
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, &output.Error{Code: output.Usage, Message: err.Error()}
		}
		if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '{' {
			var m map[string]any
			if err := json.Unmarshal(t, &m); err != nil {
				return nil, &output.Error{Code: output.Usage, Message: file + ": " + err.Error()}
			}
			for k, v := range m {
				out[k] = strings.TrimSpace(fmt.Sprint(v))
			}
		} else {
			for _, line := range strings.Split(string(raw), "\n") {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					list = append([]string{line}, list...) // flags win over the file
				}
			}
		}
	}
	for _, a := range list {
		k, v, ok := strings.Cut(a, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, &output.Error{Code: output.Usage, Message: fmt.Sprintf("answer %q: want id=value", a)}
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	for _, s := range secretNames {
		if _, ok := out[s]; ok {
			return nil, &output.Error{Code: output.Usage, Message: s + " is a secret and never travels in answers: run backsight secret set " + s}
		}
	}
	return out, nil
}

// setupEnv is what setup reads before it changes anything.
type setupEnv struct {
	dirs  paths.Dirs
	home  string
	cfg   *config.Config // nil: no config.toml yet
	loc   claudecode.Location
	saved *setup.Record
	refs  int // reference changes a retire would make (§11.5)
	// ownConfig: the hash of the config.toml setup wrote, unedited since.
	ownConfig string
	f         setupFlags
	e         *env
	answer    map[string]string
	// cur is the step running: its share of the progress and the time
	// left, for the heartbeat of a long step.
	cur stepSpan
}

// stepSpan is where a step sits in the run's progress.
type stepSpan struct {
	name      string
	pct, span int // the step starts at pct and owns span points
	eta, left int // seconds: the step's own, and the run's from its start
	start     time.Time
}

func runSetup(ctx context.Context, e *env, f setupFlags) (setupResult, error) {
	res := setupResult{Plan: f.plan, Steps: []setup.Step{}}
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	s := setupEnv{dirs: d, home: home, f: f, e: e}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return res, &output.Error{Code: output.Usage, Message: err.Error()}
	default:
		s.cfg = &cfg
	}
	if s.saved, err = setup.Load(d.SetupFile()); err != nil {
		return res, blocked(err)
	}
	// A config.toml setup wrote and nobody edited since is setup's own:
	// its answers stay open to change (an agent switching provider after
	// the secrets exit must not need a hand edit).
	// Once a server runs from it, it is adopted: a new project or port would
	// orphan that server.
	if s.cfg != nil && s.saved != nil && s.saved.ConfigSHA != "" && !s.saved.Done("server") {
		if sha, err := fsutil.HashFile(d.ConfigFile()); err == nil && sha == s.saved.ConfigSHA {
			s.cfg, s.ownConfig = nil, sha
		}
	}
	if f.resume && s.saved == nil {
		return res, &output.Error{Code: output.Usage, Message: "no setup run to resume: run backsight setup"}
	}
	if s.answer, err = parseAnswers(f.answers, f.answersFile); err != nil {
		return res, err
	}
	getenv := os.Getenv
	if f.configDir != "" {
		getenv = func(k string) string {
			if k == "CLAUDE_CONFIG_DIR" {
				return f.configDir
			}
			return os.Getenv(k)
		}
	}
	s.loc = claudecode.Locate(home, getenv)
	s.refs = referenceCount(ctx, s.loc)

	interactive := !e.json && !f.nonInteractive && !f.plan && !f.questions && stdinIsTerminal() && stdoutIsTerminal()
	if f.reconfigure {
		switch {
		case f.resume:
			return res, &output.Error{Code: output.Usage, Message: "--reconfigure asks the questions again, --resume keeps the last run's answers: use one"}
		case !interactive:
			return res, &output.Error{Code: output.Usage, Message: "--reconfigure asks at a terminal; without one, change an answer with --answer id=value"}
		}
	}
	qs, extra, err := s.resolve(interactive)
	if err != nil {
		return res, err
	}
	res.Questions = qs
	if f.reconfigure {
		var fixed []string
		for _, q := range qs {
			if q.Source == "config" {
				fixed = append(fixed, q.ID+"="+q.Value)
			}
		}
		if len(fixed) > 0 { // the installed server runs from them
			fmt.Fprintf(e.stdout, "\nkept from %s: %s (edit that file to change them)\n", d.ConfigFile(), strings.Join(fixed, ", "))
		}
	}
	if f.questions {
		res.Answers, res.listing = extra, true
		return res, nil
	}
	missing := setup.Defaults(qs)
	if interactive { // consent is asked after the plan is shown
		missing = slices.DeleteFunc(missing, func(id string) bool { return id == "consent" })
	}
	res.Answers = setup.Values(qs)
	for k, v := range extra {
		res.Answers[k] = v
	}

	probes := preflightProbes(ctx, s, extra)
	probes.SchedulerOff = extra["scheduler"] == "off"
	if schedulerGOOS == "linux" && !s.schedulerSettled(extra) {
		probes.UserScheduler = userSession
	}
	res.Preflight = setup.Preflight(ctx, probes)
	for _, c := range res.Preflight {
		if c.ID == "scheduler" && c.Action != nil {
			c.Action.Cmd = s.rerunWith("scheduler=off")
		}
	}
	class, failed := setup.Verdict(res.Preflight)
	if f.plan {
		res.Steps = s.planSteps(res.Answers)
		return res, nil
	}
	if class != "" {
		return res, preflightError(class, failed)
	}
	if len(missing) > 0 {
		ne := &output.Error{Code: output.NeedsUser, Questions: qs, Resume: "backsight setup --answer id=value …",
			Message: "setup needs answers to " + strings.Join(missing, ", ") + ": list the questions with backsight setup --questions --json, then rerun with --answer id=value"}
		if !e.json && !f.nonInteractive && (!stdinIsTerminal() || !stdoutIsTerminal()) {
			ne.Message = "setup asks its questions at a terminal and there is no terminal here. " +
				"A coding agent installing for a user: read backsight guide agent-guide, then run backsight setup --questions --json and rerun with --answer id=value --non-interactive --json. " +
				"Missing answers: " + strings.Join(missing, ", ")
		}
		if !slices.Contains(missing, "provider") {
			if names, _, _ := s.missingSecrets(res.Answers); len(names) > 0 {
				ns := needSecrets(names, "backsight setup --answer id=value …")
				var se *output.Error
				if errors.As(ns, &se) {
					ne.Secrets, ne.Actions = se.Secrets, se.Actions
				}
			}
		}
		return res, ne
	}
	if interactive && res.Answers["consent"] == "" {
		fmt.Fprint(e.stdout, setupPlanText(s.planSteps(res.Answers)))
		fmt.Fprint(e.stdout, reuseHint(qs))
		if !confirm(e, "Go ahead with this plan? [y/N] ") {
			res.Answers["consent"] = "no"
		} else {
			res.Answers["consent"] = "yes"
		}
	}
	if res.Answers["consent"] != "yes" {
		res.Declined = true
		return res, nil
	}

	unlock, err := setupLock(d, "setup")
	if err != nil {
		return res, err
	}
	defer unlock()
	// Read again under the lock: an undo may have finished meanwhile.
	if s.saved, err = setup.Load(d.SetupFile()); err != nil {
		return res, blocked(err)
	}
	return s.execute(ctx, res, interactive)
}

// userSession reaches systemd's user manager, as the scheduler step's
// systemctl --user calls will.
func userSession(ctx context.Context) error {
	out, err := sysRunner.Run(ctx, "systemctl", "--user", "show-environment")
	if err == nil {
		return nil
	}
	if msg, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); msg != "" {
		return errors.New(msg)
	}
	return err
}

// schedulerSettled: a resume whose scheduler step finished with the same
// answer, so that it does not run again.
func (s *setupEnv) schedulerSettled(extra map[string]string) bool {
	return s.f.resume && s.saved != nil && s.saved.Done("scheduler") && s.saved.Answers["scheduler"] == extra["scheduler"]
}

// rerunWith is the command this run was, with one more answer.
func (s *setupEnv) rerunWith(answer string) string {
	key, _, _ := strings.Cut(answer, "=")
	parts := []string{"backsight", "setup"}
	if s.f.resume {
		parts = append(parts, "--resume")
	}
	for _, a := range s.f.answers {
		if k, _, _ := strings.Cut(a, "="); strings.TrimSpace(k) != key {
			parts = append(parts, "--answer", shellWord(a))
		}
	}
	if s.f.answersFile != "" {
		parts = append(parts, "--answers-file", shellWord(s.f.answersFile))
	}
	if s.f.configDir != "" {
		parts = append(parts, "--config-dir", shellWord(s.f.configDir))
	}
	if s.f.nonInteractive {
		parts = append(parts, "--non-interactive")
	}
	if s.e.json {
		parts = append(parts, "--json")
	}
	return strings.Join(append(parts, "--answer", answer), " ")
}

// resolve builds the questions; at a terminal it asks each open one in
// turn (an answer can change the next questions: provider → model).
func (s *setupEnv) resolve(interactive bool) ([]setup.Question, map[string]string, error) {
	// Earlier answers are the defaults of any rerun; the go itself is
	// reused only by a --resume that changes nothing.
	saved := map[string]string{}
	if s.saved != nil {
		for k, v := range s.saved.Answers {
			if k != "consent" || (s.f.resume && doesLess(s.saved.Answers, s.answer)) {
				saved[k] = v
			}
		}
	}
	in := setup.Inputs{Config: s.cfg, Answers: s.answer, Saved: saved, Backfill: s.backfillEstimate, ReferencePieces: s.refs}
	// --reconfigure: the questions the last run answered are asked again,
	// their answers the defaults; the hidden answers (ports, project) stay.
	prev := map[string]string{}
	if s.f.reconfigure {
		prev = maps.Clone(saved)
		in.Saved = maps.Clone(saved)
		for again := true; again; {
			qs, _, err := setup.Resolve(in)
			if err != nil {
				break
			}
			again = false
			for _, q := range qs {
				if q.Source == "saved" {
					delete(in.Saved, q.ID)
					again = true
				}
			}
		}
		saved = in.Saved
	}
	for {
		qs, extra, err := setup.Resolve(in)
		var ue *setup.UsageError
		if errors.As(err, &ue) {
			return nil, nil, &output.Error{Code: output.Usage, Message: ue.Msg}
		}
		if err != nil || !interactive {
			return qs, extra, err
		}
		var next *setup.Question
		for i := range qs {
			if qs[i].Asked() && qs[i].ID != "consent" {
				next = &qs[i]
				break
			}
		}
		if next == nil {
			return qs, extra, nil
		}
		q := *next
		if v := prev[q.ID]; v != "" && (q.Free || slices.ContainsFunc(q.Options, func(o setup.Option) bool { return o.Value == v })) {
			// The last run's answer, unless it belonged to another provider.
			if !slices.Contains([]string{"model", "base_url", "ack_subscription"}, q.ID) || in.Answers["provider"] == prev["provider"] {
				q.Default = v
			}
		}
		v, err := ask(s.e, q)
		if err != nil {
			return nil, nil, err
		}
		trial := map[string]string{}
		for k, val := range in.Answers {
			trial[k] = val
		}
		trial[next.ID] = v
		if _, _, err := setup.Resolve(setup.Inputs{Config: s.cfg, Answers: trial, Saved: saved, ReferencePieces: s.refs}); errors.As(err, &ue) {
			fmt.Fprintln(s.e.stdout, "  "+ue.Msg)
			continue
		}
		in.Answers = trial
	}
}

// ask prompts one question at a terminal; Enter takes the default.
func ask(e *env, q setup.Question) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", q.Prompt)
	for i, o := range q.Options {
		fmt.Fprintf(&b, "  %d) %s — %s", i+1, o.Value, o.Label)
		if o.Value == q.Recommended {
			b.WriteString(" [recommended]")
		}
		if o.Cost != "" {
			b.WriteString(" · " + o.Cost)
		}
		if o.Caveat != "" {
			b.WriteString("\n     ⚠ " + o.Caveat)
		}
		b.WriteString("\n")
	}
	if q.Free {
		b.WriteString("  (or type a value)\n")
	}
	prompt := "choice"
	if q.Default != "" {
		prompt += " [" + q.Default + "]"
	}
	fmt.Fprint(e.stdout, b.String()+prompt+": ")
	line, err := readLine()
	if err != nil {
		return "", &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	for line == "" && q.Default == "" && q.ID != "base_url" { // no default: an answer is needed
		fmt.Fprint(e.stdout, prompt+": ")
		if line, err = readLine(); err != nil {
			return "", &output.Error{Code: output.Blocked, Message: err.Error()}
		}
	}
	switch n, convErr := strconv.Atoi(line); {
	case line == "":
		return q.Default, nil
	case convErr == nil && n >= 1 && n <= len(q.Options):
		return q.Options[n-1].Value, nil
	}
	return line, nil
}

func confirm(e *env, prompt string) bool {
	fmt.Fprint(e.stdout, prompt)
	line, _ := readLine()
	return strings.EqualFold(line, "y") || strings.EqualFold(line, "yes")
}

// stdinLines reads prompt answers; one reader per stdin, so buffered
// input is not lost between prompts.
var (
	stdinLines *bufio.Reader
	stdinSrc   io.Reader
)

func readLine() (string, error) {
	if stdinLines == nil || stdinSrc != stdin {
		stdinLines, stdinSrc = bufio.NewReader(stdin), stdin
	}
	line, err := stdinLines.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && line == "" {
		return "", errors.New("no answer (end of input)")
	}
	return strings.TrimSpace(line), nil
}

// estimateFlusher is the survey-only flusher of the backfill estimate. It
// knows the transverse aliases, so the notes it counts are the ones a real
// replay would send (a note naming the pool goes to the canonical alias).
func estimateFlusher(d paths.Dirs, home string, cfg config.Config, since time.Time, idx *retain.SentIndex) *retain.Flusher {
	return &retain.Flusher{Bank: cfg.Server.Bank, Dirs: d, Resolver: project.New(home, cfg.Projects.Transverse),
		Exclude: cfg.Privacy.ExcludePaths, Headless: cfg.Privacy.SkipsEntrypoint, Now: now, RetainSince: since, Index: idx, Outbox: notesOutboxFor(d, cfg),
		Attribution: retain.Attributor{Transverse: cfg.Projects.Transverse}}
}

// backfillEstimate counts the sessions backfill would send and prices
// them for model; it reads the transcripts only, never the server.
func (s *setupEnv) backfillEstimate(model string) (int, string) {
	cfg := config.Default(s.dirs, s.home)
	if s.cfg != nil {
		cfg = *s.cfg
	}
	// A survey only: no client, no machine id written before the go.
	idx, err := retain.LoadSentIndex(s.dirs.SentIndexDir())
	if err != nil {
		return 0, ""
	}
	since := now().UTC()
	if st, err := state.Load(s.dirs.StateFile()); err == nil && st.RetainSince != nil {
		since = *st.RetainSince
	}
	fl := estimateFlusher(s.dirs, s.home, cfg, since, idx)
	prices := retain.PricesFor(model, cfg.LLM.PriceInput, cfg.LLM.PriceCached, cfg.LLM.PriceOutput, cfg.LLM.Currency)
	if model != cfg.LLM.Model {
		prices = retain.PricesFor(model, 0, 0, 0, cfg.LLM.Currency)
	}
	files, _ := filepath.Glob(filepath.Join(s.loc.Projects, "*", "*.jsonl"))
	_, plan, err := surveyAll(fl, files, backfillFlags{}, prices)
	if err != nil {
		return 0, ""
	}
	n := plan.Send + plan.NotesOnly
	about := plan.Estimate.Duration.Round(time.Minute)
	if prices.Known() {
		return n, fmt.Sprintf("≈ %.2f %s, capped at %.2f %s, about %s", plan.Estimate.Cost, prices.Currency, plan.MaxCost, prices.Currency, about)
	}
	return n, fmt.Sprintf("≈ %dk tokens (no price known for %s), capped at %dk, about %s", (plan.Estimate.InputTokens+plan.Estimate.OutputTokens)/1000, model, plan.MaxTokens/1000, about)
}

// preflightProbes reads this machine; tests replace it.
var preflightProbes = func(ctx context.Context, s setupEnv, extra map[string]string) setup.Probes {
	api, _ := strconv.Atoi(extra["api_port"])
	ui, _ := strconv.Atoi(extra["ui_port"])
	p := setup.Probes{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, APIPort: api, UIPort: ui, MinClaude: minClaude, AtLeast: atLeast,
		Remote: s.cfg != nil && s.cfg.Server.Mode == "remote", GPUs: doctor.LocalGPUs()}
	var cfg config.Config // the runtime's engine needs no config
	if s.cfg != nil {
		cfg = *s.cfg
	}
	rt := runtimeFor(cfg)
	p.Docker = func(ctx context.Context) (string, error) {
		e, err := rt.Engine(ctx)
		if e.Rootless {
			e.Version += " rootless"
		}
		return e.Version, err
	}
	p.Compose = rt.Orchestrator
	p.PortFree = func(port int) bool {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return false
		}
		_ = ln.Close()
		return true
	}
	// The server the config on disk describes: adopted, or written by an
	// earlier run of setup and still open to new answers (s.cfg is nil
	// then), whose server may run though a later check failed (a rejected
	// LLM key) and must not count as another program on its ports.
	srv := s.cfg
	if srv == nil && s.ownConfig != "" {
		if c, err := config.Load(s.dirs.ConfigFile(), s.dirs, s.home); err == nil {
			srv = &c
		}
	}
	if srv != nil && srv.Server.Mode == "local" {
		own := runtimeFor(*srv)
		// Ours only when our compose project runs a hindsight container:
		// health alone would take the reference server on the same port
		// for ours, and miss our own while it starts or fails. Another
		// project answered would start a second server beside it.
		if (extra["project"] == "" || extra["project"] == srv.Server.Project) && serverRuns(ctx, own) {
			if api, err := srv.APIPort(); err == nil {
				p.OwnPorts = append(p.OwnPorts, api)
			}
			p.OwnPorts = append(p.OwnPorts, srv.Server.UIPort)
		}
		v, err := own.Deployed()
		p.ServerInstalled = err == nil && v != ""
	}
	p.FreeBytes = func() (uint64, error) {
		dir := s.dirs.Data
		for {
			if _, err := os.Stat(dir); err == nil || filepath.Dir(dir) == dir {
				return doctor.FreeBytes(dir)
			}
			dir = filepath.Dir(dir)
		}
	}
	var env []string
	if s.f.configDir != "" {
		env = []string{"CLAUDE_CONFIG_DIR=" + s.f.configDir}
	}
	p.Claude = func(ctx context.Context) (string, error) {
		out, err := claudeRunner.Run(ctx, env, "--version")
		if err != nil {
			return "", err
		}
		return strings.Fields(string(out) + " ?")[0], nil
	}
	p.Reference = s.refs
	var de doctor.Env
	probes(&de)
	if r, err := claudecode.ReadRetention(s.loc, de.ManagedDir); err == nil {
		p.Retention = fmt.Sprintf("Claude Code keeps transcripts %d days (%s)", r.EffectiveDays, r.Source)
	}
	if t, err := claudecode.ScanTranscripts(s.loc.Projects); err == nil && t.Count > 0 {
		p.Transcript = fmt.Sprintf("%d transcripts, %.1f MB, oldest %s", t.Count, float64(t.Bytes)/(1<<20), t.Oldest.UTC().Format("2006-01-02"))
	}
	return p
}

// serverRuns reports whether backsight's server has its hindsight service
// up (running, starting or restarting).
func serverRuns(ctx context.Context, rt server.Runtime) bool {
	list, err := rt.Status(ctx)
	if err != nil {
		return false
	}
	for _, c := range list {
		if c.Service == "hindsight" && (c.State == "running" || c.State == "restarting" || c.State == "created") {
			return true
		}
	}
	return false
}

// serverAnswers reports whether cfg's server answers its health check;
// tests replace it.
var serverAnswers = func(ctx context.Context, cfg config.Config) bool {
	cl, err := hindsight.New(cfg.Server.URL, "", 2*time.Second)
	if err != nil {
		return false
	}
	_, err = cl.Health(ctx)
	return err == nil
}

func preflightError(class string, failed []setup.Check) error {
	e := &output.Error{Code: output.Blocked, Resume: "backsight setup"}
	var parts []string
	for _, c := range failed {
		if c.ID == "scheduler" && c.Action != nil { // the same command with the answer
			e.Resume = c.Action.Cmd
		}
		parts = append(parts, c.Summary+" ("+c.Fix+")")
		if c.Action != nil {
			e.Actions = append(e.Actions, *c.Action)
		}
	}
	if class == "needs_user" {
		e.Code = output.NeedsUser
	}
	e.Message = "pre-flight: " + strings.Join(parts, "; ")
	return e
}

// setupStep is one step of the run.
type setupStep struct {
	name string
	eta  int // seconds, for progress lines
	run  func(ctx context.Context, r *setup.Record) (setup.Step, error)
}

func (s *setupEnv) steps(answers map[string]string, interactive bool, restart *bool) []setupStep {
	clientArgs := func(args ...string) []string {
		if s.f.configDir != "" {
			args = append(args, "--config-dir", s.f.configDir)
		}
		return args
	}
	// withRuns records the config-editor runs a step made, for undo.
	withRuns := func(r *setup.Record, fn func() subResult) subResult {
		before, _ := claudecfg.Runs(s.dirs.PreImagesDir())
		out := fn()
		after, _ := claudecfg.Runs(s.dirs.PreImagesDir())
		for _, ts := range newRuns(before, after) {
			_ = claudecfg.Pin(s.dirs.PreImagesDir(), ts) // pruning must not take what undo needs
			r.Undo.AddRuns(ts)
		}
		return out
	}
	serverETA := 30
	if s.cfg == nil {
		serverETA = 300 // the first pull downloads the image
	}
	return []setupStep{
		// Secrets first: nothing is written while a key is missing, so the
		// answers stay free to change.
		{"secrets", 1, func(context.Context, *setup.Record) (setup.Step, error) {
			missing, cfg, err := s.missingSecrets(answers)
			if err != nil {
				return setup.Step{}, err
			}
			if len(missing) == 0 {
				return setup.Step{Name: "secrets", Status: "skipped", Detail: "present"}, nil
			}
			if !interactive {
				var e *output.Error
				_ = errors.As(needSecrets(missing, setupResume), &e)
				if cfg.LLM.Provider == "claude-code" {
					e.Actions = append([]output.Action{{Kind: "run_as_user", Cmd: "claude setup-token", Why: "the subscription lane uses a one-year token from Claude Code"}}, e.Actions...)
				}
				return setup.Step{}, e
			}
			for _, n := range missing {
				v, err := readSecret(s.e, n, "", "")
				if err != nil {
					return setup.Step{}, err
				}
				if v == "" {
					return setup.Step{}, needSecrets([]string{n}, setupResume)
				}
				if err := config.UpdateSecrets(s.dirs.SecretsFile(), func(sec *config.Secrets) error { return sec.Set(n, v) }); err != nil {
					return setup.Step{}, blocked(err)
				}
			}
			return setup.Step{Name: "secrets", Status: "done", Detail: "saved " + strings.Join(missing, ", ")}, nil
		}},
		{"config", 1, func(_ context.Context, r *setup.Record) (setup.Step, error) {
			if s.cfg != nil {
				return setup.Step{Name: "config", Status: "skipped", Detail: "adopted " + s.dirs.ConfigFile()}, nil
			}
			if err := os.MkdirAll(s.dirs.Config, 0o700); err != nil {
				return setup.Step{}, blocked(err)
			}
			want := fsutil.Absent
			if s.ownConfig != "" { // setup's own, unedited: rewritten from the answers
				want = s.ownConfig
			}
			raw := setupConfig(answers)
			if err := fsutil.ReplaceIfUnchanged(s.dirs.ConfigFile(), raw, 0o600, want); err != nil {
				return setup.Step{}, blocked(fmt.Errorf("%s changed while setup wrote it: %w", s.dirs.ConfigFile(), err))
			}
			if _, err := config.Load(s.dirs.ConfigFile(), s.dirs, s.home); err != nil {
				_ = os.Remove(s.dirs.ConfigFile())
				return setup.Step{}, &output.Error{Code: output.Usage, Message: err.Error()}
			}
			r.Undo.CreatedConfig = true
			r.ConfigSHA, _ = fsutil.HashFile(s.dirs.ConfigFile())
			return setup.Step{Name: "config", Status: "done", Detail: "written to " + s.dirs.ConfigFile()}, nil
		}},
		{"server", serverETA, func(ctx context.Context, r *setup.Record) (setup.Step, error) {
			cfg, err := config.Load(s.dirs.ConfigFile(), s.dirs, s.home)
			if err != nil {
				return setup.Step{}, &output.Error{Code: output.Usage, Message: err.Error()}
			}
			if cfg.Server.Mode != "local" {
				return setup.Step{Name: "server", Status: "skipped", Detail: "remote server " + cfg.Server.URL + ": install it on its host"}, nil
			}
			// Another version deployed: server install would refuse it, and
			// undo must not stop a server this run did not start.
			if err := pinGuard(cfg, runtimeFor(cfg)); err != nil {
				return setup.Step{}, err
			}
			// Intent first: a failed or interrupted install may still have
			// started containers, and stopping a stopped project is harmless.
			if !serverAnswers(ctx, cfg) {
				r.Undo.ServerStarted = true
				s.persist(r)
			}
			models, err := s.download(ctx, runtimeFor(cfg))
			if err != nil {
				return setup.Step{}, err
			}
			starting := "starting the server"
			if models {
				starting = fmt.Sprintf("starting the server; its first start downloads the embedding models (about %.1f GB)", float64(server.ModelsMB)/1000)
			}
			stop := s.heartbeat(ctx, starting, true)
			out := subcommand(ctx, "server", "install")
			stop()
			if out.Err != nil {
				return setup.Step{}, out.Err
			}
			var ir installResult
			_ = json.Unmarshal(out.Data, &ir)
			return setup.Step{Name: "server", Status: "done", Detail: fmt.Sprintf("Hindsight %s at %s (UI %s)", ir.Version, ir.URL, ir.UI)}, nil
		}},
		// After the bank step (server install's): the docs bank for guide
		// search (decision 0069). Its failure never stops setup: guide
		// search answers with its keyword search, and indexes them on first
		// use unless the bank is not backsight's.
		{"docs", 5, func(ctx context.Context, _ *setup.Record) (setup.Step, error) {
			out := subcommand(ctx, "guide", "index")
			if out.Err != nil {
				return setup.Step{Name: "docs", Status: "skipped", Detail: "not indexed (" + out.Err.Message + "): backsight guide search answers with its keyword search meanwhile"}, nil
			}
			var ir guide.IndexResult
			_ = json.Unmarshal(out.Data, &ir)
			return setup.Step{Name: "docs", Status: "done", Detail: fmt.Sprintf("%d documentation section(s) in bank %s for backsight guide search (%d sent)", ir.Sections, ir.Bank, ir.Sent)}, nil
		}},
		{"client", 20, func(ctx context.Context, r *setup.Record) (setup.Step, error) {
			// Undo removes the plugin only from a profile it was not in before.
			if known, err := pluginIDs(ctx, profileEnv(s.home, s.loc.Dir)); err != nil || !known[plugin.PluginID()] {
				r.Undo.ClientProfiles = appendNew(r.Undo.ClientProfiles, filepath.Clean(s.loc.Dir))
				s.persist(r)
			}
			out := withRuns(r, func() subResult { return subcommand(ctx, clientArgs("client", "install")...) })
			if out.Err != nil {
				return setup.Step{}, out.Err
			}
			var cr clientInstallResult
			_ = json.Unmarshal(out.Data, &cr)
			for _, st := range cr.Steps {
				if st.Name == "plugin" && st.Status == "done" {
					*restart = true
				}
			}
			return setup.Step{Name: "client", Status: "done", Detail: "plugin and permission rule in " + cr.Profile}, nil
		}},
		{"retention", 1, func(ctx context.Context, r *setup.Record) (setup.Step, error) {
			args := clientArgs("client", "retention")
			switch v := answers["retention"]; v {
			case "keep":
				args = append(args, "--keep")
			case setup.RecommendedRetention: // a floor: a longer setting stays
			default:
				args = append(args, "--days", v)
			}
			out := withRuns(r, func() subResult { return subcommand(ctx, args...) })
			if out.Err != nil {
				return setup.Step{}, out.Err
			}
			var rr retentionResult
			_ = json.Unmarshal(out.Data, &rr)
			// What the run wrote: undo puts the old value back only over it.
			for _, fr := range rr.Files {
				for _, ch := range fr.Changes {
					if rr.Run != "" && ch.Path == "cleanupPeriodDays" && ch.New != nil {
						if r.Undo.Retention == nil {
							r.Undo.Retention = map[string]string{}
						}
						r.Undo.Retention[rr.Run] = *ch.New
					}
				}
			}
			detail := "Claude Code's setting left as it is"
			if rr.Target > 0 {
				detail = fmt.Sprintf("transcripts kept at least %d days", rr.Target)
			}
			return setup.Step{Name: "retention", Status: "done", Detail: detail}, nil
		}},
		{"scheduler", 2, func(ctx context.Context, r *setup.Record) (setup.Step, error) {
			// The key the daily backups encrypt to, with the jobs that make
			// them (and only once the server runs: an earlier failure stays
			// one that changed nothing).
			identity, err := s.backupIdentity(answers)
			if err != nil {
				return setup.Step{}, err
			}
			if answers["scheduler"] == "off" { // a container or CI runner without a user scheduler
				st := setup.Step{Name: "scheduler", Status: "skipped", Detail: "off as answered: run backsight jobs tick every 15 min and jobs maintain daily some other way" + identity}
				if identity != "" {
					st.Status = "done"
				}
				if r.Undo.Scheduler { // entries an earlier run wrote (its activation may have failed)
					if rec, err := state.Load(s.dirs.StateFile()); err == nil && rec.Scheduler != nil {
						if out := subcommand(ctx, "jobs", "uninstall"); out.Err != nil {
							return setup.Step{}, out.Err
						}
						st.Status, st.Detail = "done", "off as answered: removed the entries an earlier run wrote; run backsight jobs tick every 15 min and jobs maintain daily some other way"+identity
					}
					r.Undo.Scheduler = false
				}
				return st, nil
			}
			if st, err := state.Load(s.dirs.StateFile()); err == nil && st.Scheduler == nil {
				r.Undo.Scheduler = true
				s.persist(r)
			}
			// Not recorded as config runs: jobs uninstall also stops the timers.
			out := subcommand(ctx, "jobs", "install")
			if out.Err != nil {
				return setup.Step{}, out.Err
			}
			var jr jobsInstallResult
			_ = json.Unmarshal(out.Data, &jr)
			return setup.Step{Name: "scheduler", Status: "done", Detail: jr.Kind + ": jobs tick every 15 min, jobs maintain daily" + identity}, nil
		}},
		{"verify", 20, func(ctx context.Context, _ *setup.Record) (setup.Step, error) {
			out := subcommand(ctx, clientArgs("verify")...)
			var vr verifyResult
			_ = json.Unmarshal(out.Data, &vr)
			if out.Err != nil {
				var failed []string
				for _, c := range vr.Checks {
					if c.Status == "fail" {
						failed = append(failed, c.ID+": "+c.Summary)
					}
				}
				if len(failed) > 0 {
					e := *out.Err
					e.Message = strings.Join(failed, "; ")
					return setup.Step{}, &e
				}
				return setup.Step{}, out.Err
			}
			return setup.Step{Name: "verify", Status: "done", Detail: fmt.Sprintf("%d checks passed: every hook, the MCP server, a retain/recall round trip", len(vr.Checks))}, nil
		}},
		{"reference", 2, func(ctx context.Context, r *setup.Record) (setup.Step, error) {
			if answers["reference"] != "retire" {
				return setup.Step{Name: "reference", Status: "skipped", Detail: map[bool]string{true: "kept, as answered", false: "no older Hindsight install wired"}[answers["reference"] == "keep"]}, nil
			}
			// After verify: the old memory is unwired only once backsight works.
			out := withRuns(r, func() subResult { return subcommand(ctx, clientArgs("client", "retire")...) })
			var rr retireResult
			_ = json.Unmarshal(out.Data, &rr)
			r.Undo.Timers = appendNew(r.Undo.Timers, rr.Timers...)
			if out.Err != nil {
				return setup.Step{}, out.Err
			}
			n := len(rr.Timers)
			for _, f := range rr.Files {
				n += len(f.Changes)
			}
			*restart = *restart || n > 0
			return setup.Step{Name: "reference", Status: "done", Detail: fmt.Sprintf("%d piece(s) of the older Hindsight install unwired; its server is untouched", n)}, nil
		}},
		{"backfill", 5, func(ctx context.Context, _ *setup.Record) (setup.Step, error) {
			if answers["backfill"] != "yes" {
				return setup.Step{Name: "backfill", Status: "skipped", Detail: "not asked for"}, nil
			}
			args := []string{"retain", "backfill", "--queue", "--projects-dir", s.loc.Projects}
			if answers["provider"] == "claude-code" {
				args = append(args, "--allow-subscription") // answered yes past the caveat
			}
			out := subcommand(ctx, args...)
			if out.Err != nil {
				return setup.Step{}, out.Err
			}
			var br backfillResult
			_ = json.Unmarshal(out.Data, &br)
			return setup.Step{Name: "backfill", Status: "done", Detail: fmt.Sprintf("%d session(s) queued: jobs tick sends them in the background", br.Total)}, nil
		}},
	}
}

// backupIdentity generates the backup identity when backups are on and
// no recipient is configured, and says so ("" when none was made).
func (s *setupEnv) backupIdentity(answers map[string]string) (string, error) {
	cfg, err := config.Load(s.dirs.ConfigFile(), s.dirs, s.home)
	if err != nil {
		return "", &output.Error{Code: output.Usage, Message: err.Error()}
	}
	if answers["backups"] != "on" || !cfg.Backup.Enabled || cfg.Backup.Recipient != "" {
		return "", nil
	}
	made, err := ensureBackupIdentity(s.dirs)
	if err != nil || !made {
		return "", err
	}
	return "; generated backup_identity, the key of your backups (save it: backsight backup identity show)", nil
}

// download fetches the server images the machine lacks before the server
// step installs it: at a terminal Docker's own progress shows, with --json
// the progress lines keep coming. It reports whether the server's first
// start still downloads its models. A runtime that cannot tell leaves it
// all to server install.
func (s *setupEnv) download(ctx context.Context, rt server.Runtime) (models bool, err error) {
	dl, ok := rt.(server.Downloader)
	if !ok {
		return false, nil
	}
	images, models, err := dl.Downloads(ctx)
	if err != nil || (len(images) == 0 && !models) {
		return false, nil // server install says what is wrong with Docker
	}
	what := "the embedding models at the server's first start"
	if len(images) > 0 {
		what = fmt.Sprintf("%d server image(s)", len(images))
		if models {
			what += ", then the embedding models at its first start"
		}
	}
	about := fmt.Sprintf("downloading %s: about %.1f GB, several minutes on a home connection", what, float64(server.DownloadMB(images, models))/1000)
	if !s.e.json {
		fmt.Fprintln(s.e.stdout, about)
	}
	if len(images) == 0 {
		return models, nil
	}
	w := io.Discard
	if !s.e.json {
		w = s.e.stdout
	}
	stop := s.heartbeat(ctx, about, false)
	err = dl.Download(ctx, images, w)
	stop()
	if err != nil {
		return models, &output.Error{Code: output.Blocked, Message: "the download stopped: " + err.Error() + " (check the network)"}
	}
	return models, nil
}

// missingSecrets lists the keys the configuration these answers give
// needs and secrets.toml lacks: the LLM key of a local server, the API key
// of a remote one (the server keys are generated by server install).
func (s *setupEnv) missingSecrets(answers map[string]string) ([]string, config.Config, error) {
	var cfg config.Config
	var err error
	if s.cfg != nil {
		cfg = *s.cfg
	} else if cfg, err = config.Parse(setupConfig(answers), s.dirs.ConfigFile(), s.dirs, s.home); err != nil {
		return nil, cfg, &output.Error{Code: output.Usage, Message: err.Error()}
	}
	sec, err := config.LoadSecrets(s.dirs.SecretsFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, cfg, blocked(err)
	}
	have := sec.Names()
	var missing []string
	for _, n := range []string{"llm_api_key", "api_key"} {
		if need := config.RequiredSecrets(cfg)[n] && (n == "llm_api_key" || cfg.Server.Mode == "remote"); need && !have[n] {
			missing = append(missing, n)
		}
	}
	return missing, cfg, nil
}

func appendNew(list []string, items ...string) []string {
	for _, it := range items {
		if !slices.Contains(list, it) {
			list = append(list, it)
		}
	}
	return list
}

// referenceCount is how many reference pieces a retire would unwire now
// (read-only: a plan and systemctl is-enabled).
func referenceCount(ctx context.Context, loc claudecode.Location) int {
	edits, err := retireEdits(loc)
	if err != nil {
		return 0
	}
	files, err := claudecfg.Plan(edits)
	if err != nil {
		return 0
	}
	n := 0
	for _, f := range files {
		n += len(f.Changes)
	}
	if retroTimerOwned(ctx, loc) {
		n++
	}
	return n
}

func newRuns(before, after []string) []string {
	seen := map[string]bool{}
	for _, r := range before {
		seen[r] = true
	}
	var out []string
	for _, r := range after {
		if !seen[r] {
			out = append(out, r)
		}
	}
	return out
}

// setupConfig is the config.toml setup writes from the answers.
func setupConfig(a map[string]string) []byte {
	api, _ := strconv.Atoi(a["api_port"])
	ui, _ := strconv.Atoi(a["ui_port"])
	raw := string(initialConfig(installFlags{apiPort: api, uiPort: ui, provider: a["provider"], model: a["model"], base: a["base_url"], project: a["project"]}))
	raw = strings.Replace(raw, "# Written by backsight server install.", "# Written by backsight setup.", 1)
	if a["backups"] == "off" {
		raw += "\n[backup]\nenabled = false\n"
	}
	return []byte(raw)
}

func (s *setupEnv) execute(ctx context.Context, res setupResult, interactive bool) (setupResult, error) {
	rec := &setup.Record{SchemaVersion: setup.RecordVersion, Run: now().UTC().Format("20060102T150405Z"), StartedAt: now().UTC(), Profile: filepath.Clean(s.loc.Dir)}
	answers := map[string]string{}
	for k, v := range res.Answers {
		answers[k] = v
	}
	if s.saved != nil {
		rec.Undo = s.saved.Undo // undo reverses every setup since the last undo
		rec.ConfigSHA = s.saved.ConfigSHA
		if s.f.resume && len(s.saved.Answers) == len(answers) && doesLess(s.saved.Answers, answers) {
			rec.Run, rec.StartedAt, rec.Steps = s.saved.Run, s.saved.StartedAt, s.saved.Steps
			if s.saved.Answers["scheduler"] != answers["scheduler"] { // its step runs again, in its place, to remove the entries
				rec.Steps = slices.Clone(rec.Steps)
				if i := slices.IndexFunc(rec.Steps, func(st setup.Step) bool { return st.Name == "scheduler" }); i >= 0 {
					rec.Steps[i] = setup.Step{Name: "scheduler", Status: "planned"}
				}
			}
		}
	}
	rec.Answers = answers
	res.Run = rec.Run
	restart := false
	steps := s.steps(answers, interactive, &restart)
	total := 0
	for _, st := range steps {
		total += st.eta
	}
	left, did := total, false
	for i, st := range steps {
		if rec.Done(st.name) && st.name != "verify" { // verify always reruns (decision 0064)
			left -= st.eta
			continue
		}
		s.cur = stepSpan{name: st.name, pct: i * 100 / len(steps), span: 100 / len(steps), eta: st.eta, left: left, start: time.Now()}
		s.progress(st.name, s.cur.pct, left, "")
		step, err := st.run(ctx, rec)
		left -= st.eta
		if err != nil {
			rec.Set(setup.Step{Name: st.name, Status: "failed", Detail: errMessage(err), At: now().UTC()})
			s.saveRecord(rec)
			res.Steps = rec.Steps
			failure := stepFailure(st.name, err, did || doneBefore(rec, st.name))
			if s.e.json {
				return res, failure
			}
			// Once: the step line says what failed and how to fix it, then
			// how to go on.
			fmt.Fprint(s.e.stdout, stepLine(step2line(rec.Steps[len(rec.Steps)-1])))
			var oe *output.Error
			if errors.As(failure, &oe) && oe.Resume != "" {
				fmt.Fprintf(s.e.stdout, "then run: %s\n", oe.Resume)
			}
			return res, shownError{oe}
		}
		step.At = now().UTC()
		did = did || step.Status == "done"
		rec.Set(step)
		s.saveRecord(rec)
		if !s.e.json {
			fmt.Fprint(s.e.stdout, stepLine(step2line(step)))
		}
	}
	s.progress("summary", 100, 0, "")
	rec.Finished = true
	if err := rec.Save(s.dirs.SetupFile()); err != nil {
		fmt.Fprintf(s.e.stderr, "backsight: setup.json not saved: %v\n", err)
	}
	res.Steps = rec.Steps
	res.RestartRequired = restart
	if restart {
		res.NextSteps = append(res.NextSteps, "restart Claude Code (or run /reload-plugins) so it loads the backsight plugin")
	}
	if answers["backfill"] == "yes" {
		res.NextSteps = append(res.NextSteps, "your past sessions reach the memory in the background: backsight doctor shows the backfill's progress")
	}
	if answers["backups"] == "on" {
		res.NextSteps = append(res.NextSteps, "save the backup key somewhere safe: backsight backup identity show (at a terminal)")
	}
	res.NextSteps = append(res.NextSteps, "to reverse it: backsight setup --undo (config, secrets and memories are kept)")
	res.TryFirst = "In a new Claude Code session, ask: \"What do you remember about this project?\""
	return res, nil
}

// doneBefore reports whether a step other than name changed something.
func doneBefore(r *setup.Record, name string) bool {
	for _, s := range r.Steps {
		if s.Name != name && s.Status == "done" {
			return true
		}
	}
	return false
}

func step2line(s setup.Step) step { return step{Name: s.Name, Status: s.Status, Detail: s.Detail} }

// doesLess reports whether a resume's answers keep the saved run's plan
// or only turn the scheduler off: its go and its finished steps stand.
// Any other change is a new plan, asked again (decision 0064).
func doesLess(saved, answers map[string]string) bool {
	for k, v := range answers {
		if saved[k] != v && (k != "scheduler" || v != "off") {
			return false
		}
	}
	return true
}

func errMessage(err error) string {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe.Message
	}
	return err.Error()
}

// stepFailure maps a failed step to setup's exit: needs_user stays (with
// setup's resume command), anything else is partial once a step changed
// something, else blocked.
func stepFailure(name string, err error, changed bool) error {
	var oe *output.Error
	if !errors.As(err, &oe) {
		oe = &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	out := *oe
	out.Message = name + ": " + oe.Message
	out.Resume = setupResume
	switch {
	case oe.Code == output.NeedsUser:
	case changed:
		out.Code = output.Partial
	case oe.Code != output.Usage:
		out.Code = output.Blocked
	}
	return &out
}

// persist saves the record without a journal line: an undo intent noted
// before the step that acts on it.
func (s *setupEnv) persist(r *setup.Record) {
	if err := r.Save(s.dirs.SetupFile()); err != nil {
		fmt.Fprintf(s.e.stderr, "backsight: setup.json not saved: %v\n", err)
	}
}

func (s *setupEnv) saveRecord(r *setup.Record) {
	if err := r.Save(s.dirs.SetupFile()); err != nil {
		fmt.Fprintf(s.e.stderr, "backsight: setup.json not saved: %v\n", err)
	}
	last := r.Steps[len(r.Steps)-1]
	line, _ := json.Marshal(map[string]any{"ts": now().UTC().Format(time.RFC3339Nano), "command": "setup", "step": last.Name, "status": last.Status, "run": r.Run})
	if f, err := os.OpenFile(s.dirs.JournalFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
}

// progress streams one JSON line on stderr with --json (decision 0064);
// detail says what a long step is doing.
func (s *setupEnv) progress(step string, pct, eta int, detail string) {
	if !s.e.json {
		return
	}
	ev := map[string]any{"event": "progress", "step": step, "pct": pct, "eta_s": eta}
	if detail != "" {
		ev["detail"] = detail
	}
	b, _ := json.Marshal(ev)
	fmt.Fprintln(s.e.stderr, string(b))
}

// Heartbeat periods of a long step: a JSON progress line, a text line;
// stepElapsed is the time a step has run (tests speed it up).
var (
	jsonBeat    = 5 * time.Second
	textBeat    = 30 * time.Second
	stepElapsed = time.Since
)

// heartbeat shows that the running step still works until the returned
// stop is called: with --json, a progress line every jsonBeat whose pct
// moves through the step's share (never reaching the next step's) and
// whose eta counts down; at a terminal, when text is true, a line every
// textBeat with the time spent. stop returns once nothing more is written.
func (s *setupEnv) heartbeat(ctx context.Context, detail string, text bool) (stop func()) {
	every := textBeat
	if s.e.json {
		every = jsonBeat
	} else if !text {
		return func() {}
	}
	cur, start := s.cur, time.Now()
	if !cur.start.IsZero() { // the step's time, across its phases
		start = cur.start
	}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			elapsed := stepElapsed(start)
			if !s.e.json {
				fmt.Fprintf(s.e.stdout, "         … %s: %s\n", detail, elapsed.Round(time.Second))
				continue
			}
			// The step's share fills as its estimate runs out, up to 95 %:
			// a slow link stays inside the step.
			frac := elapsed.Seconds() / float64(max(cur.eta, 1))
			frac = min(frac, 0.95)
			pct := cur.pct + int(frac*float64(cur.span))
			eta := max(cur.left-int(elapsed.Seconds()), cur.left-cur.eta+1)
			s.progress(cur.name, pct, eta, detail)
		}
	}()
	return func() { close(done); <-finished }
}

// planSteps lists what a run would do with these answers.
func (s *setupEnv) planSteps(a map[string]string) []setup.Step {
	cfgDetail := "write " + s.dirs.ConfigFile()
	if s.cfg != nil {
		cfgDetail = "adopt " + s.dirs.ConfigFile()
	}
	retention := "keep transcripts at least " + a["retention"] + " days"
	if a["retention"] == "keep" {
		retention = "leave Claude Code's setting as it is"
	}
	scheduler := "backsight jobs install: jobs tick every 15 min, jobs maintain daily (backups " + a["backups"] + ")"
	if a["backups"] == "on" {
		scheduler += "; generate the backup identity if there is none"
	}
	if a["scheduler"] == "off" {
		scheduler = "none (scheduler=off): run jobs tick and jobs maintain some other way"
		if st, err := state.Load(s.dirs.StateFile()); err == nil && st.Scheduler != nil && s.saved != nil && s.saved.Undo.Scheduler {
			scheduler = "none (scheduler=off): remove the entries an earlier run wrote; run jobs tick and jobs maintain some other way"
		}
	}
	reference := "nothing to retire"
	switch a["reference"] {
	case "retire":
		reference = fmt.Sprintf("backsight client retire: unwire %d piece(s) of the older Hindsight install after verify passes", s.refs)
	case "keep":
		reference = "keep the older Hindsight install wired"
	}
	backfill := "start empty"
	if a["backfill"] == "yes" {
		backfill = "queue past sessions for jobs tick"
	}
	return []setup.Step{
		{Name: "config", Status: "planned", Detail: cfgDetail},
		{Name: "secrets", Status: "planned", Detail: "check llm_api_key (exit 10 with the command to run when missing)"},
		{Name: "server", Status: "planned", Detail: fmt.Sprintf("backsight server install: %s/%s, API on 127.0.0.1:%s, UI on %s", a["provider"], a["model"], a["api_port"], a["ui_port"])},
		{Name: "docs", Status: "planned", Detail: "backsight guide index: the documentation into the server's docs bank, for backsight guide search (0 LLM calls)"},
		{Name: "client", Status: "planned", Detail: "backsight client install: plugin and permission rule in " + s.loc.Dir},
		{Name: "retention", Status: "planned", Detail: retention},
		{Name: "scheduler", Status: "planned", Detail: scheduler},
		{Name: "verify", Status: "planned", Detail: "backsight verify: every hook, the MCP handshake, a round trip in a scratch bank"},
		{Name: "reference", Status: "planned", Detail: reference},
		{Name: "backfill", Status: "planned", Detail: backfill},
	}
}

// reuseHint says how to change answers a rerun took from the last run.
func reuseHint(qs []setup.Question) string {
	var ids []string
	for _, q := range qs {
		if q.Source == "saved" && q.ID != "consent" {
			ids = append(ids, q.ID+"="+q.Value)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return "\nanswers from your last run: " + strings.Join(ids, ", ") + "\nto change them: backsight setup --reconfigure (asks again), or --answer id=value\n"
}

func setupPlanText(steps []setup.Step) string {
	var b strings.Builder
	b.WriteString("\nsetup will:\n")
	for _, s := range steps {
		fmt.Fprintf(&b, "  %-9s %s\n", s.Name, s.Detail)
	}
	return b.String()
}

func setupText(r setupResult) string {
	var b strings.Builder
	switch {
	case r.listing:
		for _, q := range r.Questions {
			fmt.Fprintf(&b, "%-17s %s", q.ID, q.Prompt)
			if !q.Asked() {
				fmt.Fprintf(&b, " → %s (%s)", q.Value, q.Source)
			}
			b.WriteString("\n")
		}
		return b.String()
	case r.Declined:
		return "nothing changed\n"
	case r.Plan:
		for _, c := range r.Preflight {
			fmt.Fprintf(&b, "%-6s %-11s %s\n", c.Status, c.ID, c.Summary)
			if c.Status == setup.Fail {
				fmt.Fprintf(&b, "%19sfix: %s\n", "", c.Fix)
			}
		}
		b.WriteString(setupPlanText(r.Steps))
		b.WriteString(reuseHint(r.Questions))
		return b.String()
	}
	for _, c := range r.Preflight {
		if c.Status == setup.Fail {
			fmt.Fprintf(&b, "fail   %-11s %s\n%19sfix: %s\n", c.ID, c.Summary, "", c.Fix)
		}
	}
	if len(r.Steps) > 0 && r.TryFirst != "" {
		b.WriteString("\nbacksight is set up.\n")
		for _, n := range r.NextSteps {
			b.WriteString("- " + n + "\n")
		}
		b.WriteString(r.TryFirst + "\n")
	}
	return b.String()
}
