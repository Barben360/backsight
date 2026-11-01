// Package supervise is the native server's supervisor on Windows (decision
// 0080): `backsight server supervise`, started by the per-user task, runs
// postgres then the API from supervise.json, restarts them with backoff,
// and answers a per-user named pipe (status, stop, api-stop, api-start).
// Windows has no per-user service manager: this is what systemd's two
// units do on Linux.
//
// Every process it starts is in a named kill-on-close Job Object it joins
// first, so nothing survives it (E29 requirement 3); a second supervisor
// of the same project exits at once, holding a named mutex (E34 ruling 1).
// No secret is on an argv or in supervise.json: the API's come from
// server.env, read at each start into its environment; child output goes
// to the logs through the redaction of the known secrets.
//
// On other OSes the transports are a unix socket and a flock, for the
// tests only: Linux runs systemd's units, never this.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/redact"
)

// Config is supervise.json, written by the native runtime's Render.
type Config struct {
	Project   string   `json:"project"`
	DataDir   string   `json:"data_dir"`
	Postgres  []string `json:"postgres"` // argv, no secret
	PGIsReady []string `json:"pg_isready"`
	PGCtlStop []string `json:"pg_ctl_stop"`
	API       []string `json:"api"`
	Env       []string `json:"env"`      // the tool environment, no secret
	EnvFile   string   `json:"env_file"` // server.env: the API's secrets, read at each API start
	// WorkDir is the children's working directory, the native root
	// (Linux's units: WorkingDirectory=<Root>); "" is server.env's
	// directory, the root too.
	WorkDir string `json:"work_dir,omitempty"`
	LogDir  string `json:"log_dir"`
	Venv    string `json:"venv"` // the deployed version (DeployedVenv)
}

// Options are what the command gives Run besides supervise.json.
type Options struct {
	// Check is server native-check's guard, run before every start of the
	// database. An error whose Final method returns true (the identity
	// check's *server.GuardError) is a refusal: nothing starts until stop.
	// Any other error is retried with the restart backoff. What it writes
	// to log (the strays it stops) goes to the supervisor's log, a line
	// each.
	Check   func(ctx context.Context, log io.Writer) error
	NoAPI   bool
	Secrets []string // redacted from child output and the supervisor's log
	Version string
}

// The states Status reports for the database and the API.
const (
	stateStarting   = "starting"
	stateRunning    = "running"
	stateRestarting = "restarting"
	stateStopped    = "stopped"
	stateRefused    = "refused"
)

// outputWait bounds the wait for the children's last output before the
// logs close (a postgres backend may hold the pipe a moment longer).
const outputWait = 2 * time.Second

// refusedFile, under the root (server.env's directory), holds the reason
// of a refusal for server status and doctor; it is removed once the guard
// passes.
const refusedFile = "refused"

// timing holds the supervisor's numbers (0080's Global Constraints); the
// tests shorten them and fake the restart clock.
type timing struct {
	backoffMin, backoffMax, resetAfter time.Duration // restart backoff
	readyPoll, readyWait               time.Duration // pg_isready
	apiStopWait, dbStopWait            time.Duration // CTRL_BREAK → kill; pg_ctl stop
	requestWait                        time.Duration // a client's request and answer
	after                              func(time.Duration) <-chan time.Time
	now                                func() time.Time
	interrupt                          func(*os.Process) error // CTRL_BREAK / SIGTERM to the group
	// wrapOutput, set by a test, wraps a child's log writer (a slow one).
	wrapOutput func(io.WriteCloser) io.WriteCloser
}

var tm = timing{
	backoffMin: time.Second, backoffMax: 5 * time.Minute, resetAfter: time.Minute,
	readyPoll: 100 * time.Millisecond, readyWait: 300 * time.Second,
	apiStopWait: 30 * time.Second, dbStopWait: 60 * time.Second,
	requestWait: 5 * time.Second,
	after:       time.After, now: time.Now, interrupt: interrupt,
}

// backoff is a child's restart delay: 1 s doubling to 5 min, back to 1 s
// once the child stayed up a minute.
type backoff struct{ cur time.Duration }

func (b *backoff) next(up time.Duration) time.Duration {
	if up >= tm.resetAfter {
		b.cur = 0
	}
	if b.cur == 0 {
		b.cur = tm.backoffMin
	} else {
		b.cur = min(2*b.cur, tm.backoffMax)
	}
	return b.cur
}

