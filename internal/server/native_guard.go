package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// The native database's guards (decision 0078, E29 requirements 1-3):
// the identity check refuses to start a missing, empty or foreign
// instance in place of the user's memory; the stale-process guard stops a
// postgres on the instance's data outside the db unit, never adopting it;
// the fsync check refuses to run the database with fsync off.

// strayWait is StopStrays' default bound (Native.StrayWait overrides it);
// strayPoll is how often it looks. Tests shorten both.
var (
	strayWait = 30 * time.Second
	strayPoll = 200 * time.Millisecond
)

func (n Native) strayWait() time.Duration {
	if n.StrayWait > 0 {
		return n.StrayWait
	}
	return strayWait
}

// InstanceDir is pg0's instance: <home>/.pg0/instances/hindsight.
func (n Native) InstanceDir() string {
	return filepath.Join(n.Home(), ".pg0", "instances", pgInstance)
}

// GuardError is the identity check's refusal: the instance is missing,
// empty, another cluster than the one recorded (identity), or has no
// recorded identity (unrecorded).
type GuardError struct {
	Reason    string // missing | empty | identity | unrecorded
	Instance  string // InstanceDir()
	Want, Got string // the recorded identifier, the instance's
	File      string // IdentityFile()
}

func (e *GuardError) Error() string {
	switch e.Reason {
	case "identity":
		return "the native database " + e.Instance + " is not the one backsight recorded (identifier " + e.Got + ", recorded " + e.Want +
			"): refusing to start it. Put the right one back, or restore a backup: backsight restore <archive>"
	case "unrecorded":
		return "the native database " + e.Instance + " has no recorded identity (" + e.File +
			" is missing): refusing to start it. Restore a backup to adopt one: backsight restore <archive>"
	}
	return "the native database " + e.Instance + " is " + e.Reason +
		": refusing to start an empty memory in its place. Put it back, or restore a backup: backsight restore <archive>"
}

// toolEnv is a bundled Postgres tool's whole environment, no password.
func (n Native) toolEnv() []string {
	return []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=" + n.Home(),
		"LD_LIBRARY_PATH=" + filepath.Join(n.installation(), "lib")}
}

