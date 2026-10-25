package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Exec runs a command in a service's container, without a TTY.
func (c Compose) Exec(ctx context.Context, service string, cmd ...string) ([]byte, error) {
	return c.run(ctx, append([]string{"exec", "-T", service}, cmd...)...)
}

// DB is the server's Postgres, reached through its container (backups).
type DB struct{ Compose Compose }

const dbUser, dbName = "hindsight", "hindsight"

// cleanupTimeout bounds a cleanup run after a failure or a cancel.
const cleanupTimeout = 20 * time.Second

// Query runs one statement and returns its single value, from stdout only.
func (d DB) Query(ctx context.Context, sql string) (string, error) {
	out, err := d.Compose.stdout(ctx, "exec", "-T", "db", "psql", "-U", dbUser, "-d", dbName, "-At", "-v", "ON_ERROR_STOP=1", "-c", sql)
	return strings.TrimSpace(string(out)), err
}

// Dump writes a custom-format dump to dst and returns the rows of tables
// counted from the dump itself (one snapshot: rows written by the server
// meanwhile cannot make verify disagree). The dump is made into a file
// inside the container (binary data never goes through a pipe that mixes
// stderr), listed by pg_restore as a check, counted, copied out, removed.
func (d DB) Dump(ctx context.Context, dst string, tables []string) (map[string]int64, error) {
	tmp := "/tmp/backsight-" + randHex() + ".dump"
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_, _ = d.Compose.Exec(cctx, "db", "rm", "-f", tmp)
	}()
	if _, err := d.Compose.Exec(ctx, "db", "pg_dump", "-U", dbUser, "-d", dbName, "-Fc", "-f", tmp); err != nil {
		return nil, err
	}
	if _, err := d.Compose.Exec(ctx, "db", "pg_restore", "--list", tmp); err != nil {
		return nil, fmt.Errorf("the dump does not list: %w", err)
	}
	counts := map[string]int64{}
	for _, t := range tables {
		// The table's COPY block, from the dump: rows between "COPY …" and "\.".
		script := "pg_restore --data-only -t " + t + " -f - " + tmp + ` | awk '/^COPY /{c=1;next} /^\\\.$/{c=0} c{n++} END{print n+0}'`
		out, err := d.Compose.stdout(ctx, "exec", "-T", "db", "sh", "-c", script)
		if err != nil {
			return nil, fmt.Errorf("counting %s in the dump: %w", t, err)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("counting %s in the dump: %q", t, out)
		}
		counts[t] = n
	}
	_, err := d.Compose.run(ctx, "cp", "db:"+tmp, dst)
	return counts, err
}

// Restore replaces the database with the dump at src, the API stopped: the
// database is dropped (any connection left is cut) and created empty, so
// nothing a newer schema added survives, then the dump is restored into
// it. The dump is copied into the container and removed afterwards.
// dropped reports that the database was dropped: a failure before leaves
// it as it was.
func (d DB) Restore(ctx context.Context, src string) (dropped bool, err error) {
	tmp := "/tmp/backsight-" + randHex() + ".dump"
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_, _ = d.Compose.Exec(cctx, "db", "rm", "-f", tmp)
	}()
	if _, err := d.Compose.run(ctx, "cp", src, "db:"+tmp); err != nil {
		return false, err
	}
	// From the drop on the database is not what it was, even when the
	// command reports a failure (it may have dropped before failing).
	if _, err := d.Compose.Exec(ctx, "db", "dropdb", "-U", dbUser, "--if-exists", "--force", dbName); err != nil {
		return true, err
	}
	if _, err := d.Compose.Exec(ctx, "db", "createdb", "-U", dbUser, dbName); err != nil {
		return true, err
	}
	_, err = d.Compose.Exec(ctx, "db", "pg_restore", "-U", dbUser, "-d", dbName, "--no-owner", "--exit-on-error", tmp)
	return true, err
}

