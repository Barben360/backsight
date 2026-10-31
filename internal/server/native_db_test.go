package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/testtool"
)

// argOf is the value after flag in args ("" without it).
func argOf(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// envOf is key's value in env ("" when it is not set).
func envOf(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// copyBlock is a plain SQL script holding rows rows of documents' data,
// between other statements and another table's empty block.
func copyBlock(rows int) string {
	var b strings.Builder
	b.WriteString("--\n-- PostgreSQL database dump\n--\nSET statement_timeout = 0;\nCOPY public.documents (id, bank_id) FROM stdin;\n")
	for i := range rows {
		b.WriteString("doc-" + strconv.Itoa(i) + "\tclaude-memory\n")
	}
	b.WriteString("\\.\n\n\nSELECT pg_catalog.setval('x', 1, false);\nCOPY public.banks (bank_id) FROM stdin;\n\\.\n")
	return b.String()
}

// dumpEffects does to the disk what pg_dump and pg_restore do: a dump
// file, and each table's data, of rows rows, where -f names.
func dumpEffects(rows int) func(c toolCall) {
	return func(c toolCall) {
		f := argOf(c.Args, "-f")
		switch filepath.Base(c.Path) {
		case "pg_dump":
			_ = os.WriteFile(f, []byte("PGDMP"), 0o600)
		case "pg_restore":
			if slices.Contains(c.Args, "--data-only") && f != "" && f != "-" {
				_ = os.WriteFile(f, []byte(copyBlock(rows)), 0o600)
			}
		}
	}
}

// RF 1: every database tool gets the password in PGPASSWORD, its lib on
// LD_LIBRARY_PATH and the server's address in PG*, never the password on
// its argv; VerifyDump's throwaway database has a password of its own,
// which no argv carries either.
func TestNativeDatabaseToolsTakeThePasswordFromTheEnvironment(t *testing.T) {
	ctx := context.Background()
	sec := config.Secrets{DBPassword: "dbpw-XYZ", APIKey: "api-XYZ", LLMAPIKey: "llm-XYZ", UIKey: "uikey-XYZ"}
	n, _, tools := nativeFixture(t, sec)
	installed(t, n, sec)
	withFreePort(t, 18890)
	tools.Effect = dumpEffects(2)
	tools.Out = func(c toolCall) []byte {
		if slices.Contains(c.Args, "select count(*) from documents") {
			return []byte("2\n")
		}
		return nil
	}
	inst := filepath.Join(n.Home(), ".pg0", "installation", "18.1.0")
	work := t.TempDir()
	db := n.Database()
	if _, err := db.Query(ctx, "select 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Dump(ctx, filepath.Join(work, "db.dump"), []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Restore(ctx, filepath.Join(work, "db.dump")); err != nil {
		t.Fatal(err)
	}
	live := len(tools.Calls)
	if _, err := n.VerifyDump(ctx, filepath.Join(work, "db.dump"), []string{"documents"}); err != nil {
		t.Fatal(err)
	}

	var verifyPW string
	seen := map[string]bool{}
	for i, c := range tools.Calls {
		argv := c.Path + " " + strings.Join(c.Args, " ")
		for _, s := range []string{"dbpw-XYZ", "api-XYZ", "llm-XYZ"} {
			if strings.Contains(argv, s) {
				t.Errorf("%s has %s on its argv", argv, s)
			}
		}
		name := filepath.Base(c.Path)
		if !slices.Contains([]string{"psql", "pg_dump", "pg_restore", "dropdb", "createdb"}, name) {
			continue
		}
		if filepath.Dir(c.Path) != filepath.Join(inst, "bin") {
			t.Errorf("%s is not the bundled one (%s/bin)", c.Path, inst)
		}
		seen[name] = true
		if v := envOf(c.Env, "LD_LIBRARY_PATH"); v != filepath.Join(inst, "lib") {
			t.Errorf("%s: LD_LIBRARY_PATH %q", argv, v)
		}
		host := envOf(c.Env, "PGHOST")
		user := envOf(c.Env, "PGUSER")
		pw := envOf(c.Env, "PGPASSWORD")
		port := envOf(c.Env, "PGPORT")
		if host != "127.0.0.1" || user != "hindsight" {
			t.Errorf("%s: PGHOST %q PGUSER %q", argv, host, user)
		}
		if i < live {
			if pw != "dbpw-XYZ" || port != "8889" {
				t.Errorf("%s on the server's database: PGPASSWORD %q PGPORT %q, want db_password and 8889", argv, pw, port)
			}
			continue
		}
		if pw == "" || pw == "dbpw-XYZ" || port != "18890" {
			t.Errorf("%s on the throwaway database: PGPASSWORD %q PGPORT %q, want its own password and 18890", argv, pw, port)
		}
		verifyPW = pw
	}
	for _, name := range []string{"psql", "pg_dump", "pg_restore", "dropdb", "createdb"} {
		if !seen[name] {
			t.Errorf("%s never ran: the test proves nothing about it", name)
		}
	}
	if verifyPW == "" {
		t.Fatal("no tool ran on the throwaway database")
	}
	for _, c := range tools.Calls {
		if strings.Contains(c.Path+" "+strings.Join(c.Args, " "), verifyPW) {
			t.Errorf("the throwaway's password is on %s %v", c.Path, c.Args)
		}
	}

	// The argv the brief fixes, nothing else on it.
	want := []string{
		filepath.Join(inst, "bin", "psql") + " -d hindsight -At -v ON_ERROR_STOP=1 -c select 1",
		filepath.Join(inst, "bin", "pg_dump") + " -d hindsight -Fc -f " + filepath.Join(work, "db.dump"),
		filepath.Join(inst, "bin", "pg_restore") + " --list " + filepath.Join(work, "db.dump"),
		filepath.Join(inst, "bin", "pg_restore") + " --data-only -t documents -f " + filepath.Join(work, ".count-documents.sql") + " " + filepath.Join(work, "db.dump"),
		filepath.Join(inst, "bin", "dropdb") + " --maintenance-db=postgres --if-exists --force hindsight",
		filepath.Join(inst, "bin", "createdb") + " --maintenance-db=postgres hindsight",
		filepath.Join(inst, "bin", "pg_restore") + " -d hindsight --no-owner --exit-on-error " + filepath.Join(work, "db.dump"),
	}
	var got []string
	for _, c := range tools.Calls[:live] {
		got = append(got, c.Path+" "+strings.Join(c.Args, " "))
	}
	if !slices.Equal(got, want) {
		t.Errorf("ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if exists(filepath.Join(work, ".count-documents.sql")) {
		t.Error("the table's data in clear stays beside the dump")
	}
}

// The rows are counted from the dump's COPY block, not from the database.
func TestNativeDumpCountsFromTheDump(t *testing.T) {
	n, _, tools := nativeFixture(t, secrets)
	installed(t, n, secrets)
	tools.Effect = dumpEffects(3)
	counts, err := n.Database().Dump(context.Background(), filepath.Join(t.TempDir(), "db.dump"), []string{"documents"})
	if err != nil {
		t.Fatal(err)
	}
	if counts["documents"] != 3 || len(counts) != 1 {
		t.Errorf("counts %v, want documents: 3", counts)
	}
	// A dump that does not list is an error.
	tools.Fail = func(c toolCall) error {
		if slices.Contains(c.Args, "--list") {
			return errors.New("pg_restore: error: input file is too short")
		}
		return nil
	}
	if _, err := n.Database().Dump(context.Background(), filepath.Join(t.TempDir(), "db.dump"), []string{"documents"}); err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Errorf("a dump that does not list: %v", err)
	}
}

// ctxSys records each call with whether its context was still live.
type ctxSys struct {
	mu    sync.Mutex
	calls []string
	live  []bool
}

func (s *ctxSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name+" "+strings.Join(args, " "))
	s.live = append(s.live, ctx.Err() == nil)
	return nil, nil
}

// VerifyDump's throwaway database runs in a supervised unit (an hour at
// most), and is stopped and removed after a success, a failing restore
// and a cancel alike.
func TestNativeVerifyDumpCleansUpAlways(t *testing.T) {
	for _, name := range []string{"success", "failing restore", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			n, _, tools := nativeFixture(t, secrets)
			installed(t, n, secrets)
			withFreePort(t, 18890)
			sys := &ctxSys{}
			n.Sys = sys
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var home string
			tools.Effect = func(c toolCall) {
				if c.Path == "systemd-run" {
					for i, a := range c.Args {
						if v, ok := strings.CutPrefix(a, "HOME="); ok && i > 0 && c.Args[i-1] == "-E" {
							home = v
						}
					}
					if !exists(home) {
						t.Errorf("the throwaway's HOME %s does not exist when its unit starts", home)
					} else {
						testtool.RequirePrivate(t, home)
					}
				}
			}
			tools.Out = func(c toolCall) []byte {
				if filepath.Base(c.Path) == "psql" && slices.Contains(c.Args, "select count(*) from documents") {
					return []byte("7\n")
				}
				return nil
			}
			tools.Fail = func(c toolCall) error {
				switch {
				case name == "failing restore" && filepath.Base(c.Path) == "pg_restore":
					return errors.New("pg_restore: error: could not execute query")
				case name == "cancelled" && filepath.Base(c.Path) == "psql":
					cancel()
					return context.Canceled
				}
				return nil
			}
			counts, err := n.VerifyDump(ctx, filepath.Join(t.TempDir(), "db.dump"), []string{"documents"})
			switch name {
			case "success":
				if err != nil || counts["documents"] != 7 {
					t.Errorf("counts %v, %v", counts, err)
				}
				// The throwaway cluster has no hindsight database before the
				// restore creates it: its readiness is asked of postgres.
				i := slices.IndexFunc(tools.Calls, func(c toolCall) bool { return filepath.Base(c.Path) == "psql" })
				if i < 0 || argOf(tools.Calls[i].Args, "-d") != "postgres" {
					t.Errorf("the first psql on the throwaway database asks %v", tools.Calls[i].Args)
				}
			default:
				if err == nil {
					t.Errorf("no error, counts %v", counts)
				}
			}

			i := slices.IndexFunc(tools.Calls, func(c toolCall) bool { return c.Path == "systemd-run" })
			if i < 0 {
				t.Fatal("no systemd-run")
			}
			args := tools.Calls[i].Args
			unit := argOf(args, "--unit")
			if !strings.HasPrefix(unit, "backsight-verify-") || !strings.HasSuffix(unit, ".service") {
				t.Errorf("unit %q", unit)
			}
			for _, p := range []string{"KillMode=control-group", "RuntimeMaxSec=3600"} {
				if !slices.Contains(args, p) || args[slices.Index(args, p)-1] != "-p" {
					t.Errorf("the throwaway unit lacks -p %s: %v", p, args)
				}
			}
			if !slices.Contains(args, "--user") || !slices.Contains(args, "--collect") || slices.Contains(args, "--wait") {
				t.Errorf("systemd-run %v", args)
			}
			if want := filepath.Join(n.Dirs.State, "staging"); filepath.Dir(home) != want || !strings.HasPrefix(filepath.Base(home), "verify-") {
				t.Errorf("HOME %s, want %s/verify-<id>", home, want)
			}
			stop := slices.Index(sys.calls, "systemctl --user stop "+unit)
			if stop < 0 {
				t.Fatalf("the unit was not stopped: %q", sys.calls)
			}
			if !sys.live[stop] {
				t.Error("the stop ran on a cancelled context: it would not run at all")
			}
			if exists(home) || exists(home+".lock") {
				t.Errorf("%s left behind", home)
			}
			ents, _ := os.ReadDir(filepath.Join(n.Dirs.State, "staging"))
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), "verify-") {
					t.Errorf("staging holds %s", e.Name())
				}
			}
		})
	}
}