// ReadIdentity is the instance's "Database system identifier", as the
// bundled pg_controldata prints it.
func (n Native) ReadIdentity(ctx context.Context) (string, error) {
	if n.Err != nil {
		return "", n.Err
	}
	out, err := n.Tools.Run(ctx, n.toolEnv(), nil, filepath.Join(n.installation(), "bin", "pg_controldata"), "-D", n.DataDir())
	if err != nil {
		return "", n.redact(fmt.Errorf("reading the native database's identity (pg_controldata -D %s): %w", n.DataDir(), err))
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "Database system identifier:"); ok {
			if id := strings.TrimSpace(v); id != "" && isDigits(id) {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("pg_controldata -D %s printed no database system identifier", n.DataDir())
}

func isDigits(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// RecordIdentity writes the instance's identifier to IdentityFile (0600):
// at the first start and after a restore into the instance.
func (n Native) RecordIdentity(ctx context.Context) error {
	id, err := n.ReadIdentity(ctx)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(n.IdentityFile(), []byte(id+"\n"), 0o600)
}

// recorded is IdentityFile's identifier, and whether the file exists.
func (n Native) recorded() (string, bool, error) {
	raw, err := os.ReadFile(n.IdentityFile())
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(raw)), true, nil
}

// CheckInstance is requirement 2: nil for a first install (no identity
// file, no instance) and for the recorded instance, else a *GuardError. A
// missing identity file beside an instance is a refusal, never a fresh
// start.
func (n Native) CheckInstance(ctx context.Context) error {
	if n.Err != nil {
		return n.Err
	}
	want, recorded, err := n.recorded()
	if err != nil {
		return err
	}
	refuse := func(reason, got string) error {
		return &GuardError{Reason: reason, Instance: n.InstanceDir(), Want: want, Got: got, File: n.IdentityFile()}
	}
	if _, err := os.Lstat(n.InstanceDir()); errors.Is(err, fs.ErrNotExist) {
		if !recorded {
			return nil
		}
		return refuse("missing", "")
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(n.DataDir(), "PG_VERSION")); errors.Is(err, fs.ErrNotExist) {
		return refuse("empty", "")
	} else if err != nil {
		return err
	}
	if !recorded {
		return refuse("unrecorded", "")
	}
	got, err := n.ReadIdentity(ctx)
	if err != nil {
		return err
	}
	if got != want {
		return refuse("identity", got)
	}
	return nil
}

// absent: path does not exist (ENOENT); any other error is returned.
func absent(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return false, err
}

// ensureInstance creates the database on a first install (no instance
// and no identity file: ruling R29), only where creating is allowed
// (MayCreate, ruling R35), and records its identity before the db unit
// ever starts on it (Task 1's C2). Without MayCreate, nothing there is the
// missing refusal. Any other case is CheckInstance's to judge: a lost
// instance is never replaced by an empty one.
func (n Native) ensureInstance(ctx context.Context) error {
	noInstance, err := absent(n.InstanceDir())
	if err != nil {
		return err
	}
	noIdentity, err := absent(n.IdentityFile())
	if err != nil {
		return err
	}
	if !noInstance || !noIdentity {
		return nil
	}
	if !n.MayCreate {
		return &GuardError{Reason: "missing", Instance: n.InstanceDir(), File: n.IdentityFile()}
	}
	if err := n.createInstance(ctx); err != nil {
		return err
	}
	if err := n.RecordIdentity(ctx); err != nil {
		// A fresh, empty instance without its identity would be refused
		// as unrecorded at every start: remove it, the next start creates
		// it again.
		if rmErr := os.RemoveAll(n.InstanceDir()); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return fmt.Errorf("recording the new native database's identity: %w", err)
	}
	return nil
}

// guardStart is what Start and StartDatabase do before the db unit: the
// first install's creation, then the identity check, so a refusal names
// the instance before anything is enabled.
func (n Native) guardStart(ctx context.Context) error {
	if err := n.ensureInstance(ctx); err != nil {
		return err
	}
	return n.CheckInstance(ctx)
}

// Stray is a postgres on the instance's data directory outside the db
// unit: its pid and its cgroup.
type Stray struct {
	PID    int
	Cgroup string
}

func (n Native) proc() string {
	if n.Proc == "" {
		return "/proc"
	}
	return n.Proc
}

// dataDirs are the spellings of DataDir a process may show: as configured
// and with its symlinks resolved (a process's cwd is the resolved one).
func (n Native) dataDirs() []string {
	out := []string{filepath.Clean(n.DataDir())}
	if r, err := filepath.EvalSymlinks(n.DataDir()); err == nil && r != out[0] {
		out = append(out, r)
	}
	return out
}

// Strays are requirement 3's processes: postmasters (argv[0] is postgres)
// whose -D, or whose working directory, is DataDir, and whose cgroup does
// not end in /<project>-db.service. A process that ends while it is read
// is left out.
func (n Native) Strays() ([]Stray, error) {
	if n.Err != nil {
		return nil, n.Err
	}
	entries, err := os.ReadDir(n.proc())
	if err != nil {
		return nil, err
	}
	data := n.dataDirs()
	var out []Stray
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || !n.postmasterOn(pid, data) {
			continue
		}
		cg, ok := n.cgroupOf(pid)
		if !ok || strings.HasSuffix(cg, "/"+n.DBUnit()) {
			continue
		}
		out = append(out, Stray{PID: pid, Cgroup: cg})
	}
	slices.SortFunc(out, func(a, b Stray) int { return a.PID - b.PID })
	return out, nil
}

// postmasterOn: pid is a postgres server (not one of its children, whose
// title is "postgres: …") on one of data.
func (n Native) postmasterOn(pid int, data []string) bool {
	dir := filepath.Join(n.proc(), strconv.Itoa(pid))
	raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return false
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(argv) == 0 || filepath.Base(argv[0]) != "postgres" {
		return false
	}
	cwd, _ := os.Readlink(filepath.Join(dir, "cwd"))
	on := func(p string) bool {
		if p == "" {
			return false
		}
		if !filepath.IsAbs(p) {
			if cwd == "" {
				return false
			}
			p = filepath.Join(cwd, p)
		}
		return slices.Contains(data, filepath.Clean(p))
	}
	for i, a := range argv[1:] {
		switch {
		case a == "-D" && i+2 < len(argv):
			if on(argv[i+2]) {
				return true
			}
		case strings.HasPrefix(a, "-D") && len(a) > 2:
			if on(a[2:]) {
				return true
			}
		}
	}
	return cwd != "" && on(cwd)
}

// cgroupOf is pid's cgroup (v2's 0:: line, else v1's name=systemd), false
// when the process is gone.
func (n Native) cgroupOf(pid int) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(n.proc(), strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", false
	}
	v1 := ""
	for _, l := range strings.Split(string(raw), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			return p, true
		}
		if _, p, ok := strings.Cut(l, ":name=systemd:"); ok {
			v1 = p
		}
	}
	return v1, true
}

