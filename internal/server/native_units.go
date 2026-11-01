package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/supervise"
)

// The native server's files, start, stop and state, through its host
// (native_host.go): systemd's units on Linux.

// freePort is the first port from from on that 127.0.0.1 can listen on,
// skip left out; tests replace it.
var freePort = func(from int, skip ...int) (int, error) {
	for p := from; p > 0 && p <= 65535; p++ {
		if slices.Contains(skip, p) {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err == nil {
			_ = ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port on 127.0.0.1 from %d", from)
}

// apiHealth asks the API's /health and returns its database field; tests
// replace it. apiWait bounds how long Start waits for "connected" (the
// health wait of a Docker start), apiPoll is how often it asks.
var (
	apiHealth = func(ctx context.Context, base string) (string, error) {
		cl, err := hindsight.New(base, "", 5*time.Second)
		if err != nil {
			return "", err
		}
		h, err := cl.Health(ctx)
		return h.Database, err
	}
	apiWait = 10 * time.Minute
	apiPoll = 2 * time.Second
)

// dbWait bounds how long a start waits for the database to answer; dbPoll
// is how often it asks.
var (
	dbWait = 60 * time.Second
	dbPoll = 250 * time.Millisecond
)

const (
	// pgInstance is pg0's instance name: ~/.pg0/instances/<it>.
	pgInstance = "hindsight"
	// pgPortKey in server.env keeps the database's port across renders.
	pgPortKey = "BACKSIGHT_PG_PORT"
	// restartPending lists the units whose files or environment changed
	// under a running server (ruling R1): Start restarts them.
	restartPending = "restart-pending"
)

func (n Native) project() string {
	if n.Cfg.Server.Project == "" {
		return Project
	}
	return n.Cfg.Server.Project
}

// DBUnit is the database's unit, ServerUnit Hindsight's.
func (n Native) DBUnit() string     { return n.project() + "-db.service" }
func (n Native) ServerUnit() string { return n.project() + "-server.service" }

// DataDir is the database's data directory (pg0's instance; on Windows
// <Root>\db, InstanceDir itself).
func (n Native) DataDir() string {
	if n.goos() == "windows" {
		return n.InstanceDir()
	}
	return n.join(n.InstanceDir(), "data")
}

func (n Native) shim() string { return n.join(n.Dirs.Data, "bin", plugin.ShimName(n.goos())) }

func (n Native) pendingFile() string { return n.join(n.Root(), restartPending) }

// units lists the unit files and the service names Status reports them
// under, the database first.
func (n Native) units() [][2]string {
	return [][2]string{{n.DBUnit(), "db"}, {n.ServerUnit(), "hindsight"}}
}

// Render builds server.env and the two units for Version. The database's
// port is the one server.env already names, else the first free one after
// the API's.
func (n Native) Render(sec config.Secrets, host string) (Files, error) {
	if n.Err != nil {
		return Files{}, n.Err
	}
	cfg := n.Cfg
	if err := checkRenderable(cfg, sec); err != nil {
		return Files{}, err
	}
	for _, p := range []string{n.Root(), n.shim()} {
		if strings.ContainsAny(p, "\n\r") {
			return Files{}, fmt.Errorf("%q: a unit file cannot carry a path with a line break", p)
		}
	}
	api, err := cfg.APIPort()
	if err != nil {
		return Files{}, err
	}
	pg, err := n.pgPort(api)
	if err != nil {
		return Files{}, err
	}
	env := &envFile{}
	env.comment(envHeader)
	dsn := url.URL{Scheme: "postgresql", User: url.UserPassword("hindsight", sec.DBPassword),
		Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(pg)), Path: "/hindsight"}
	cfg.LLM.BaseURL = onThisHost(cfg.LLM.BaseURL)
	hindsightEnv(env, cfg, sec, host, dsn.String(), false)
	env.comment("Native runtime: the API on this PC only, the models in HF_HOME, the database's port kept across renders.")
	env.plain("HINDSIGHT_API_HOST", "127.0.0.1")
	env.plain("HINDSIGHT_API_PORT", strconv.Itoa(api))
	env.quoted("HOME", n.Home())
	env.quoted("HF_HOME", n.HF())
	env.plain(pgPortKey, strconv.Itoa(pg))
	// Hindsight's entry points load the first .env found up from their
	// working directory over this environment; Docker's <compose_dir>/.env
	// is one of them after a switch. python-dotenv skips any with this.
	env.comment("No .env file from the server's directory or above overrides this file.")
	env.plain("PYTHON_DOTENV_DISABLED", "1")
	if n.goos() == "windows" {
		// The API's output is a pipe to the supervisor's log, which Python
		// encodes in the ANSI code page: Hindsight's banner (U+2584) then
		// stops the API at its start (seen on the VM).
		env.comment("Windows: the API writes UTF-8 to its log, whatever the code page.")
		env.plain("PYTHONUTF8", "1")
	}
	if env.err != nil {
		return Files{}, env.err
	}
	units, err := n.host().Files(pg, n.Version())
	if err != nil {
		return Files{}, err
	}
	return Files{Env: env.buf.Bytes(), Units: units}, nil
}

// onThisHost is an LLM base URL as the native server reaches it:
// Docker's name for the host, host.docker.internal, is 127.0.0.1 there
// (ruling R32).
func onThisHost(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() != "host.docker.internal" {
		return base
	}
	port := u.Port()
	u.Host = "127.0.0.1"
	if port != "" {
		u.Host = net.JoinHostPort("127.0.0.1", port)
	}
	return u.String()
}

// checkRenderable refuses what neither runtime can render: another
// embeddings preset, a missing secret.
func checkRenderable(cfg config.Config, sec config.Secrets) error {
	if cfg.Embeddings.Preset != "multilingual" {
		return fmt.Errorf("embeddings preset %q is not available yet: use multilingual", cfg.Embeddings.Preset)
	}
	var missing []string
	have := sec.Names()
	for name, needed := range config.RequiredSecrets(cfg) {
		if needed && !have[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return &MissingSecretsError{Names: missing}
	}
	return nil
}

// pgPort is server.env's database port, else a free one after the API's,
// never the API's or the UI's.
func (n Native) pgPort(api int) (int, error) {
	if p, err := n.envPort(); err == nil {
		return p, nil
	}
	return freePort(api+1, api, n.Cfg.Server.UIPort)
}

// envPort reads the database's port from server.env.
func (n Native) envPort() (int, error) {
	raw, err := os.ReadFile(n.EnvFile())
	if err != nil {
		return 0, fmt.Errorf("the native server's environment: %w (run backsight server render)", err)
	}
	for _, kv := range parseEnvFile(raw) {
		if v, ok := strings.CutPrefix(kv, pgPortKey+"="); ok {
			if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
				return p, nil
			}
		}
	}
	return 0, fmt.Errorf("%s names no database port (%s): run backsight server render", n.EnvFile(), pgPortKey)
}

// parseEnvFile reads the KEY=value lines of an environment file Render
// wrote, without the quotes quoted() adds: the supervisor's reading.
func parseEnvFile(raw []byte) []string { return supervise.ParseEnv(raw) }

// shellWord is s as one word for /bin/sh.
func shellWord(s string) string {
	if s != "" && safeShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var safeShellWord = regexp.MustCompile(`^[A-Za-z0-9_./:=@+-]+$`)

// pgSettings are pg0's own settings, which a postgres started directly
// would otherwise not get (ruling R23).
var pgSettings = []string{"shared_buffers=256MB", "work_mem=64MB", "maintenance_work_mem=512MB", "effective_cache_size=1GB",
	"max_parallel_maintenance_workers=4", "timezone=UTC", "log_timezone=UTC"}

// Write installs f: server.env (0600) and home/ (0700) directly, the units
// through the config editor, each only when it changed. A change under a
// unit that already existed is recorded in restart-pending first (ruling
// R1): the next Start restarts what runs.
func (n Native) Write(f Files) (Changes, error) { return n.write(f, true) }

// Diff is what Write would change, writing nothing.
func (n Native) Diff(f Files) (Changes, error) { return n.write(f, false) }

func (n Native) write(f Files, apply bool) (Changes, error) {
	var c Changes
	if n.Err != nil {
		return c, n.Err
	}
	h := n.host()
	dir := h.Dir()
	if dir == "" { // the systemd host's unit directory; the task host's is Root()
		return c, errors.New("no systemd user unit directory for the native server")
	}
	var err error
	if c.Env, err = differs(n.EnvFile(), f.Env); err != nil {
		return c, err
	}
	var restart []string
	if c.Env && pathExists(n.EnvFile()) {
		restart = append(restart, h.restartsFor("")...)
	}
	var edits []claudecfg.Edit
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range f.Units {
			if !yield(k) {
				return
			}
		}
	}) {
		path, content := filepath.Join(dir, name), string(f.Units[name])
		d, err := differs(path, f.Units[name])
		if err != nil {
			return c, err
		}
		if !d {
			continue
		}
		c.Units = true
		if pathExists(path) {
			restart = append(restart, h.restartsFor(name)...)
		}
		edits = append(edits, claudecfg.Edit{Target: path, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
			var before *string
			if doc != nil {
				old := string(doc)
				before = &old
			}
			return []byte(content), []claudecfg.Change{{Path: "(whole file)", Old: before, New: &content}}, nil
		}})
	}
	if !apply {
		return c, nil
	}
	if err := n.ensureRoot(); err != nil {
		return c, err
	}
	if err := n.ensureHome(); err != nil {
		return c, err
	}
	if len(restart) > 0 {
		if err := n.markRestart(restart); err != nil {
			return c, err
		}
	}
	if c.Env {
		if err := fsutil.WriteAtomic(n.EnvFile(), f.Env, 0o600); err != nil {
			return c, err
		}
	}
	if _, err := os.Stat(n.EnvFile()); err == nil {
		if err := fsutil.MakePrivate(n.EnvFile()); err != nil { // loosened by hand: it holds secrets
			return c, err
		}
	}
	if len(edits) > 0 {
		if _, _, err := n.editor("server render").Apply(edits); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (n Native) editor(command string) claudecfg.Runner {
	return claudecfg.Runner{PreImages: n.Dirs.PreImagesDir(), Journal: n.Dirs.JournalFile(), Command: command}
}

func pathExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// markRestart adds the host's markers (systemd: units) to
// restart-pending.
func (n Native) markRestart(marks []string) error {
	have := n.pending()
	for _, u := range marks {
		have[u] = true
	}
	var lines []string
	for _, u := range n.host().markers() {
		if have[u] {
			lines = append(lines, u)
		}
	}
	return fsutil.WriteAtomic(n.pendingFile(), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// pending reads restart-pending: a marker naming nothing of the host's
// restarts the server (the API).
func (n Native) pending() map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(n.pendingFile())
	if err != nil {
		return out
	}
	marks := n.host().markers()
	for _, l := range strings.Fields(string(raw)) {
		if slices.Contains(marks, l) {
			out[l] = true
		}
	}
	if len(out) == 0 {
		out[marks[1]] = true
	}
	return out
}

// redactedError is an error whose text had the secrets taken out; it
// still wraps the original (exec.ErrNotFound stays visible).
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// secrets are secrets.toml's, for redaction; none when it cannot be read.
func (n Native) secrets() config.Secrets {
	if n.Dirs.Config == "" {
		return config.Secrets{}
	}
	sec, _ := config.LoadSecrets(n.Dirs.SecretsFile())
	return sec
}

// redact is err with every secret replaced.
func (n Native) redact(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: Redact(err.Error(), n.secrets()), err: err}
}

// sys runs a system tool; its error carries the output, redacted.
func (n Native) sys(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := n.Sys.Run(ctx, name, args...)
	if err == nil {
		return out, nil
	}
	err = fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	if t := strings.TrimSpace(string(out)); t != "" {
		err = fmt.Errorf("%w: %s", err, t)
	}
	return out, n.redact(err)
}

// dbPassword is secrets.toml's db_password, the only source of it (ruling
// R11).
func (n Native) dbPassword() (string, error) {
	sec, err := config.LoadSecrets(n.Dirs.SecretsFile())
	if err != nil {
		return "", err
	}
	if sec.DBPassword == "" {
		return "", errors.New("secrets.toml holds no db_password: run backsight secret set db_password")
	}
	if strings.ContainsAny(sec.DBPassword, "'\\\n\r") {
		return "", errors.New("db_password holds a quote, backslash or line break: set another (backsight secret set db_password)")
	}
	return sec.DBPassword, nil
}

// pgEnv is a bundled Postgres client's whole environment: the password in
// PGPASSWORD, never on its argv.
func (n Native) pgEnv(password string) []string {
	return append(n.toolEnv(), "PGPASSWORD="+password)
}

// waitDB waits until the database answers a query as hindsight, dbWait at
// most.
func (n Native) waitDB(ctx context.Context) error {
	pw, err := n.dbPassword()
	if err != nil {
		return err
	}
	port, err := n.envPort()
	if err != nil {
		return err
	}
	psql := n.pgBin("psql")
	deadline := time.Now().Add(dbWait)
	for {
		_, err := n.Tools.Run(ctx, n.pgEnv(pw), nil, psql, "-h", "127.0.0.1", "-p", strconv.Itoa(port),
			"-U", "hindsight", "-d", "hindsight", "-At", "-v", "ON_ERROR_STOP=1", "-c", "select 1")
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			if n.goos() == "windows" {
				return n.redact(fmt.Errorf("the database (127.0.0.1:%d) did not answer within %s: %w: see %s",
					port, dbWait, err, taskHost{n: n}.logFile("db")))
			}
			return n.redact(fmt.Errorf("the database (%s, 127.0.0.1:%d) did not answer within %s: %w", n.DBUnit(), port, dbWait, err))
		}
		select {
		case <-ctx.Done():
		case <-time.After(dbPoll):
		}
	}
}

// Start creates the database on a first install and refuses a missing,
// empty or foreign one (requirement 2), enables both units (with linger
// they start at boot), waits for the database to answer and checks its
// fsync (requirement 1: off stops and disables both), then waits for the
// API to answer with its database connected. What restart-pending lists
// is restarted, and the marker removed only once the API answered
// (rulings R1, R30).
func (n Native) Start(ctx context.Context) error { return n.start(ctx, false) }

// RestartSupervisor is Start with the supervisor restarted whatever
// restart-pending says (Windows): update apply's, when the supervisor
// that runs speaks another protocol than the new backsight, or still runs
// the old one after a server upgrade. The task's action runs the shim,
// the new binary since the update's switch. The systemd host has no
// supervisor: an error.
func (n Native) RestartSupervisor(ctx context.Context) error {
	if _, ok := n.host().(taskHost); !ok {
		return errors.New("the native server has a supervisor on Windows only")
	}
	return n.start(ctx, true)
}

// start is Start; restart forces the database side's restart (the task
// host: the supervisor).
func (n Native) start(ctx context.Context, restart bool) error {
	if n.Err != nil {
		return n.Err
	}
	if err := n.checkVenv(); err != nil {
		return err
	}
	if err := n.guardStart(ctx); err != nil {
		return err
	}
	h := n.host()
	pending := n.pending()
	if !pathExists(n.pendingFile()) {
		pending = map[string]bool{}
	}
	marks := h.markers()
	db, srv := marks[0], marks[1]
	// A supervisor restarted on purpose starts an API that reads the
	// server.env of now: nothing more to restart.
	apiRestart := (pending[db] || pending[srv]) && !restart
	if err := h.StartDB(ctx, pending[db] || restart); err != nil {
		return err
	}
	if err := n.waitDB(ctx); err != nil {
		return err
	}
	if err := n.fsyncOrStop(ctx); err != nil {
		return err
	}
	if err := h.StartAPI(ctx, apiRestart); err != nil {
		return err
	}
	if err := n.waitAPI(ctx); err != nil {
		return err
	}
	if err := os.Remove(n.pendingFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// checkVenv refuses a server unit whose venv is not marked complete
// (rulings R2, R40): an interrupted install, or one removed by hand,
// never runs.
func (n Native) checkVenv() error {
	v, err := n.Deployed()
	if err != nil {
		return err
	}
	if v == "" {
		v = n.Version()
	}
	if !n.venvInstalled(v) {
		return fmt.Errorf("the native server's venv %s is incomplete (no %s): nothing was started; run backsight server install to re-fetch it",
			n.VenvDir(v), venvComplete)
	}
	return nil
}

// waitAPI waits until the API on 127.0.0.1 answers /health with its
// database connected, apiWait at most (ruling R30): Start's callers
// expect a running server, as Docker's compose up --wait gives them.
func (n Native) waitAPI(ctx context.Context) error {
	api, err := n.Cfg.APIPort()
	if err != nil {
		return err
	}
	base := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(api))
	deadline := time.Now().Add(apiWait)
	for {
		db, err := apiHealth(ctx, base)
		if err == nil && db == "connected" {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("its database is %q", db)
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			if n.goos() == "windows" {
				return n.redact(fmt.Errorf("the server did not answer %s/health with its database connected within %s (%w): see %s",
					base, apiWait, err, taskHost{n: n}.logFile("api")))
			}
			return n.redact(fmt.Errorf("the server (%s) did not answer %s/health with its database connected within %s (%w): see journalctl --user -u %s",
				n.ServerUnit(), base, apiWait, err, n.ServerUnit()))
		}
		select {
		case <-ctx.Done():
		case <-time.After(apiPoll):
		}
	}
}

// StartDatabase starts the database unit alone, enabling nothing, behind
// the same guards as Start, and waits until it answers with fsync on.
func (n Native) StartDatabase(ctx context.Context) error {
	if n.Err != nil {
		return n.Err
	}
	if err := n.guardStart(ctx); err != nil {
		return err
	}
	h := n.host()
	s, ok := h.(dbAloneStarter)
	if !ok {
		return errNoDBAlone
	}
	if err := s.startDBAlone(ctx); err != nil {
		return err
	}
	if err := n.waitDB(ctx); err != nil {
		return err
	}
	return n.fsyncOrStop(ctx)
}

// installedFiles are the host's files that exist, in fileNames' order
// (systemd: the units, the server first).
func (n Native) installedFiles() []string {
	var out []string
	h := n.host()
	dir := h.Dir()
	for _, u := range h.fileNames() {
		if pathExists(filepath.Join(dir, u)) {
			out = append(out, u)
		}
	}
	return out
}

// StopAPI stops the server unit; the database keeps running.
func (n Native) StopAPI(ctx context.Context) error {
	if n.Err != nil {
		return n.Err
	}
	return n.host().StopAPI(ctx)
}

// Stop stops and disables both units: nothing starts at boot (RF 2).
func (n Native) Stop(ctx context.Context) error {
	if n.Err != nil {
		return n.Err
	}
	return n.host().Stop(ctx)
}

func (n Native) StopCmd() string { return n.host().StopCmd() }

// Delete stops and disables both units (Windows: stops the supervisor
// and disables its task) before anything is removed, then removes the
// host's files through the config editor, has the host forget them
// (unregister the task), then removes Root(): the database, the
// environments and the models.
func (n Native) Delete(ctx context.Context) error {
	if n.Err != nil {
		return n.Err
	}
	h := n.host()
	units := n.installedFiles()
	if len(units) > 0 || h.stopsWithoutFiles() {
		if err := h.Stop(ctx); err != nil {
			return err
		}
		var edits []claudecfg.Edit
		for _, u := range units {
			edits = append(edits, claudecfg.Edit{Target: filepath.Join(h.Dir(), u), Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
				if doc == nil {
					return nil, nil, nil
				}
				old := string(doc)
				return nil, []claudecfg.Change{{Path: "(whole file)", Old: &old}}, nil
			}})
		}
		if _, _, err := n.editor("server delete").Apply(edits); err != nil {
			return err
		}
		if err := h.Remove(ctx); err != nil {
			return err
		}
	}
	_, task := h.(taskHost)
	remove := os.RemoveAll
	if task {
		// Windows: a file the supervisor's processes, just ended, or a
		// scanner still hold a moment refuses its removal that moment.
		remove = removeAllRetry
	}
	if err := remove(n.Root()); err != nil {
		return err
	}
	if task {
		n.dropPg0Skeleton()
	}
	return nil
}

// Status reads each installed unit's state, as Docker's container states:
// none when no unit file exists.
func (n Native) Status(ctx context.Context) ([]Container, error) {
	if n.Err != nil {
		return nil, n.Err
	}
	states, err := n.host().States(ctx)
	if err != nil {
		return nil, err
	}
	var list []Container
	for _, u := range n.units() {
		if s, ok := states[u[1]]; ok {
			list = append(list, Container{Service: u[1], State: unitState(stateParts(s))})
		}
	}
	return list, nil
}

// NotInstalled is UnitStates' state of a unit whose file does not exist.
const NotInstalled = "not-installed"

// UnitStates maps both units to systemd's ActiveState (doctor's check),
// NotInstalled for a unit without its file. Windows has no unit: its keys
// are the services, db and hindsight.
func (n Native) UnitStates(ctx context.Context) (map[string]string, error) {
	if n.Err != nil {
		return nil, n.Err
	}
	states, err := n.host().States(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, u := range n.units() {
		key := u[0]
		if n.goos() == "windows" {
			key = u[1]
		}
		out[key] = NotInstalled
		if s, ok := states[u[1]]; ok {
			out[key], _ = stateParts(s)
		}
	}
	return out, nil
}

// unitState names a unit's state as Docker names a container's.
func unitState(active, sub string) string {
	switch active {
	case "active", "reloading":
		return "running"
	case "activating":
		if sub == "auto-restart" {
			return "restarting"
		}
		return "starting"
	case "deactivating":
		return "stopping"
	case "failed":
		return "exited"
	case "inactive":
		return "stopped"
	}
	return active
}

// Logs is the journal of service's unit (db or hindsight).
func (n Native) Logs(ctx context.Context, service string, lines int) (string, error) {
	if n.Err != nil {
		return "", n.Err
	}
	return n.host().Logs(ctx, service, lines)
}

// Deployed is the version whose venv the server unit (Windows:
// supervise.json) runs, "" without that file.
func (n Native) Deployed() (string, error) {
	if n.Err != nil {
		return "", n.Err
	}
	h := n.host()
	name := h.fileNames()[0]
	path := filepath.Join(h.Dir(), name)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	v, err := h.DeployedVenv(map[string][]byte{name: raw})
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// runningVersion is the version whose venv the server unit runs, else
// Version.
func (n Native) runningVersion() string {
	if v, err := n.Deployed(); err == nil && v != "" {
		return v
	}
	return n.Version()
}

func (n Native) DataExists(context.Context) (bool, error) {
	if n.Err != nil {
		return false, n.Err
	}
	_, err := os.Lstat(filepath.Join(n.DataDir(), "PG_VERSION"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (n Native) DataName() string {
	if n.goos() == "windows" {
		return "the native database in " + n.DataDir()
	}
	return "the native database in " + n.Home()
}

var _ Sizer = Native{}

// DataSize adds up the files of pg0's instances (ruling R10): the
// database, and any instance restore moved aside.
func (n Native) DataSize(context.Context) (int64, error) {
	if n.Err != nil {
		return 0, n.Err
	}
	if n.goos() == "windows" { // <Root>\db and the db.refused-* beside it
		dirs, err := filepath.Glob(n.InstanceDir() + ".refused-*")
		if err != nil {
			return 0, err
		}
		var total int64
		for _, d := range append([]string{n.InstanceDir()}, dirs...) {
			s, err := dirSize(d)
			if err != nil {
				return 0, err
			}
			total += s
		}
		return total, nil
	}
	return dirSize(filepath.Dir(n.InstanceDir()))
}

var _ Cleaner = Native{}

// Leftover: Root() exists (a database, or only the downloads a failed
// switch left).
func (n Native) Leftover(context.Context) (bool, error) {
	if n.Err != nil {
		return false, n.Err
	}
	_, err := os.Lstat(n.Root())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// CleanWhat names what Delete removes: all of Root().
func (n Native) CleanWhat() string {
	return "the native server in " + n.Root() + ": the database, the Python environments and the models"
}

// CleanSize is Root()'s size.
func (n Native) CleanSize(context.Context) (int64, error) {
	if n.Err != nil {
		return 0, n.Err
	}
	return dirSize(n.Root())
}

// dirSize adds up the regular files under dir; none there is 0.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, de fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if de.Type().IsRegular() {
			fi, err := de.Info()
			if err != nil {
				return err
			}
			total += fi.Size()
		}
		return nil
	})
	return total, err
}

var _ Creator = Native{}

// AllowCreate is n creating its first database when there is none
// (MayCreate, ruling R35): a switch to native.
func (n Native) AllowCreate() Runtime { n.MayCreate = true; return n }

// DataRemoveCmd stops the server and deletes its database: in PowerShell
// on Windows (PowerShell 5 has no &&).
func (n Native) DataRemoveCmd() string {
	if n.goos() == "windows" {
		return "in PowerShell: " + n.StopCmd() + "; Remove-Item -Recurse -Force " + PSQuote(n.Home())
	}
	return n.StopCmd() + " && rm -rf " + shellWord(n.Home())
}

// ImagesRemoveCmd frees what the server ran: Python, the venvs, the models
// and uv.
func (n Native) ImagesRemoveCmd() string {
	dirs := []string{n.Python(), n.join(n.Root(), "venv"), n.HF(), n.join(n.Root(), "uv")}
	if n.goos() == "windows" {
		for i, d := range dirs {
			dirs[i] = PSQuote(d)
		}
		return "in PowerShell: Remove-Item -Recurse -Force " + strings.Join(dirs, ", ")
	}
	for i, d := range dirs {
		dirs[i] = shellWord(d)
	}
	return "rm -rf " + strings.Join(dirs, " ")
}

// PSQuote is s as one PowerShell word: single-quoted, a quote doubled.
func PSQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Engine is the host's service manager (systemd's: native_systemd.go).
func (n Native) Engine(ctx context.Context) (Engine, error) {
	if n.Err != nil {
		return Engine{}, n.Err
	}
	return n.host().Engine(ctx)
}

// Orchestrator is the pinned uv, once installed.
func (n Native) Orchestrator(context.Context) (string, error) {
	if n.Err != nil {
		return "", n.Err
	}
	if !n.uvInstalled() {
		return "", nil
	}
	return "uv " + nativelock.UVVersion, nil
}

// RenameBank runs the server's own hindsight-admin with the server's
// environment (its database's URL among it), nothing else.
func (n Native) RenameBank(ctx context.Context, from, to string, dryRun bool) ([]byte, error) {
	if n.Err != nil {
		return nil, n.Err
	}
	raw, err := os.ReadFile(n.EnvFile())
	if err != nil {
		return nil, fmt.Errorf("the native server's environment: %w (run backsight server render)", err)
	}
	env := append([]string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}, parseEnvFile(raw)...)
	if n.goos() == "windows" {
		env = append(n.windowsEnv(filepath.Dir(n.venvBin(n.runningVersion(), "hindsight-admin"))), parseEnvFile(raw)...)
	}
	args := []string{"rename-bank", "--from", from, "--to", to}
	if dryRun {
		args = append(args, "--dry-run")
	}
	out, err := n.Tools.Run(ctx, env, nil, n.venvBin(n.runningVersion(), "hindsight-admin"), args...)
	if err != nil {
		return out, n.redact(fmt.Errorf("hindsight-admin rename-bank: %w", err))
	}
	return out, nil
}

// SecretFiles is server.env.
func (n Native) SecretFiles() []string { return []string{n.EnvFile()} }

// createInstance has pg0 create the database once (Task 1's C2 and C4,
// ruling R21): pg0 starts it with its own default password in a transient
// unit, psql then sets the real one for hindsight and postgres from stdin,
// and pg0 stops it. No password is on any argv; pg0's temporary files go
// to a private TMPDIR under home/, removed after. ensureInstance calls it
// on a first install.
func (n Native) createInstance(ctx context.Context) error {
	if n.Err != nil {
		return n.Err
	}
	if n.goos() == "windows" {
		return n.createInstanceWindows(ctx)
	}
	pw, err := n.dbPassword()
	if err != nil {
		return err
	}
	port, err := n.envPort()
	if err != nil {
		return err
	}
	if err := n.ensureHome(); err != nil {
		return err
	}
	tmp := filepath.Join(n.Home(), "tmp")
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	pg0 := filepath.Join(n.sitePackages(n.runningVersion()), "pg0", "bin", "pg0")
	inst, p := n.installation(), strconv.Itoa(port)
	steps := [][]string{
		{pg0, "start", "--name", pgInstance, "--port", p, "-u", "hindsight", "-n", "hindsight", "-c", "fsync=on", stepNoStdin, stepNoStdout},
		{"LD_LIBRARY_PATH=" + filepath.Join(inst, "lib"), n.pgBin("psql"),
			"-h", "127.0.0.1", "-p", p, "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-q", "-f", "-"},
		{pg0, "stop", "--name", pgInstance, stepNoStdout},
	}
	env := []string{"HOME=" + n.Home(), "PGPASSWORD=postgres", "TMPDIR=" + tmp}
	// A failing statement is never logged: it would hold the password.
	stdin := "SET log_min_error_statement = panic;\nALTER ROLE hindsight PASSWORD '" + pw + "';\nALTER ROLE postgres PASSWORD '" + pw + "';\n"
	if err := n.host().Transient(ctx, n.project()+"-db-init", env, strings.NewReader(stdin), steps); err != nil {
		// The transient unit is gone with its processes (--wait,
		// KillMode=control-group): an instance left on pg0's default
		// password is removed, never kept.
		if rmErr := os.RemoveAll(filepath.Dir(n.DataDir())); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return n.redact(fmt.Errorf("creating the native database (%s): %w", n.DataDir(), err))
	}
	return nil
}
