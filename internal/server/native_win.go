package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/server/nativelock"
)

// The database's first install on Windows (decision 0080, E34 W4 and W5):
// pg0 ignores HOME and USERPROFILE there and keeps its installation and
// instances in the user's own %USERPROFILE%\.pg0, so it only extracts
// Postgres and creates the cluster, in <Root>\db; backsight then moves
// the installation to <Root>\pg\<version>, runs that copy, and forgets
// pg0's instance. Nothing backsight depends on stays under .pg0.

// pg0Home is pg0's own directory on Windows, %USERPROFILE%\.pg0 of the
// user's profile (native_windows.go); tests use another.
var pg0Home = userPg0Home

// dropPg0Skeleton removes what the first install left in pg0's own
// directory once its installation moved and its instance was forgotten:
// installation\, instances\ and .pg0 itself, each only when empty (a
// pg0 of the user's own keeps everything of it). Delete's last step.
func (n Native) dropPg0Skeleton() {
	dir, err := pg0Home()
	if err != nil {
		return
	}
	for _, d := range []string{n.join(dir, "installation"), n.join(dir, "instances"), dir} {
		if ents, err := os.ReadDir(d); err == nil && len(ents) == 0 {
			_ = os.Remove(d)
		}
	}
}

// renameDir moves a directory; tests make it fail as across volumes.
var renameDir = renameRetry

// osRename is os.Rename; tests make it fail.
var osRename = os.Rename

// renameRetryWait bounds renameRetry.
var renameRetryWait = 10 * time.Second