// StopStrays stops the strays with the bundled pg_ctl (a fast shutdown,
// ruling R22: a failed pg0 start deletes the instance.json pg0 stop
// needs), then waits for each to be gone; one still running after
// strayWait is an error naming it. Never adopted.
func (n Native) StopStrays(ctx context.Context, strays []Stray) error {
	if len(strays) == 0 {
		return nil
	}
	if n.Err != nil {
		return n.Err
	}
	if err := n.ownsPidFile(strays); err != nil {
		return err
	}
	wait := n.strayWait()
	deadline := time.Now().Add(wait)
	secs := max(1, int(wait/time.Second))
	_, runErr := n.Tools.Run(ctx, n.toolEnv(), nil, filepath.Join(n.installation(), "bin", "pg_ctl"),
		"stop", "-D", n.DataDir(), "-m", "fast", "-w", "-t", strconv.Itoa(secs))
	data := n.dataDirs()
	for {
		var alive []string
		for _, s := range strays {
			if n.postmasterOn(s.PID, data) {
				alive = append(alive, strconv.Itoa(s.PID))
			}
		}
		if len(alive) == 0 {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			err := fmt.Errorf("a postgres still runs on %s outside %s after %s (pid %s): stop it (kill -INT %s), then start again",
				n.DataDir(), n.DBUnit(), wait, strings.Join(alive, ", "), strings.Join(alive, " "))
			if runErr != nil {
				err = fmt.Errorf("%w; pg_ctl stop: %w", err, runErr)
			}
			return n.redact(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(strayPoll):
		}
	}
}

// ownsPidFile: pg_ctl stop -D stops the pid DataDir's postmaster.pid
// names, so it may run only when that is the stray's own (ruling R36):
// otherwise it could stop the db unit's postgres. Anything else refuses,
// stopping nothing.
func (n Native) ownsPidFile(strays []Stray) error {
	pidFile := filepath.Join(n.DataDir(), "postmaster.pid")
	raw, err := os.ReadFile(pidFile)
	owner := ""
	if err == nil {
		first, _, _ := strings.Cut(string(raw), "\n")
		owner = strings.TrimSpace(first)
	}
	var others []string
	for _, s := range strays {
		if strconv.Itoa(s.PID) != owner {
			others = append(others, strconv.Itoa(s.PID))
		}
	}
	if len(others) == 0 {
		return nil
	}
	names := "pid " + owner
	if owner == "" {
		names = "no pid"
	}
	return fmt.Errorf("a postgres (pid %s) runs on %s outside %s, but %s names %s: refusing to stop anything with pg_ctl, which stops the pid that file names. Stop pid %s yourself (kill -INT %s), then start again",
		strings.Join(others, ", "), n.DataDir(), n.DBUnit(), pidFile, names, strings.Join(others, ", "), strings.Join(others, " "))
}

// CheckFsync is requirement 1: the running database's fsync must read on.
func (n Native) CheckFsync(ctx context.Context) error {
	if n.Err != nil {
		return n.Err
	}
	pw, err := n.dbPassword()
	if err != nil {
		return err
	}
	port, err := n.envPort()
	if err != nil {
		return err
	}
	out, err := n.Tools.Run(ctx, n.pgEnv(pw), nil, filepath.Join(n.installation(), "bin", "psql"), "-h", "127.0.0.1", "-p", strconv.Itoa(port),
		"-U", "hindsight", "-d", "hindsight", "-At", "-v", "ON_ERROR_STOP=1", "-c", "SHOW fsync")
	if err != nil {
		return n.redact(fmt.Errorf("checking fsync on the native database (%s): %w", n.DBUnit(), err))
	}
	switch v := strings.TrimSpace(string(out)); v {
	case "on":
		return nil
	case "off":
		return errors.New("fsync is off on the native database: refusing to run it (an OS crash could corrupt it); check " + n.DBUnit() + "'s settings")
	default:
		return fmt.Errorf("fsync reads %q on the native database, not on: refusing to run it (an OS crash could corrupt it); check %s's settings", v, n.DBUnit())
	}
}

// fsyncOrStop checks fsync after a start; anything but on stops and
// disables both units (nothing starts at boot on it).
func (n Native) fsyncOrStop(ctx context.Context) error {
	err := n.CheckFsync(ctx)
	if err == nil {
		return nil
	}
	if stopErr := n.Stop(ctx); stopErr != nil {
		return errors.Join(err, stopErr)
	}
	return err
}
