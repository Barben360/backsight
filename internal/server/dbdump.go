package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// Query runs one statement and returns its single value.
func (d DB) Query(ctx context.Context, sql string) (string, error) {
	out, err := d.Compose.Exec(ctx, "db", "psql", "-U", dbUser, "-d", dbName, "-At", "-v", "ON_ERROR_STOP=1", "-c", sql)
	return strings.TrimSpace(string(out)), err
}

// Dump writes a custom-format dump to dst: dumped to a file inside the
// container (binary data never goes through a pipe that mixes stderr),
// listed by pg_restore as a check, copied out, removed.
func (d DB) Dump(ctx context.Context, dst string) error {
	tmp := "/tmp/backsight-" + randHex() + ".dump"
	defer func() { _, _ = d.Compose.Exec(context.Background(), "db", "rm", "-f", tmp) }()
	if _, err := d.Compose.Exec(ctx, "db", "pg_dump", "-U", dbUser, "-d", dbName, "-Fc", "-f", tmp); err != nil {
		return err
	}
	if _, err := d.Compose.Exec(ctx, "db", "pg_restore", "--list", tmp); err != nil {
		return fmt.Errorf("the dump does not list: %w", err)
	}
	_, err := d.Compose.run(ctx, "cp", "db:"+tmp, dst)
	return err
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RestoreCounts restores a dump into a throwaway Postgres container (the
// pinned image, no network, removed afterwards) and counts the rows of
// tables: what `backup verify` compares with the manifest.
func RestoreCounts(ctx context.Context, r Runner, dump string, tables []string) (map[string]int64, error) {
	name := "backsight-verify-" + randHex()
	if out, err := r.Run(ctx, "run", "-d", "--network", "none", "--name", name, "-e", "POSTGRES_USER="+dbUser, "-e", "POSTGRES_DB="+dbName,
		"-e", "POSTGRES_PASSWORD=verify-"+randHex(), PostgresImage); err != nil {
		return nil, fmt.Errorf("starting a throwaway database: %w: %s", err, strings.TrimSpace(string(out)))
	}
	defer func() { _, _ = r.Run(context.Background(), "rm", "-f", "-v", name) }()
	ready := false
	for i := 0; i < 60 && !ready; i++ {
		// pg_isready answers during the image's init run too: a query
		// against the database proves the final server is up.
		if _, err := r.Run(ctx, "exec", name, "psql", "-U", dbUser, "-d", dbName, "-At", "-c", "select 1"); err == nil {
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