// renameRetry is os.Rename, retried for a few seconds while it is denied:
// on Windows a file just closed by a job's process, or held a moment by
// an indexer or an antivirus, refuses a move of its directory.
func renameRetry(from, to string) error {
	deadline := time.Now().Add(renameRetryWait)
	for {
		err := osRename(from, to)
		if err == nil || !errors.Is(err, fs.ErrPermission) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// partialPrefix is the suffix's start of a copy in progress beside its
// destination (copyPostgres): a killed copy's is removed at the next.
const partialPrefix = ".partial-"

// copyPostgres moves pg0's extracted installation under pg0dir to
// installation() (E34 ruling 8): a rename on the same volume, else a copy
// and the original removed (pg0 extracts again if it ever needs it). A
// complete installation with its marker is kept as it is. The caller
// copies the runtime DLLs into it, then marks it complete.
func (n Native) copyPostgres(pg0dir string) error {
	dst := n.installation()
	if n.pgMissing() == "" && isRegular(n.join(dst, pgUnpacked)) {
		return nil
	}
	src := n.join(pg0dir, "installation", nativelock.PGInstallation)
	if m := n.pgMissingIn(src); m != "" {
		return fmt.Errorf("pg0 ran, but the %s it extracts is incomplete: %s is missing", NativePostgres, m)
	}
	if err := os.RemoveAll(dst); err != nil { // a partial one
		return err
	}
	stale, _ := filepath.Glob(dst + partialPrefix + "*") // a killed copy's
	for _, d := range stale {
		if err := removeAllRetry(d); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := renameDir(src, dst); err != nil {
		if !crossVolume(err) {
			return fmt.Errorf("moving pg0's %s to %s: %w", NativePostgres, dst, err)
		}
		// Into a sibling first: a copy cut midway is never dst, which a
		// later run would take for complete.
		staging := dst + partialPrefix + randHex()
		if err := copyTree(src, staging); err != nil {
			_ = removeAllRetry(staging)
			return fmt.Errorf("copying pg0's %s to %s: %w", NativePostgres, dst, err)
		}
		if err := renameRetry(staging, dst); err != nil {
			_ = removeAllRetry(staging)
			return fmt.Errorf("copying pg0's %s to %s: %w", NativePostgres, dst, err)
		}
		if err := os.RemoveAll(src); err != nil {
			return err
		}
	}
	if m := n.pgMissing(); m != "" {
		return fmt.Errorf("the %s moved to %s is incomplete: %s is missing", NativePostgres, dst, m)
	}
	return nil
}

// copyFile copies one file of copyTree; tests make it fail midway.
var copyFile = func(out io.Writer, in io.Reader) error {
	_, err := io.Copy(out, in)
	return err
}

// copyTree copies the directory src to dst, which must not exist: files
// with their mode, directories 0755.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		if err := copyFile(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}

// markInstallation copies the runtime DLLs into the installation's bin\
// (version v's venv holds them), then marks it complete: an installation
// whose programs cannot load their DLLs is never complete.
func (n Native) markInstallation(v string) error {
	if err := n.copyRuntimeDLLs(v, n.join(n.installation(), "bin")); err != nil {
		return err
	}
	return fsutil.WriteAtomic(n.join(n.installation(), pgUnpacked), []byte(nativelock.PG0+"\n"), 0o644)
}

// unpackPostgresWindows is unpackPostgres on Windows: pg0 starts and
// drops a throwaway of its own, <project>-unpack, on <Root>\tmp\unpack (pg0
// extracts its Postgres at a first start only), in a transient job;
// copyPostgres then moves the installation under Root(). The throwaway's
// data and pg0's entry for it are removed whatever happens.
func (n Native) unpackPostgresWindows(ctx context.Context, w io.Writer, v string) (err error) {
	if n.pgMissing() == "" {
		if isRegular(n.join(n.installation(), pgUnpacked)) {
			return nil
		}
		return n.markInstallation(v)
	}
	fmt.Fprintf(w, "Unpacking %s\n", NativeUnpack)
	if err := n.ensureRoot(); err != nil {
		return err
	}
	pg0dir, err := pg0Home()
	if err != nil {
		return err
	}
	port, err := n.verifyPort()
	if err != nil {
		return err
	}
	name := n.project() + "-unpack"
	data := n.join(n.Root(), "tmp", "unpack")
	throwaway := n.join(pg0dir, "instances", name)
	extracted := n.join(pg0dir, "installation", nativelock.PGInstallation)
	stale := []string{data, throwaway, n.installation()} // a killed run's, a partial copy
	if n.pgMissingIn(extracted) != "" {
		stale = append(stale, extracted) // a cut extraction: pg0 extracts only where there is none
	}
	for _, d := range stale {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
	}
	if err := fsutil.MkdirPrivate(filepath.Dir(data)); err != nil {
		return err
	}
	defer func() {
		for _, d := range []string{data, throwaway} {
			if rmErr := removeAllRetry(d); rmErr != nil {
				err = errors.Join(err, rmErr)
			}
		}
	}()
	pg0 := n.pg0Exe(v)
	steps := [][]string{
		{pg0, "start", "--name", name, "--data-dir", data, "--port", strconv.Itoa(port), "-c", "listen_addresses=127.0.0.1", stepToLog},
		{pg0, "drop", "--force", "--name", name, stepToLog},
	}
	if err := n.host().Transient(ctx, name, n.toolEnvFor(filepath.Dir(pg0)), nil, steps); err != nil {
		return fmt.Errorf("unpacking %s with pg0 into %s: %w", NativePostgres, n.installation(), err)
	}
	if err := n.copyPostgres(pg0dir); err != nil {
		return err
	}
	return n.markInstallation(v)
}

// createInstanceWindows is createInstance on Windows (E34 W4): pg0 creates
// and starts the cluster in DataDir() with its default password, psql
// sets the real one for hindsight and postgres from stdin, pg0 stops it,
// all in one transient job; then pg0's installation is moved under Root()
// and its instance entry deleted (never pg0 drop, which would delete the
// data). No password is on any argv. A failure removes the new database.
func (n Native) createInstanceWindows(ctx context.Context) error {
	pw, err := n.dbPassword()
	if err != nil {
		return err
	}
	port, err := n.envPort()
	if err != nil {
		return err
	}
	if err := n.ensureRoot(); err != nil {
		return err
	}
	pg0dir, err := pg0Home()
	if err != nil {
		return err
	}
	v := n.runningVersion()
	pg0, p := n.pg0Exe(v), strconv.Itoa(port)
	inst := n.join(pg0dir, "instances", n.project())
	// psql is the copy's when Root() has it (Fetch's unpack), else the one
	// pg0's start extracts, which finds the runtime DLLs in pg0's bin\ on
	// the job's PATH.
	psql := []string{pathOf(n.toolEnvFor(n.join(n.installation(), "bin"))), n.pgBin("psql")}
	if n.pgMissing() != "" {
		psql = []string{n.join(pg0dir, "installation", nativelock.PGInstallation, "bin", "psql.exe")}
	}
	steps := [][]string{
		{pg0, "start", "--name", n.project(), "--data-dir", n.DataDir(), "--port", p, "-u", "hindsight", "-n", "hindsight", "-c", "fsync=on", stepToLog},
		append(psql, "-h", "127.0.0.1", "-p", p, "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-q", "-f", "-"),
		{pg0, "stop", "--name", n.project(), stepToLog},
	}
	env := append(n.toolEnvFor(filepath.Dir(pg0)), "PGPASSWORD=postgres")
	// A failing statement is never logged: it would hold the password.
	stdin := "SET log_min_error_statement = panic;\nALTER ROLE hindsight PASSWORD '" + pw + "';\nALTER ROLE postgres PASSWORD '" + pw + "';\n"
	err = n.host().Transient(ctx, n.project()+"-db-init", env, strings.NewReader(stdin), steps)
	if err == nil {
		err = n.finishInstance(pg0dir, inst, v)
	}
	if err != nil {
		// The job's processes died with the call. A database without its
		// installation, or still on pg0's default password, is removed,
		// never kept: ensureInstance would refuse it for ever, having
		// recorded no identity for it.
		for _, d := range []string{n.DataDir(), inst} {
			if rmErr := removeAllRetry(d); rmErr != nil {
				err = errors.Join(err, rmErr)
			}
		}
		return n.redact(fmt.Errorf("creating the native database (%s): %w", n.DataDir(), err))
	}
	return nil
}

// finishInstance is the first install's end, once pg0 created the
// database: its installation moved under Root(), its instance entry
// forgotten, the installation's DLLs and marker.
func (n Native) finishInstance(pg0dir, inst, v string) error {
	if err := n.copyPostgres(pg0dir); err != nil {
		return err
	}
	if err := os.RemoveAll(inst); err != nil {
		return fmt.Errorf("forgetting pg0's instance %s: %w", inst, err)
	}
	n.dropExtracted(pg0dir)
	return n.markInstallation(v)
}

// dropExtracted removes the installation pg0 extracted again for the
// first start when Root() already had its copy (copyPostgres kept it):
// moved aside first, so that pg0 never sees a half-removed one, and left
// where it is if pg0 runs from it elsewhere (a file in use). pg0 extracts
// again if it ever needs it. Aside is under Root()'s tmp\, else, when
// Root() is on another volume than the user's profile (BACKSIGHT_HOME on
// D:, which no rename reaches), beside it in pg0's installation\.
func (n Native) dropExtracted(pg0dir string) {
	src := n.join(pg0dir, "installation", nativelock.PGInstallation)
	for _, aside := range []string{n.join(n.Root(), "tmp", "pg0-installation-"+randHex()), n.join(pg0dir, "installation", ".backsight-drop-"+randHex())} {
		err := renameDir(src, aside)
		if err == nil {
			_ = removeAllRetry(aside)
			return
		}
		if !crossVolume(err) {
			return
		}
	}
}

// pathOf is env's PATH=… entry.
func pathOf(env []string) string {
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			return e
		}
	}
	return "PATH="
}

// winProc is a postgres.exe of <Root>\pg\ as the stray scan saw it.
type winProc struct {
	pid, parent uint32
	created     int64 // its creation time (FILETIME), 0 when unknown
}

// strayProcs are the strays among procs (decision 0080): each that skip
// does not keep (the supervisor's, a backup verify's) and that is no
// backend, a process whose parent is one of procs and older than it (a
// parent's pid reused by a newer process is no parent).
func strayProcs(procs []winProc, skip func(pid uint32) bool) []uint32 {
	byPID := map[uint32]winProc{}
	for _, p := range procs {
		byPID[p.pid] = p
	}
	var out []uint32
	for _, p := range procs {
		if parent, ok := byPID[p.parent]; ok && parent.pid != p.pid && parent.created != 0 && p.created != 0 && parent.created <= p.created {
			continue // a backend: its postmaster is the stray
		}
		if skip(p.pid) {
			continue
		}
		out = append(out, p.pid)
	}
	return out
}

// verifyJobSuffix is VerifyDump id's Job Object's suffix (supervise's
// named job): the stray scan skips its processes while it runs.
func verifyJobSuffix(id string) string { return "verify-" + id }