// The throwaway database is a new cluster of its own: initdb with a
// password file then postgres, on the free port, on loopback only, with no
// Unix socket; a unit that stops ends the wait at once.
func TestNativeVerifyDumpRunsItsOwnCluster(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: the Linux units and scripts are the systemd host's")
	n, _, tools := nativeFixture(t, secrets)
	installed(t, n, secrets)
	withFreePort(t, 18890)
	var script string
	tools.Effect = func(c toolCall) {
		if c.Path == "systemd-run" {
			script = c.Args[len(c.Args)-1]
		}
	}
	tools.Fail = func(c toolCall) error {
		if filepath.Base(c.Path) == "psql" {
			return errors.New("connection refused")
		}
		return nil
	}
	sys := n.Sys.(*fakeSys)
	sys.Out = func(c []string) ([]byte, error) {
		if slices.Contains(c, "is-active") {
			return []byte("inactive\n"), errors.New("exit status 3")
		}
		return nil, nil
	}
	_, err := n.VerifyDump(context.Background(), filepath.Join(t.TempDir(), "db.dump"), []string{"documents"})
	if err == nil || !strings.Contains(err.Error(), "journalctl --user -u backsight-verify-") {
		t.Errorf("a stopped unit: %v", err)
	}
	inst := filepath.Join(n.Home(), ".pg0", "installation", "18.1.0", "bin")
	for _, w := range []string{inst + "/initdb -D ", " -U hindsight --pwfile=", " -A scram-sha-256 ", "&& rm -f ", inst + "/postgres -D ",
		" -p 18890 ", "-c listen_addresses=127.0.0.1", "-c unix_socket_directories="} {
		if !strings.Contains(script, w) {
			t.Errorf("the unit's script lacks %q:\n%s", w, script)
		}
	}
	if strings.Contains(script, "fakedbpassword") {
		t.Error("the unit's script holds db_password")
	}
}