// RemoveStaleDumps removes the dumps a killed backup or restore left in the
// database container (/tmp/backsight-*.dump: the memory in clear). The
// caller holds the maintenance lock, so no dump is being made.
func (d DB) RemoveStaleDumps(ctx context.Context) error {
	_, err := d.Compose.Exec(ctx, "db", "sh", "-c", "rm -f /tmp/backsight-*.dump")
	return err
}

// DocumentMetadata reads one document's metadata from the database, read
// only, for when the API cannot answer (Hindsight 1.1.0 keeps it in
// documents.retain_params.metadata). No such document is nil, nil.
func (d DB) DocumentMetadata(ctx context.Context, bank, id string) (map[string]string, error) {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	out, err := d.Query(ctx, "select coalesce((select (retain_params::jsonb)->'metadata' from documents where bank_id = "+q(bank)+" and id = "+q(id)+"), 'null'::jsonb)")
	if err != nil {
		return nil, err
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(out), &meta); err != nil {
		return nil, fmt.Errorf("document %s metadata %q: %w", id, out, err)
	}
	return meta, nil
}

// Counts returns the rows of tables in the database.
func (d DB) Counts(ctx context.Context, tables []string) (map[string]int64, error) {
	counts := map[string]int64{}
	for _, t := range tables {
		out, err := d.Query(ctx, "select count(*) from "+t)
		if err != nil {
			return nil, fmt.Errorf("counting %s: %w", t, err)
		}
		if counts[t], err = strconv.ParseInt(out, 10, 64); err != nil {
			return nil, fmt.Errorf("counting %s: %q", t, out)
		}
	}
	return counts, nil
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RestoreCounts restores a dump into a throwaway Postgres container (the
// pinned image, no network, the server's shared memory, removed afterwards
// whatever happens) and counts the rows of tables: what `backup verify`
// compares with the manifest.
func RestoreCounts(ctx context.Context, r Runner, dump string, tables []string) (map[string]int64, error) {
	name := "backsight-verify-" + randHex()
	defer func() { // registered first: a cancel during run must remove it too
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_, _ = r.Run(cctx, "rm", "-f", "-v", name)
	}()
	if out, err := r.Run(ctx, "run", "-d", "--network", "none", "--shm-size", "1g", "--name", name, "-e", "POSTGRES_USER="+dbUser, "-e", "POSTGRES_DB="+dbName,
		"-e", "POSTGRES_PASSWORD=verify-"+randHex(), PostgresImage); err != nil {
		return nil, fmt.Errorf("starting a throwaway database: %w: %s", err, strings.TrimSpace(string(out)))
	}
	ready := false
	for i := 0; i < 90 && !ready; i++ {
		// Over TCP: the image's temporary init server listens on no port,
		// so this answers only once the final server is up.
		if _, err := r.Run(ctx, "exec", name, "psql", "-h", "127.0.0.1", "-U", dbUser, "-d", dbName, "-At", "-c", "select 1"); err == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !ready {
		return nil, fmt.Errorf("the throwaway database did not start")
	}
	if out, err := r.Run(ctx, "cp", dump, name+":/tmp/db.dump"); err != nil {
		return nil, fmt.Errorf("copying the dump: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := r.Run(ctx, "exec", name, "pg_restore", "-U", dbUser, "-d", dbName, "--no-owner", "--exit-on-error", "/tmp/db.dump"); err != nil {
		return nil, fmt.Errorf("restoring: %w: %s", err, strings.TrimSpace(string(out)))
	}
	counts := map[string]int64{}
	for _, t := range tables {
		out, err := r.Run(ctx, "exec", name, "psql", "-U", dbUser, "-d", dbName, "-At", "-c", "select count(*) from "+t)
		if err != nil {
			return nil, fmt.Errorf("counting %s: %w", t, err)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("counting %s: %q", t, out)
		}
		counts[t] = n
	}
	return counts, nil
}