// isFinal: err is a refusal no retry can change.
func isFinal(err error) bool {
	var f interface{ Final() bool }
	return errors.As(err, &f) && f.Final()
}

type supervisor struct {
	cfg    Config
	opts   Options
	slog   *rotating
	dbLog  *rotating
	apiLog *rotating

	mu sync.Mutex
	st Status
	// envSecrets are server.env's secrets, read at each API start: a
	// rotated key is redacted from then on.
	envSecrets []string

	// outputs are the children's output copies, waited for (a bounded
	// time) before the logs close.
	outputs []*proc

	ops chan opRequest
}

type opRequest struct {
	op    string
	reply chan Status
}

// Run supervises cfg's processes until a stop request (nil) or ctx's end
// (ctx.Err()), each followed by stopping both; a second supervisor of
// cfg.Project returns nil at once, having started nothing.
func Run(ctx context.Context, cfg Config, opts Options) error {
	if err := cfg.check(opts.NoAPI); err != nil {
		return err
	}
	if err := fsutil.MkdirPrivate(cfg.LogDir); err != nil {
		return err
	}
	s := &supervisor{cfg: cfg, opts: opts, ops: make(chan opRequest)}
	var err error
	if s.slog, err = openRotating(s.logPath("supervisor")); err != nil {
		return err
	}
	defer func() { _ = s.slog.Close() }()

	release, err := acquire(cfg.Project)
	if errors.Is(err, errRunning) {
		s.logf("another supervisor of %s runs: this one (pid %d) exits", cfg.Project, os.Getpid())
		return nil
	}
	if err != nil {
		return s.fail(fmt.Errorf("taking the supervisor's single-instance lock: %w", err))
	}
	defer release()
	if err := joinJob(cfg.Project); err != nil {
		return s.fail(fmt.Errorf("joining the server's job object: %w", err))
	}
	if s.dbLog, err = openRotating(s.logPath("db")); err != nil {
		return s.fail(err)
	}
	defer func() { _ = s.dbLog.Close() }()
	if s.apiLog, err = openRotating(s.logPath("api")); err != nil {
		return s.fail(err)
	}
	defer func() { _ = s.apiLog.Close() }()

	api := stateStarting
	if opts.NoAPI {
		api = stateStopped
	}
	s.st = Status{Proto: Proto, Version: opts.Version, PID: os.Getpid(), DB: stateStarting, API: api, Venv: cfg.Venv}
	l, err := listen(cfg.Project)
	if err != nil {
		return s.fail(fmt.Errorf("opening the supervisor's control channel: %w", err))
	}
	served := serve(l, s.answer)
	defer func() {
		_ = l.Close()
		<-served
	}()
	s.logf("supervisor of %s started (pid %d, backsight %s, server %s)", cfg.Project, os.Getpid(), opts.Version, cfg.Venv)
	err = s.loop(ctx)
	s.waitOutputs(outputWait)
	s.logf("supervisor of %s stopped", cfg.Project)
	return err
}

func (c Config) check(noAPI bool) error {
	var missing []string
	for name, empty := range map[string]bool{"project": c.Project == "", "postgres": len(c.Postgres) == 0,
		"pg_isready": len(c.PGIsReady) == 0, "pg_ctl_stop": len(c.PGCtlStop) == 0, "log_dir": c.LogDir == "",
		"env_file": c.EnvFile == "", "api": len(c.API) == 0 && !noAPI} {
		if empty {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("supervise.json names no %s: run backsight server render", strings.Join(missing, ", "))
	}
	return nil
}

func (s *supervisor) logPath(what string) string {
	return filepath.Join(s.cfg.LogDir, s.cfg.Project+"-"+what+".log")
}

// root is the native runtime's root: server.env's directory.
func (s *supervisor) root() string { return filepath.Dir(s.cfg.EnvFile) }

// workDir is the children's working directory: work_dir, else the root.
func (s *supervisor) workDir() string {
	if s.cfg.WorkDir != "" {
		return s.cfg.WorkDir
	}
	return s.root()
}

// secrets are the values the logs never carry: Options.Secrets and
// server.env's secret-named values (and its database URL's password).
func (s *supervisor) secrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(append([]string{}, s.opts.Secrets...), s.envSecrets...)
}

