package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// nativeDBTools plays the native runtime's tools on a database of rows
// rows in each counted table: dumps carry their row count ("PGDMP
// n=<rows>"), the throwaway database of a verify (any password but the
// server's) is apart, systemd-run's db-init creates an instance whose
// identifier is nextID.
type nativeDBTools struct {
	mu         sync.Mutex
	n          server.Native
	pw         string // the server database's password
	calls      []string
	envs       [][]string
	up         bool // the server's database answers
	rows       int  // rows of each counted table; -1: no database
	verifyRows int
	nextID     string
	restores   []int // rows of each dump restored into the server's database
	lose       int   // the next restores into the server's database lose one row each
}

func (f *nativeDBTools) Run(_ context.Context, env []string, _ io.Reader, path string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(append([]string{path}, args...), " ")
	f.calls = append(f.calls, line)
	f.envs = append(f.envs, env)
	live := slices.Contains(env, "PGPASSWORD="+f.pw)
	dumpRows := func(p string) int {
		raw, _ := os.ReadFile(p)
		n, _ := strconv.Atoi(strings.TrimPrefix(string(raw), "PGDMP n="))
		return n
	}
	last := args[len(args)-1]
	switch filepath.Base(path) {
	case "systemd-run":
		if strings.HasSuffix(argAfter(args, "--unit"), "-db-init") {
			if err := os.MkdirAll(f.n.DataDir(), 0o700); err != nil {
				return nil, err
			}
			_ = os.WriteFile(filepath.Join(f.n.DataDir(), "PG_VERSION"), []byte("18\n"), 0o600)
			_ = os.WriteFile(filepath.Join(f.n.DataDir(), "fake-sysid"), []byte(f.nextID+"\n"), 0o600)
			f.rows = 0
		}
	case "pg_controldata":
		id, err := os.ReadFile(filepath.Join(argAfter(args, "-D"), "fake-sysid"))
		if err != nil {
			return nil, errors.New("pg_controldata: could not open file")
		}
		return []byte("Database system identifier:           " + strings.TrimSpace(string(id)) + "\n"), nil
	case "psql":
		if live && !f.up {
			return nil, errors.New("psql: error: connection to server at \"127.0.0.1\" failed: Connection refused")
		}
		switch {
		case last == "select 1":
			return []byte("1\n"), nil
		case last == "SHOW fsync":
			return []byte("on\n"), nil
		case strings.Contains(last, "alembic_version"):
			return []byte("c7e2f9a4d1b8\n"), nil
		case strings.Contains(last, "pg_extension"):
			return []byte("0.8.5\n"), nil
		case strings.Contains(last, "retain_params"):
			return []byte("null\n"), nil
		case strings.Contains(last, "json_agg(bank_id"):
			return []byte("[]\n"), nil
		case strings.HasPrefix(last, "select count(*) from") && !live:
			return []byte(strconv.Itoa(f.verifyRows) + "\n"), nil
		case strings.HasPrefix(last, "select count(*) from"):
			if f.rows < 0 {
				return nil, errors.New("database \"hindsight\" does not exist")
			}
			return []byte(strconv.Itoa(f.rows) + "\n"), nil
		}
	case "pg_dump":
		if !f.up {
			return nil, errors.New("pg_dump: error: connection refused")
		}
		return nil, os.WriteFile(argAfter(args, "-f"), []byte(fmt.Sprintf("PGDMP n=%d", f.rows)), 0o600)
	case "pg_restore":
		switch {
		case slices.Contains(args, "--list"):
		case slices.Contains(args, "--data-only"):
			var b strings.Builder
			b.WriteString("COPY public.t (id) FROM stdin;\n")
			for i := range dumpRows(last) {
				b.WriteString(strconv.Itoa(i) + "\n")
			}
			b.WriteString("\\.\n")
			return nil, os.WriteFile(argAfter(args, "-f"), []byte(b.String()), 0o600)
		case live:
			if !f.up {
				return nil, errors.New("pg_restore: connection refused")
			}
			f.rows = dumpRows(last)
			f.restores = append(f.restores, f.rows)
			if f.lose > 0 {
				f.lose--
				f.rows--
			}
		default:
			f.verifyRows = dumpRows(last)
		}
	case "dropdb":
		if live {
			f.rows = -1
		}
	case "createdb":
		if live {
			f.rows = 0
		}
	case "pg_ctl":
		if pid := strings.TrimSpace(strings.SplitN(readFile0(filepath.Join(argAfter(args, "-D"), "postmaster.pid")), "\n", 2)[0]); pid != "" {
			_ = os.RemoveAll(filepath.Join(nativeProc, pid))
		}
	}
	return nil, nil
}

