package server

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/state"
)

// The native runtime's database tools (decision 0078, requirement 5):
// pg0's bundled psql, pg_dump, pg_restore, dropdb and createdb, with their
// lib on LD_LIBRARY_PATH, reaching the database over 127.0.0.1 as
// hindsight. The password is secrets.toml's db_password (ruling R11), in
// PGPASSWORD only: nothing on their argv but options, names and file
// paths. The dump is the custom format Docker writes, so archives cross
// runtimes.

// NativePostgres is what the native runtime's database runs, as a backup's
// manifest names it (its postgres_image on Docker).
const NativePostgres = "pg0-embedded " + nativelock.PG0 + " (" + nativelock.Postgres + ")"

// Database is the native server's Postgres.
func (n Native) Database() Database { return NativeDB{n: n} }

// NativeDB is the native runtime's Postgres. port and password name
// another instance than the server's (VerifyDump's throwaway one); zero,
// they are server.env's port and secrets.toml's db_password.
type NativeDB struct {
	n        Native
	port     int
	password string
}

var _ Database = NativeDB{}

// tool is a bundled Postgres program.
func (d NativeDB) tool(name string) string { return filepath.Join(d.n.installation(), "bin", name) }

// env is a bundled client's whole environment: where the database is,
// who connects, and the password, never on argv.
func (d NativeDB) env() ([]string, error) {
	if d.n.Err != nil {
		return nil, d.n.Err
	}
	port, pw := d.port, d.password
	if port == 0 {
		p, err := d.n.envPort()
		if err != nil {
			return nil, err
		}
		port = p
	}
	if pw == "" {
		p, err := d.n.dbPassword()
		if err != nil {
			return nil, err
		}
		pw = p
	}
	return append(d.n.pgEnv(pw), "PGHOST=127.0.0.1", "PGPORT="+strconv.Itoa(port), "PGUSER="+dbUser), nil
}

// run runs a bundled tool; its error names the tool and carries its
// stderr, redacted.
func (d NativeDB) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	env, err := d.env()
	if err != nil {
		return nil, err
	}
	out, err := d.n.Tools.Run(ctx, env, nil, d.tool(name), args...)
	if err != nil {
		return out, d.n.redact(fmt.Errorf("%s: %w", name, err))
	}
	return out, nil
}

// Query runs one statement and returns its single value.
func (d NativeDB) Query(ctx context.Context, sql string) (string, error) {
	out, err := d.run(ctx, "psql", "-d", dbName, "-At", "-v", "ON_ERROR_STOP=1", "-c", sql)
	return strings.TrimSpace(string(out)), err
}

// Dump writes a custom-format dump to dst, checks that it lists, and
// returns the rows of tables counted in the dump itself, as Docker's DB
// does. Each table's data is written next to dst (the caller's private
// work directory, which already holds the dump in clear) and counted as it
// is read, never held in memory, then removed.
func (d NativeDB) Dump(ctx context.Context, dst string, tables []string) (map[string]int64, error) {
	if _, err := d.run(ctx, "pg_dump", "-d", dbName, "-Fc", "-f", dst); err != nil {
		return nil, err
	}
	if _, err := d.run(ctx, "pg_restore", "--list", dst); err != nil {
		return nil, fmt.Errorf("the dump does not list: %w", err)
	}
	counts := map[string]int64{}
	for _, t := range tables {
		data := filepath.Join(filepath.Dir(dst), ".count-"+t+".sql")
		_, err := d.run(ctx, "pg_restore", "--data-only", "-t", t, "-f", data, dst)
		var n int64
		if err == nil {
			n, err = copyRows(data)
		}
		_ = os.Remove(data)
		if err != nil {
			return nil, fmt.Errorf("counting %s in the dump: %w", t, err)
		}
		counts[t] = n
	}
	return counts, nil
}

// copyRows counts the rows of the COPY blocks of a plain SQL script: the
// lines between "COPY …" and "\." (dbdump.go's awk).
func copyRows(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024*1024) // a row may be long
	var n int64
	in := false
	for sc.Scan() {
		l := sc.Text()
		switch {
		case !in && strings.HasPrefix(l, "COPY "):
			in = true
		case in && l == `\.`:
			in = false
		case in:
			n++
		}
	}
	return n, sc.Err()
}

// Restore replaces the database with the dump at src, as Docker's DB
// does: dropped (connections cut), created empty, the dump restored into
// it. dropped reports that the old one was dropped.
func (d NativeDB) Restore(ctx context.Context, src string) (dropped bool, err error) {
	// From the drop on the database is not what it was, even when the
	// command reports a failure.
	if _, err := d.run(ctx, "dropdb", "--maintenance-db=postgres", "--if-exists", "--force", dbName); err != nil {
		return true, err
	}
	if _, err := d.run(ctx, "createdb", "--maintenance-db=postgres", dbName); err != nil {
		return true, err
	}
	_, err = d.run(ctx, "pg_restore", "-d", dbName, "--no-owner", "--exit-on-error", src)
	return true, err
}

// RemoveStaleDumps has nothing to remove: the native dumps are written
// straight where the caller asked, never copied elsewhere.
func (NativeDB) RemoveStaleDumps(context.Context) error { return nil }

func (d NativeDB) Counts(ctx context.Context, tables []string) (map[string]int64, error) {
	return counts(ctx, d.Query, tables)
}

func (d NativeDB) DocumentMetadata(ctx context.Context, bank, id string) (map[string]string, error) {
	return documentMetadata(ctx, d.Query, bank, id)
}

func (d NativeDB) BankIDs(ctx context.Context) ([]string, error) { return bankIDs(ctx, d.Query) }