func (s *supervisor) redact(text string) string { return redact.Values(text, s.secrets()) }

// envSecretValues are the secrets among server.env's pairs: the values of
// secret-named keys and the password of any URL value.
func envSecretValues(pairs []string) []string {
	var out []string
	for _, kv := range pairs {
		k, v, _ := strings.Cut(kv, "=")
		if redact.EnvSecretKey.MatchString(k) {
			out = append(out, v)
		}
		if u, err := url.Parse(v); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok {
				out = append(out, pw)
			}
		}
	}
	return out
}

// started records p's output copy and why it has no process tree, if so.
func (s *supervisor) started(name string, p *proc) {
	s.mu.Lock()
	// The finished copies go: a supervisor restarting for days keeps none.
	s.outputs = slices.DeleteFunc(s.outputs, func(q *proc) bool {
		select {
		case <-q.outDone:
			return true
		default:
			return false
		}
	})
	s.outputs = append(s.outputs, p)
	s.mu.Unlock()
	if p.treeErr != nil {
		s.logf("%s (pid %d) runs without a job of its own, a kill ends it alone: %v", name, p.pid, p.treeErr)
	}
}

// waitOutputs gives the children's last output up to d to reach the logs.
func (s *supervisor) waitOutputs(d time.Duration) {
	deadline := time.Now().Add(d)
	s.mu.Lock()
	outs := s.outputs
	s.mu.Unlock()
	for _, p := range outs {
		p.waitOutput(max(time.Until(deadline), 0))
	}
}

// logLines is the supervisor's log as a writer: each line written is a
// line of it, stamped and redacted (a Write is whole lines: the guard's
// Fprintf calls).
type logLines struct{ s *supervisor }

func (l logLines) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			l.s.logf("%s", line)
		}
	}
	return len(p), nil
}

// logf writes a line to the supervisor's log, redacted.
func (s *supervisor) logf(format string, args ...any) {
	line := tm.now().UTC().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...) + "\n"
	_, _ = io.WriteString(s.slog, s.redact(line))
}

func (s *supervisor) fail(err error) error {
	s.logf("supervisor of %s failed: %v", s.cfg.Project, err)
	return errors.New(s.redact(err.Error()))
}

func (s *supervisor) set(f func(st *Status)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.st)
}

func (s *supervisor) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// answer is the control channel's handler: status from here, the other
// requests through the loop, which answers once it acted.
func (s *supervisor) answer(op string, done <-chan struct{}) (Status, bool) {
	if op == opStatus {
		return s.status(), true
	}
	r := opRequest{op: op, reply: make(chan Status, 1)}
	select {
	case s.ops <- r:
	case <-done:
		return Status{}, false
	}
	select {
	case st := <-r.reply:
		return st, true
	case <-done:
		// The loop answers before it returns and the channel closes
		// (stop): an answer already there still goes to the client.
		select {
		case st := <-r.reply:
			return st, true
		default:
			return Status{}, false
		}
	}
}

// readyResult is a pg_isready poll's end for database start gen.
type readyResult struct {
	gen int
	err error
}

// run is the loop's state: what runs, what is wanted, what is pending.
type run struct {
	db, api          *proc
	dbGen            int
	dbReady          bool
	dbUp, apiUp      time.Time
	apiWanted        bool
	dbBack, apiBack  backoff
	dbTimer, apiTime <-chan time.Time
	stopPoll         context.CancelFunc
	polls            sync.WaitGroup // every pollReady, waited for before the loop returns
	ready            chan readyResult
}

func immediate() <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