func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func readFile0(p string) string { raw, _ := os.ReadFile(p); return string(raw) }

// nativeSysFake plays the user manager: the db unit's start makes the
// database answer, the server unit's makes the API answer, a stop the
// other way round.
type nativeSysFake struct {
	mu      sync.Mutex
	calls   []string
	db      *nativeDBTools
	apiDown *atomic.Bool
}

func (s *nativeSysFake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, line)
	if name != "systemctl" || len(args) < 3 {
		return nil, nil
	}
	verb, units := args[1], args[2:]
	if verb == "enable" && units[0] == "--now" {
		verb, units = "start", units[1:]
	}
	if verb == "disable" && units[0] == "--now" {
		verb, units = "stop", units[1:]
	}
	for _, u := range units {
		switch {
		case (verb == "start" || verb == "restart") && strings.HasSuffix(u, "-db.service"):
			s.db.mu.Lock()
			_, err := os.Stat(filepath.Join(s.db.n.DataDir(), "PG_VERSION"))
			s.db.up = err == nil
			s.db.mu.Unlock()
		case (verb == "start" || verb == "restart") && strings.HasSuffix(u, "-server.service"):
			s.apiDown.Store(false)
		case verb == "stop" && strings.HasSuffix(u, "-db.service"):
			s.db.mu.Lock()
			s.db.up = false
			s.db.mu.Unlock()
		case verb == "stop" && strings.HasSuffix(u, "-server.service"):
			s.apiDown.Store(true)
		}
	}
	return nil, nil
}

