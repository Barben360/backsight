package cli

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metricsfile"
	"github.com/Barben360/backsight/internal/paths"
)

// collectCopyTables are the tables and views migration 0002 adds, whose row
// counts the copy run reports (ingest_errors is block 1's: it says whether
// the new lines were refused).
var collectCopyTables = []string{
	"cursors", "show_sets", "server_snapshots", "server_snapshot_projects", "server_days", "server_day_llm",
	"server_day_llm_status", "doctor_runs", "memory_docs", "memory_days", "doc_ops", "scans", "metrics_other",
	"memory_doc", "memory_day", "memory_coverage", "doc_op", "server_day", "ingest_errors",
}

// refuseLive is an error when d is the install this process would use: the
// copy run never writes to it.
func refuseLive(d paths.Dirs) error {
	live, err := paths.Default()
	if err != nil {
		return err
	}
	same := func(a, b string) bool {
		ra, ea := filepath.EvalSymlinks(a)
		rb, eb := filepath.EvalSymlinks(b)
		return filepath.Clean(a) == filepath.Clean(b) || (ea == nil && eb == nil && ra == rb)
	}
	if same(d.State, live.State) || same(d.Config, live.Config) {
		return errors.New("that is the live install, not a copy")
	}
	return nil
}

// collectOnACopy is what the copy run does: against root/state and
// root/config (a cp -a of the live state and config), the doctor-history
// append, collectServerDaily and collectMemoryDocs, through the copy's
// configured server with hindsight.ReadOnly (GET only), then the copy's
// index built and caught up. It returns the step lines (counts and
// classes) and the row count of each new table. It never runs jobs
// maintain (its backup, update and re-attribution steps write), and
// returns no memory text, id or path.
func collectOnACopy(t *testing.T, root string) (lines []string, rows map[string]int) {
	t.Helper()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	if err := refuseLive(d); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{d.State, d.ConfigFile()} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the copy lacks %s: %v", filepath.Base(p), err)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatalf("the copy's config does not load: %v", err)
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the copy's secrets do not load: %v", err)
	}
	ro, err := hindsight.NewReadOnly(cfg.Server.URL, sec.APIKey, cfg.Server.Bank, serverDailyTimeout)
	if err != nil {
		t.Fatalf("no read-only client for the copy's server: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	ix, err := index.Open(ctx, indexOptions(d, home, nil))
	if err != nil {
		t.Fatalf("the copy's index does not open: %v", err)
	}
	defer func() { _ = ix.Close() }()
	if _, err := ix.CatchUp(ctx, 5*time.Minute); err != nil {
		t.Fatalf("the copy's index does not catch up: %v", err)
	}

	at := time.Now()
	w := metricsfile.New(d.MetricsDir(), buildinfo.Version)
	// One doctor check stands for the run: the real checks probe the
	// machine, and only their ids and statuses are kept.
	lines = append(lines,
		"doctor history: "+maintainDoctorHistory(d, doctor.Report{Checks: []doctor.Check{{ID: "collect-copy", Status: doctor.OK}}}, time.Now),
		collectServerDaily(ctx, w, ro, at, time.Local),
		collectMemoryDocs(ctx, w, ix, ro, at))

	if _, err := ix.CatchUp(ctx, 5*time.Minute); err != nil {
		t.Fatalf("the copy's index does not catch up with the record: %v", err)
	}
	rows = map[string]int{}
	db, err := sql.Open("sqlite", filepath.Join(d.IndexDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, table := range collectCopyTables {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		rows[table] = n
	}
	return lines, rows
}

// TestCollectOnACopy runs the collection against a copy of the live state
// and config: skipped unless BACKSIGHT_COLLECT_COPY names a directory that
// holds `state` and `config` (cp -a of the live ones). The server is only
// read (GET, hindsight.ReadOnly) and everything is written under that
// directory. The log holds step lines and row counts, never a memory's
// text, id or path. Delete root/config afterwards: it holds the API key.
func TestCollectOnACopy(t *testing.T) {
	root := os.Getenv("BACKSIGHT_COLLECT_COPY")
	if root == "" {
		t.Skip("BACKSIGHT_COLLECT_COPY names a directory holding a cp -a copy of the live state and config")
	}
	lines, rows := collectOnACopy(t, root)
	for _, l := range lines {
		t.Log(l)
	}
	for _, table := range collectCopyTables {
		t.Logf("%-26s %d rows", table, rows[table])
	}
}

// copyTree copies the files of src under dst (the test's cp -a: modes kept
// private).
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if de.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !de.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The copy run's own code, on a copy of the canary install against the
// fake server: it records the day lists, the operations, the daily counts
// and the doctor history in the copy; the fake sees GET requests only; the
// install the copy came from is untouched; the live install is refused.
func TestCollectOnACopyHarness(t *testing.T) {
	home := canaryState(t)
	canaryServer(t, home)
	if got := tickIndex(context.Background(), pathsOf(home), home); !strings.HasPrefix(got, "caught up") {
		t.Fatalf("tick: %s", got)
	}
	live := pathsOf(home)
	if err := refuseLive(live); err == nil {
		t.Error("the install in use was not refused")
	}
	var mu sync.Mutex
	var methods []string
	fakeHook = func(r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
	}
	root := t.TempDir()
	copyTree(t, live.State, filepath.Join(root, "state"))
	copyTree(t, live.Config, filepath.Join(root, "config"))
	before := treeOf(t, filepath.Dir(live.State))

	lines, rows := collectOnACopy(t, root)
	t.Log(strings.Join(lines, "\n"))
	for _, table := range []string{"server_snapshots", "doctor_runs", "memory_docs", "memory_days", "doc_ops", "memory_doc", "memory_day"} {
		if rows[table] == 0 {
			t.Errorf("no row in %s", table)
		}
	}
	if rows["memory_coverage"] != 1 {
		t.Errorf("memory_coverage has %d rows, want 1", rows["memory_coverage"])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) == 0 {
		t.Error("the server was not read")
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("a %s request reached the server", m)
		}
	}
	sameTree(t, before, treeOf(t, filepath.Dir(live.State)))
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(root, "state", "metrics")); err != nil || st.Mode().Perm()&0o077 != 0 {
			t.Errorf("the copy's record directory: %v %v", st, err)
		}
	}
}