func (s *supervisor) loop(ctx context.Context) error {
	r := &run{apiWanted: !s.opts.NoAPI, dbTimer: immediate(), ready: make(chan readyResult, 1), stopPoll: func() {}}
	defer func() {
		r.stopPoll()
		r.polls.Wait()
	}()
	for {
		var dbDone, apiDone <-chan error
		if r.db != nil {
			dbDone = r.db.done
		}
		if r.api != nil {
			apiDone = r.api.done
		}
		select {
		case <-ctx.Done():
			s.logf("cancelled: stopping")
			s.stopAll(context.WithoutCancel(ctx), r)
			return ctx.Err()
		case <-r.dbTimer:
			r.dbTimer = nil
			if err := s.startDB(ctx, r); err != nil {
				return s.refused(ctx, err)
			}
		case <-r.apiTime:
			r.apiTime = nil
			s.startAPI(r)
		case res := <-r.ready:
			if res.gen != r.dbGen || r.db == nil {
				continue
			}
			if res.err != nil {
				s.logf("db: %v: killing it", res.err)
				_ = r.db.kill()
				continue
			}
			r.dbReady = true
			s.set(func(st *Status) { st.DB = stateRunning })
			s.logf("db ready (pid %d)", r.db.pid)
			if r.apiWanted && r.api == nil {
				r.apiTime = nil
				s.startAPI(r)
			}
		case err := <-dbDone:
			up := tm.now().Sub(r.dbUp)
			s.logf("db exited after %s: %v", up.Round(time.Millisecond), exitText(err))
			r.db, r.dbReady = nil, false
			r.stopPoll()
			s.set(func(st *Status) { st.DB = stateRestarting })
			// The API on a database that is gone: stopped first, started
			// again once the database is ready.
			s.stopAPI(r)
			if r.apiWanted {
				s.set(func(st *Status) { st.API = stateRestarting })
			}
			d := r.dbBack.next(up)
			s.logf("db restarts in %s", d)
			r.dbTimer = tm.after(d)
		case err := <-apiDone:
			up := tm.now().Sub(r.apiUp)
			s.logf("api exited after %s: %v", up.Round(time.Millisecond), exitText(err))
			r.api = nil
			if r.apiWanted && r.dbReady {
				d := r.apiBack.next(up)
				s.logf("api restarts in %s", d)
				s.set(func(st *Status) { st.API = stateRestarting })
				r.apiTime = tm.after(d)
			}
		case req := <-s.ops:
			switch req.op {
			case opStop:
				s.logf("stop requested")
				req.reply <- s.status()
				s.stopAll(ctx, r)
				return nil
			case opAPIStop:
				s.logf("api-stop requested")
				r.apiWanted = false
				s.stopAPI(r)
				s.set(func(st *Status) { st.API = stateStopped })
			case opAPIStart:
				s.logf("api-start requested")
				r.apiWanted = true
				if r.api == nil && r.dbReady {
					r.apiTime = nil
					s.startAPI(r)
				} else if r.api == nil {
					s.set(func(st *Status) { st.API = stateStarting })
				}
			}
			req.reply <- s.status()
		}
	}
}