func (d NativeDB) PendingOperations(ctx context.Context, banks []string) (int, error) {
	return pendingOperations(ctx, d.Query, banks)
}

// verifyWait bounds how long VerifyDump waits for its throwaway database,
// verifyPoll how often it asks; verifyMaxSec is the throwaway unit's
// lifetime, so that a killed backsight leaves nothing running beyond it.
var (
	verifyWait = 90 * time.Second
	verifyPoll = 500 * time.Millisecond
)

const verifyMaxSec = 3600

// VerifyDump restores dump into a throwaway Postgres (pg0's own binaries,
// a new cluster under <state>/staging/verify-<id>, 0700, on a free
// loopback port, with a password of its own) and counts the rows of
// tables. The cluster runs in a transient user unit that systemd kills
// after an hour at most; the unit is stopped and the directory removed
// whatever happens (an error, a cancel). The directory holds a lock
// beside it, as backup verify's own work directory does, so that a
// concurrent backup leaves it alone and a killed one's is removed.
func (n Native) VerifyDump(ctx context.Context, dump string, tables []string) (map[string]int64, error) {
	if n.Err != nil {
		return nil, n.Err
	}
	staging := filepath.Join(n.Dirs.State, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, err
	}
	id := randHex()
	home := filepath.Join(staging, "verify-"+id)
	unit := n.project() + "-verify-" + id + ".service"
	l, err := state.TryAcquire(home+".lock", "backup verify")
	if err != nil {
		return nil, err
	}
	started := false
	defer func() { // registered first: a cancel must clean up too
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if started {
			_, _ = n.systemctl(cctx, "stop", unit)
		}
		_ = os.RemoveAll(home)
		_ = l.Release()
	}()
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, err
	}
	// The throwaway's own password: the real one is never written to disk
	// here, and the file goes as soon as initdb has read it.
	pw := "verify-" + randHex() + randHex()
	pwFile := filepath.Join(home, "pwfile")
	if err := os.WriteFile(pwFile, []byte(pw+"\n"), 0o600); err != nil {
		return nil, err
	}
	port, err := n.verifyPort()
	if err != nil {
		return nil, err
	}
	inst, data := n.installation(), filepath.Join(home, "data")
	script := shellWord(filepath.Join(inst, "bin", "initdb")) + " -D " + shellWord(data) + " -U " + dbUser +
		" --pwfile=" + shellWord(pwFile) + " -A scram-sha-256 -E UTF8 --no-locale >/dev/null && rm -f " + shellWord(pwFile) +
		" && exec " + shellWord(filepath.Join(inst, "bin", "postgres")) + " -D " + shellWord(data) + " -p " + strconv.Itoa(port) +
		" -c listen_addresses=127.0.0.1 -c unix_socket_directories="
	// The service manager expands ${VAR} and $VAR in a transient unit's
	// command line, as in ExecStart=, and turns $$ into $ (systemd-run(1),
	// --expand-environment, on by default): every $ is doubled so the
	// script reaches sh as written. It uses no variable today; a $ in a
	// path would otherwise be expanded away.
	args := []string{"--user", "--collect", "--quiet", "--unit", unit, "-p", "KillMode=control-group",
		"-p", "RuntimeMaxSec=" + strconv.Itoa(verifyMaxSec), "-p", "UnsetEnvironment=" + unsetKeys,
		"-E", "HOME=" + home, "-E", "LD_LIBRARY_PATH=" + filepath.Join(inst, "lib"), "-E", "PATH=/usr/bin:/bin", "-E", "LANG=C.UTF-8",
		"/bin/sh", "-c", strings.ReplaceAll(script, "$", "$$")}
	started = true // a failed systemd-run may still have started the unit
	if _, err := n.Tools.Run(ctx, systemdRunEnv(), nil, "systemd-run", args...); err != nil {
		return nil, n.redact(fmt.Errorf("starting a throwaway database (%s): %w", unit, err))
	}
	db := NativeDB{n: n, port: port, password: pw}
	if err := n.waitVerify(ctx, db, unit); err != nil {
		return nil, err
	}
	if _, err := db.Restore(ctx, dump); err != nil {
		return nil, fmt.Errorf("restoring: %w", err)
	}
	return db.Counts(ctx, tables)
}

// verifyPort is a free loopback port for the throwaway database, never
// the API's, the UI's or the server's database's.
func (n Native) verifyPort() (int, error) {
	api, err := n.Cfg.APIPort()
	if err != nil {
		return 0, err
	}
	skip := []int{api, n.Cfg.Server.UIPort}
	if pg, err := n.envPort(); err == nil {
		skip = append(skip, pg)
	}
	return freePort(api+2, skip...)
}

// waitVerify waits until the throwaway database answers, verifyWait at
// most, on its postgres database (hindsight is created by the restore); a
// unit that is gone (initdb or postgres failed) ends the wait.
func (n Native) waitVerify(ctx context.Context, db NativeDB, unit string) error {
	deadline := time.Now().Add(verifyWait)
	for {
		_, err := db.run(ctx, "psql", "-d", "postgres", "-At", "-v", "ON_ERROR_STOP=1", "-c", "select 1")
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if out, _ := n.systemctl(ctx, "is-active", unit); strings.TrimSpace(string(out)) == "failed" || strings.TrimSpace(string(out)) == "inactive" {
			return fmt.Errorf("the throwaway database stopped (%s): see journalctl --user -u %s", unit, unit)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the throwaway database (%s) did not answer within %s: %w", unit, verifyWait, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(verifyPoll):
		}
	}
}

// systemdRunEnv is systemd-run's own environment: it reaches the user
// manager through the session's bus.
func systemdRunEnv() []string {
	env := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	for _, k := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}