// nativeHost is a native install, rendered, running on a recorded database
// of 10 rows (identifier 111), with a fake server whose API answers only
// while the server unit runs; /proc is an empty fake.
func nativeHost(t *testing.T) (home string, n server.Native, tools *nativeDBTools, sys *nativeSysFake, port int) {
	t.Helper()
	home = devInstall(t, true)
	port = fakeHindsight(t)
	cfgPath := pathsOf(home).ConfigFile()
	if err := os.WriteFile(cfgPath, []byte("[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\nruntime = \"native\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSys(t, "linux")
	d := pathsOf(home)
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil || sec.DBPassword == "" {
		t.Fatalf("no db_password: %v", err)
	}
	n = server.Native{Cfg: cfg, Dirs: d}
	tools = &nativeDBTools{n: n, pw: sec.DBPassword, up: true, rows: 10, nextID: "333"}
	apiDown := &atomic.Bool{}
	sys = &nativeSysFake{db: tools, apiDown: apiDown}
	n.Tools = tools
	oldSys, oldTools, oldProc, oldPoll := nativeSys, nativeTools, nativeProc, healthPoll
	nativeSys, nativeTools, nativeProc, healthPoll = sys, tools, t.TempDir(), time.Millisecond
	t.Cleanup(func() { nativeSys, nativeTools, nativeProc, healthPoll = oldSys, oldTools, oldProc, oldPoll })
	fakeHook = func(*http.Request) {
		if apiDown.Load() {
			panic(http.ErrAbortHandler) // the connection drops: nothing listens
		}
	}
	if code, out, errOut := run(t, "server", "render"); code != output.OK {
		t.Fatalf("server render: exit %d\n%s%s", code, out, errOut)
	}
	withInstance(t, n, "111", "111")
	return home, n, tools, sys, port
}

// nativeArchive is a backup of the native install's 10 rows taken at
// 04:00; the clock is then at noon.
func nativeArchive(t *testing.T, home string) string {
	t.Helper()
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	if code, out, errOut := run(t, "backup", "create"); code != output.OK {
		t.Fatalf("backup: exit %d\n%s%s", code, out, errOut)
	}
	withClock(t, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	return filepath.Join(pathsOf(home).Data, "backups", "backsight-20261025T040000Z.tar.gz.age")
}

// sidecar reads an archive's manifest from its .json sidecar.
func sidecar(t *testing.T, archive string) (backup.Manifest, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(archive + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var m backup.Manifest
	var keys map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &keys)
	return m, keys
}

// The manifest names the runtime it came from and its Hindsight release
// (ruling R4); a native archive names pg0's Postgres and holds no compose
// file (ruling R26).
func TestManifestRecordsTheRuntime(t *testing.T) {
	t.Run("docker", func(t *testing.T) {
		home, _, _, _ := restoreSetup(t)
		m, keys := sidecar(t, filepath.Join(pathsOf(home).Data, "backups", "backsight-20261025T040000Z.tar.gz.age"))
		if m.Runtime != "docker" || m.HindsightVersion != "1.1.0" || keys["runtime"] != "docker" || keys["hindsight_version"] != "1.1.0" {
			t.Errorf("runtime %q, hindsight_version %q (%v)", m.Runtime, m.HindsightVersion, keys)
		}
		if m.HindsightImage != server.HindsightImage || m.PostgresImage != server.PostgresImage {
			t.Errorf("images %q %q", m.HindsightImage, m.PostgresImage)
		}
	})
	t.Run("native", func(t *testing.T) {
		home, n, _, _, _ := nativeHost(t)
		// A compose file left from Docker, before a switch: not the
		// native server's.
		if err := os.WriteFile(filepath.Join(n.Cfg.Server.ComposeDir, "compose.override.yaml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m, keys := sidecar(t, nativeArchive(t, home))
		if m.Runtime != "native" || m.HindsightVersion != "1.1.0" || keys["runtime"] != "native" || keys["hindsight_version"] != "1.1.0" {
			t.Errorf("runtime %q, hindsight_version %q (%v)", m.Runtime, m.HindsightVersion, keys)
		}
		if m.HindsightImage != "" || m.PostgresImage != "pg0-embedded 0.15.2 (PostgreSQL 18.1, pgvector 0.8.5)" {
			t.Errorf("images %q %q", m.HindsightImage, m.PostgresImage)
		}
		if m.Counts["memory_units"] != 10 || m.PGVector != "0.8.5" {
			t.Errorf("counts %v, pgvector %q", m.Counts, m.PGVector)
		}
		if slices.Contains(m.Files, "compose.override.yaml") || !slices.Contains(m.Files, "config.toml") {
			t.Errorf("files %v", m.Files)
		}
	})
}

// Archives cross runtimes by Hindsight release (ruling R4): not newer
// restores, newer or unknown is refused (exit 20).
func TestRestoreAcrossRuntimesComparesHindsightVersions(t *testing.T) {
	home := isolate(t)
	cfg := config.Default(pathsOf(home), home)
	emb := cfg.Embeddings.Preset + ": " + server.EmbeddingModels
	docker := server.Docker{Cfg: cfg}
	native := server.Native{Cfg: cfg}
	for _, c := range []struct {
		name string
		m    backup.Manifest
		rt   server.Runtime
		want string // "" restores
	}{
		{"docker archive into native", backup.Manifest{Runtime: "docker", HindsightVersion: "1.1.0", HindsightImage: server.HindsightImage}, native, ""},
		{"docker archive of before the field into native", backup.Manifest{HindsightImage: "ghcr.io/vectorize-io/hindsight:1.1.0@sha256:ffff"}, native, ""},
		{"native archive into native", backup.Manifest{Runtime: "native", HindsightVersion: "1.1.0"}, native, ""},
		{"native archive into docker", backup.Manifest{Runtime: "native", HindsightVersion: "1.1.0"}, docker, ""},
		{"older native archive into docker", backup.Manifest{Runtime: "native", HindsightVersion: "1.0.4"}, docker, ""},
		{"newer native archive into docker", backup.Manifest{Runtime: "native", HindsightVersion: "1.2.0"}, docker,
			"the archive was taken with Hindsight 1.2.0 on native, newer than this install's 1.1.0: update backsight first"},
		{"newer native archive into native", backup.Manifest{Runtime: "native", HindsightVersion: "1.1.1"}, native,
			"the archive was taken with Hindsight 1.1.1 on native, newer than this install's 1.1.0: update backsight first"},
		{"newer docker archive into native", backup.Manifest{HindsightImage: "ghcr.io/vectorize-io/hindsight:9.0.0@sha256:ffff"}, native,
			"the archive was taken with Hindsight 9.0.0 on docker, newer than this install's 1.1.0: update backsight first"},
		{"neither version nor tag into native", backup.Manifest{HindsightImage: "ghcr.io/vectorize-io/hindsight@sha256:ffff"}, native,
			"cannot tell the archive's Hindsight version (ghcr.io/vectorize-io/hindsight@sha256:ffff)"},
		{"native archive with no version into docker", backup.Manifest{Runtime: "native"}, docker,
			"cannot tell the archive's Hindsight version (none recorded)"},
		// Same runtime, Docker: placed by its image, today's messages.
		{"docker rebuild into docker", backup.Manifest{Runtime: "docker", HindsightVersion: "1.1.0", HindsightImage: "ghcr.io/vectorize-io/hindsight:1.1.0@sha256:ffff"}, docker,
			`cannot tell whether the archive's Hindsight "ghcr.io/vectorize-io/hindsight:1.1.0@sha256:ffff" is older than this install's`},
		{"newer docker archive into docker", backup.Manifest{HindsightImage: "ghcr.io/vectorize-io/hindsight:9.0.0@sha256:ffff"}, docker,
			"the archive was taken with Hindsight ghcr.io/vectorize-io/hindsight:9.0.0@sha256:ffff, newer than this install's"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.m.Bank, c.m.Embeddings = cfg.Server.Bank, emb
			err := restorable(c.m, cfg, c.rt)
			if c.want == "" {
				if err != nil {
					t.Errorf("refused: %v", err)
				}
				return
			}
			var oe *output.Error
			if !errors.As(err, &oe) || oe.Code != output.Blocked || !strings.HasPrefix(oe.Message, c.want) {
				t.Errorf("got %v, want exit 20 with %q", err, c.want)
			}
		})
	}
	// Through the command, nothing changed: a newer native archive into
	// a Docker install.
	home, _, dk, port := restoreSetup(t)
	archive := writeArchive(t, home, backup.Manifest{Runtime: "native", HindsightVersion: "1.2.0", Bank: "claude-memory"})
	dk.calls = nil
	code, out, errOut := run(t, "restore", archive)
	if code != output.Blocked || !strings.Contains(errOut, "newer than this install's 1.1.0") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	for _, call := range dk.calls {
		if strings.Contains(call, "stop") || strings.Contains(call, "dropdb") || strings.Contains(call, "pg_dump") {
			t.Errorf("acted before refusing: %q", call)
		}
	}
	if e := epochOf(t, port); e.Epoch != 0 {
		t.Errorf("epoch %+v", e)
	}
}

// Ruling R6: after a refused start (here another database than the one
// recorded), restore moves the refused one aside, kept as it is, makes a
// new database, restores the archive into it and records its identity;
// the new epoch is above this machine's and the archive's, though the
// refused database and the API it served cannot be read.
func TestRestoreAfterARefusedStartAdoptsTheArchive(t *testing.T) {
	home, n, tools, sys, port := nativeHost(t)
	archive := nativeArchive(t, home)
	// The archive's epoch is 3, this machine last reconciled with 5.
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "", 5*time.Second)
	if err := retain.WriteEpoch(context.Background(), cl, "claude-memory", retain.Epoch{Epoch: 3}); err != nil {
		t.Fatal(err)
	}
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: "claude-memory", Epoch: 5}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Another database is there now: its start was refused, nothing runs.
	if err := os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte("222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(n.DataDir(), "theirs"), []byte("someone's memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.CheckInstance(context.Background()); err == nil {
		t.Fatal("the instance is not refused: the test proves nothing")
	}
	tools.up, tools.rows = false, 25
	sys.apiDown.Store(true)
	before := backupsIn(home)
	tools.calls = nil

	// The plan says so and changes nothing.
	stamp := strconv.FormatInt(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC).Unix(), 10)
	moved := n.InstanceDir() + ".refused-" + stamp
	code, out, errOut := run(t, "restore", archive, "--plan")
	if code != output.OK || !strings.Contains(out, "would be moved aside, kept, to "+moved) || readFile0(filepath.Join(n.DataDir(), "theirs")) == "" {
		t.Errorf("plan: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = run(t, "restore", archive, "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var env struct {
		Data struct {
			MovedAside string `json:"moved_aside"`
			Epoch      struct {
				Epoch int `json:"epoch"`
			} `json:"epoch"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.MovedAside != moved {
		t.Errorf("moved_aside %q, want %q", env.Data.MovedAside, moved)
	}
	// The refused database, kept as it was.
	if readFile0(filepath.Join(moved, "data", "fake-sysid")) != "222\n" || readFile0(filepath.Join(moved, "data", "theirs")) != "someone's memory" ||
		readFile0(filepath.Join(moved, "data", "PG_VERSION")) != "18\n" {
		t.Errorf("the refused database was not kept as it was in %s", moved)
	}
	if readFile0(n.IdentityFile()+".refused-"+stamp) != "111\n" {
		t.Error("the identity it was refused against is not kept")
	}
	// A new database, recorded, holding the archive.
	if got := readFile0(n.IdentityFile()); got != "333\n" || readFile0(filepath.Join(n.DataDir(), "fake-sysid")) != "333\n" {
		t.Errorf("identity %q: the new database's is 333", got)
	}
	if !slices.Equal(tools.restores, []int{10}) || tools.rows != 10 {
		t.Errorf("restores %v, rows %d", tools.restores, tools.rows)
	}
	create := slices.IndexFunc(tools.calls, func(c string) bool { return strings.Contains(c, "-db-init") })
	load := slices.IndexFunc(tools.calls, func(c string) bool { return strings.Contains(c, "/pg_restore -d hindsight") })
	if create < 0 || load < create {
		t.Errorf("the archive was not restored into a new database (created at %d, restored at %d)", create, load)
	}
	// No safety backup: the refused database cannot be dumped, and it is
	// kept instead.
	if got := backupsIn(home); len(got) != len(before) {
		t.Errorf("backups %v, before %v", got, before)
	}
	// Above this machine's 5 and the archive's 3.
	if e := epochOf(t, port); e.Epoch != 6 || env.Data.Epoch.Epoch != 6 {
		t.Errorf("epoch %d (reported %d), want 6", e.Epoch, env.Data.Epoch.Epoch)
	}
	if st, _ := state.ReadRestoreMarker(pathsOf(home)); st != nil {
		t.Errorf("the restore marker stays: %+v", st)
	}
	if err := n.CheckInstance(context.Background()); err != nil {
		t.Errorf("the new database is refused: %v", err)
	}
}

// A restore that fails after the refused database was moved aside removes
// the new one (never served empty or half restored) and keeps the refused
// one; the marker holds every client; restore again adopts the archive.
func TestRestoreAfterARefusedStartRemovesTheNewDatabaseOnFailure(t *testing.T) {
	home, n, tools, sys, port := nativeHost(t)
	archive := nativeArchive(t, home)
	if err := os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte("222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools.up = false
	sys.apiDown.Store(true)
	// The restored database lacks rows: the counts differ from the manifest's.
	tools.lose = 1
	code, out, errOut := run(t, "restore", archive)
	if code != output.Blocked || !strings.Contains(errOut, "the new native database was removed; the refused database is kept in "+n.InstanceDir()+".refused-") ||
		!strings.Contains(errOut, "run backsight restore "+archive+" again") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if exists(n.InstanceDir()) || exists(n.IdentityFile()) {
		t.Error("the new database or its identity stays: a start would serve it")
	}
	moved, _ := filepath.Glob(n.InstanceDir() + ".refused-*")
	if len(moved) != 1 || readFile0(filepath.Join(moved[0], "data", "fake-sysid")) != "222\n" {
		t.Errorf("the refused database is not kept: %v", moved)
	}
	if m, _ := state.ReadRestoreMarker(pathsOf(home)); m == nil {
		t.Error("no restore marker: clients would send to a server without its memory")
	}
	sys.apiDown.Store(false) // to read the fake's epoch
	if e := epochOf(t, port); e.Epoch != 0 {
		t.Errorf("an epoch was written: %+v", e)
	}
	// Restore again: nothing to move aside, a new database, the archive.
	withClock(t, time.Date(2026, 10, 25, 13, 0, 0, 0, time.UTC))
	code, out, errOut = run(t, "restore", archive)
	if code != output.OK {
		t.Fatalf("again: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "into a new native database; restore epoch") || strings.Contains(out, "is kept in") {
		t.Errorf("summary:\n%s", out)
	}
	if readFile0(n.IdentityFile()) != "333\n" || tools.rows != 10 {
		t.Errorf("identity %q, rows %d", readFile0(n.IdentityFile()), tools.rows)
	}
	if m, _ := state.ReadRestoreMarker(pathsOf(home)); m != nil {
		t.Errorf("the marker stays: %+v", m)
	}
}

// backup verify on native stops a stray postgres on the database's data
// first (requirement 3), then verifies in a throwaway database.
func TestBackupVerifyOnNativeStopsAStrayFirst(t *testing.T) {
	home, n, tools, _, _ := nativeHost(t)
	archive := nativeArchive(t, home)
	pg := filepath.Join(n.Home(), ".pg0", "installation", "18.1.0", "bin", "postgres")
	writeProc(t, "4242", []string{pg, "-D", n.DataDir()}, "foo.scope", n.DataDir())
	writePidFile(t, n, "4242")
	tools.calls = nil
	code, out, errOut := run(t, "backup", "verify", archive)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out+errOut, "a postgres (pid 4242) ran on "+n.DataDir()+" outside backsight-db.service: stopped, never adopted") {
		t.Errorf("the stray is not reported:\n%s%s", out, errOut)
	}
	stop := slices.IndexFunc(tools.calls, func(c string) bool { return strings.Contains(c, "/pg_ctl stop -D "+n.DataDir()) })
	verify := slices.IndexFunc(tools.calls, func(c string) bool { return strings.HasPrefix(c, "systemd-run") && strings.Contains(c, "-verify-") })
	if stop < 0 || verify < 0 || stop > verify {
		t.Errorf("pg_ctl at %d, the throwaway database at %d:\n%s", stop, verify, strings.Join(tools.calls, "\n"))
	}
	if tools.verifyRows != 10 || !strings.Contains(out, "ok") {
		t.Errorf("verify rows %d\n%s", tools.verifyRows, out)
	}
}