// startDB runs the guard, then postgres and its readiness poll. A final
// refusal is returned; anything else failing schedules a retry.
func (s *supervisor) startDB(ctx context.Context, r *run) error {
	retry := func(what string, err error) {
		d := r.dbBack.next(0)
		s.logf("db: %s: %v; retrying in %s", what, err, d)
		s.set(func(st *Status) { st.DB = stateRestarting })
		r.dbTimer = tm.after(d)
	}
	if s.opts.Check != nil {
		if err := s.opts.Check(ctx, logLines{s}); err != nil {
			if isFinal(err) {
				return err
			}
			retry("the guard failed", err)
			return nil
		}
	}
	if err := os.Remove(filepath.Join(s.root(), refusedFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logf("removing %s: %v", refusedFile, err)
	}
	p, err := startChild(s.cfg.Postgres, s.cfg.Env, s.workDir(), newLineRedactor(s.dbLog, s.secrets()), true)
	if err != nil {
		retry("starting postgres", err)
		return nil
	}
	s.started("db", p)
	r.db, r.dbUp, r.dbReady = p, tm.now(), false
	r.dbGen++
	s.logf("db started (pid %d)", p.pid)
	s.set(func(st *Status) {
		if st.DB != stateRestarting {
			st.DB = stateStarting
		}
	})
	pctx, cancel := context.WithCancel(ctx)
	r.stopPoll = cancel
	gen := r.dbGen
	r.polls.Go(func() { s.pollReady(pctx, gen, r.ready) })
	return nil
}

// pollReady runs pg_isready every readyPoll until it answers, for up to
// readyWait.
func (s *supervisor) pollReady(ctx context.Context, gen int, out chan<- readyResult) {
	deadline := time.Now().Add(tm.readyWait)
	var res readyResult
	for {
		if err := runTool(ctx, s.cfg.PGIsReady, s.cfg.Env, nil); err == nil {
			res = readyResult{gen: gen}
			break
		}
		if !time.Now().Before(deadline) {
			res = readyResult{gen: gen, err: fmt.Errorf("pg_isready did not answer within %s", tm.readyWait)}
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(tm.readyPoll):
		}
	}
	select {
	case out <- res:
	case <-ctx.Done():
	}
}

// startAPI starts the API with the tool environment and server.env's
// pairs, read now; a failure schedules a retry.
func (s *supervisor) startAPI(r *run) {
	retry := func(what string, err error) {
		d := r.apiBack.next(0)
		s.logf("api: %s: %v; retrying in %s", what, err, d)
		s.set(func(st *Status) { st.API = stateRestarting })
		r.apiTime = tm.after(d)
	}
	pairs, err := readEnvFile(s.cfg.EnvFile)
	if err != nil {
		retry("reading its environment", err)
		return
	}
	s.mu.Lock()
	s.envSecrets = envSecretValues(pairs)
	s.mu.Unlock()
	p, err := startChild(s.cfg.API, append(append([]string{}, s.cfg.Env...), pairs...), s.workDir(), newLineRedactor(s.apiLog, s.secrets()), false)
	if err != nil {
		retry("starting it", err)
		return
	}
	s.started("api", p)
	r.api, r.apiUp = p, tm.now()
	s.logf("api started (pid %d)", p.pid)
	s.set(func(st *Status) { st.API = stateRunning })
}

// stopAPI stops the API (CTRL_BREAK, then a kill after apiStopWait) and
// cancels a pending restart.
func (s *supervisor) stopAPI(r *run) {
	r.apiTime = nil
	if r.api == nil {
		return
	}
	s.logf("api stopping (pid %d)", r.api.pid)
	how, err := r.api.stop(tm.apiStopWait)
	switch how {
	case stopKilled:
		s.logf("api killed, still running %s after the interrupt: %v", tm.apiStopWait, exitText(err))
	case stopNoInterrupt:
		s.logf("api: no console to interrupt it: killed (%v)", exitText(err))
	}
	s.logf("api stopped: %v", exitText(err))
	r.api = nil
}

// stopAll stops the API, then the database with pg_ctl stop (killed if
// still running after dbStopWait).
func (s *supervisor) stopAll(ctx context.Context, r *run) {
	r.dbTimer = nil
	s.stopAPI(r)
	s.set(func(st *Status) { st.API = stateStopped })
	r.stopPoll()
	if r.db != nil {
		s.logf("db stopping (pid %d): %s", r.db.pid, strings.Join(s.cfg.PGCtlStop, " "))
		deadline := time.Now().Add(tm.dbStopWait)
		sctx, cancel := context.WithDeadline(ctx, deadline)
		if err := runTool(sctx, s.cfg.PGCtlStop, s.cfg.Env, newLineRedactor(s.dbLog, s.secrets())); err != nil {
			s.logf("db: pg_ctl stop: %v", err)
		}
		cancel()
		select {
		case err := <-r.db.done:
			s.logf("db stopped: %v", exitText(err))
		case <-time.After(time.Until(deadline)):
			s.logf("db still running after %s: killing it", tm.dbStopWait)
			_ = r.db.kill()
			s.logf("db killed: %v", exitText(<-r.db.done))
		}
		r.db = nil
	}
	s.set(func(st *Status) { st.DB = stateStopped })
}

// refused records a final refusal (the refused file, the status, the
// log) and starts nothing; the control channel answers until stop.
func (s *supervisor) refused(ctx context.Context, err error) error {
	reason := s.redact(err.Error())
	s.logf("refused: %s", reason)
	if werr := fsutil.WriteAtomic(filepath.Join(s.root(), refusedFile), []byte(reason+"\n"), 0o600); werr != nil {
		s.logf("writing %s: %v", refusedFile, werr)
	}
	s.set(func(st *Status) { st.DB, st.API, st.Refusal = stateRefused, stateStopped, reason })
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req := <-s.ops:
			req.reply <- s.status()
			if req.op == opStop {
				s.logf("stop requested")
				return nil
			}
		}
	}
}

func exitText(err error) string {
	if err == nil {
		return "exit 0"
	}
	return err.Error()
}
