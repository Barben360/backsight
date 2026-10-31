package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// jsonData decodes an envelope's data (and its error) into v.
func jsonData(t *testing.T, out string, v any) {
	t.Helper()
	var env struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *output.Error   `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, v); err != nil {
			t.Fatal(err)
		}
	}
}

func jsonError(t *testing.T, out string) *output.Error {
	t.Helper()
	var env struct {
		Error *output.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Error == nil {
		t.Fatalf("no error in the envelope: %v\n%s", err, out)
	}
	return env.Error
}

// stateTree is every file under the state directory with its bytes (-shm
// aside: lock state), what a command that claims to read only must leave.
func stateTree(t *testing.T, d paths.Dirs) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(d.State, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || strings.HasSuffix(p, "-shm") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatal(rerr)
		}
		out[p] = string(b)
		return nil
	})
	return out
}

func sameStateTree(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	var names []string
	for p := range before {
		names = append(names, p)
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	for _, p := range names {
		b, inB := before[p]
		a, inA := after[p]
		if inB != inA || a != b {
			t.Errorf("%s: %s changed (before %v, after %v)", what, p, inB, inA)
		}
	}
}

// catchUpIndex builds and catches up the index under home.
func catchUpIndex(t *testing.T, home string) {
	t.Helper()
	ix, err := index.Open(context.Background(), indexOptions(pathsOf(home), home, nil))
	if err != nil {
		t.Fatal(err)
	}
	// Until caught up: a slow machine (the race detector) may stop a
	// run at its budget, and every run goes on from the last one.
	for run := 1; ; run++ {
		fr, err := ix.CatchUp(context.Background(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !fr.Partial {
			break
		}
		if run == 20 {
			t.Fatalf("still partial after %d runs: %+v", run, fr)
		}
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendTelemetry(t *testing.T, d paths.Dirs, month, content string) {
	t.Helper()
	if err := os.MkdirAll(d.TelemetryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(d.TelemetryDir(), "events-"+month+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// markUpdate writes an uncommitted update marker.
func markUpdate(t *testing.T, home string) {
	t.Helper()
	if err := state.WriteUpdateMarker(pathsOf(home), state.UpdateMarker{}); err != nil {
		t.Fatal(err)
	}
}

const updateSentence = "an update is being applied, try again in a minute"

func TestStatsIndexNotBuilt(t *testing.T) {
	for _, withUpdate := range []bool{false, true} {
		home := devInstall(t, true)
		d := pathsOf(home)
		if withUpdate {
			markUpdate(t, home)
		}
		before := stateTree(t, d)
		code, out, errOut := run(t, "stats", "index")
		if code != output.OK || !strings.Contains(out, "Index        not built yet: the next jobs tick builds it, or backsight stats index rebuild\n") || errOut != "" {
			t.Fatalf("update %v: exit %d\n%s%s", withUpdate, code, out, errOut)
		}
		if strings.Contains(out, home) {
			t.Errorf("a path in the output:\n%s", out)
		}
		code, out, _ = run(t, "stats", "index", "--json")
		var st index.Status
		jsonData(t, out, &st)
		if code != output.OK || st.Exists || st.Readable || st.Head != 1 || st.Using != "metrics.db" {
			t.Errorf("update %v: exit %d %+v", withUpdate, code, st)
		}
		sameStateTree(t, "stats index", before, stateTree(t, d))
		if _, err := os.Stat(filepath.Join(d.IndexDir(), "metrics.db")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stats index created the index (%v)", err)
		}
	}
}

// ingestedFixture builds an index of two month files, then loses one,
// appends a line to the other and leaves two lines the ingest skipped.
func ingestedFixture(t *testing.T) (home string, d paths.Dirs) {
	t.Helper()
	home = devInstall(t, true)
	d = pathsOf(home)
	old := time.Now().Add(-40 * 24 * time.Hour)
	recent := time.Now().Add(-2 * time.Hour)
	writeIndexLines(t, d, monthOf(old), 2, old)
	writeIndexLines(t, d, monthOf(recent), 3, recent)
	appendTelemetry(t, d, monthOf(recent), "not json\n"+`{"event":"prompt"}`+"\n")
	catchUpIndex(t, home)
	if err := os.Remove(filepath.Join(d.TelemetryDir(), "events-"+monthOf(old)+".jsonl")); err != nil {
		t.Fatal(err)
	}
	catchUpIndex(t, home) // the old month is marked missing
	writeIndexLines(t, d, monthOf(recent), 1, time.Now().Add(-time.Hour))
	return home, d
}

func TestStatsIndexShowsItsState(t *testing.T) {
	home, d := ingestedFixture(t)
	rows := indexCount(t, home, `SELECT count(*) FROM tel_prompt`)
	before := stateTree(t, d)

	code, out, errOut := run(t, "stats", "index", "--json")
	var st index.Status
	jsonData(t, out, &st)
	if code != output.OK || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !st.Exists || !st.Readable || st.Schema != 1 || st.Head != 1 || st.Newer || st.Using != "metrics.db" || st.JournalMode != "wal" {
		t.Errorf("%+v", st)
	}
	if st.Errors30d != 2 || st.Errors24h != 2 || st.ErrorsByReason["unparsable"] != 1 || st.ErrorsByReason["no_ts"] != 1 {
		t.Errorf("errors %d/%d %v", st.Errors30d, st.Errors24h, st.ErrorsByReason)
	}
	var gone, behind index.SourceLag
	for _, s := range st.Sources {
		if s.Missing {
			gone = s
		} else {
			behind = s
		}
	}
	if len(st.Sources) != 2 || !strings.HasSuffix(gone.File, "-"+monthOf(time.Now().Add(-40*24*time.Hour))+".jsonl") || behind.LagSeconds <= 0 || behind.Offset >= behind.Size {
		t.Errorf("sources %+v", st.Sources)
	}
	if st.IngestedAt.IsZero() {
		t.Error("no ingested_at")
	}

	code, out, _ = run(t, "stats", "index")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, re := range []string{
		`(?m)^Index        metrics\.db \([^)]+, schema 1 of 1\)$`,
		`(?m)^Ingested     \d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC \(lag \d+ s\)$`,
		`(?m)^Sources      2 file\(s\), 1 behind, 1 gone \(rows kept\)$`,
		`(?m)^Errors       2 line\(s\) skipped in the last 30 days \(no_ts 1, unparsable 1\)$`,
		`(?m)^Side files   none$`,
		`(?m)^Set aside    none$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("no match for %s:\n%s", re, out)
		}
	}
	if strings.Contains(out, home) || strings.Contains(out, "0001") {
		t.Errorf("a path or a migration id in the output:\n%s", out)
	}
	if got := indexCount(t, home, `SELECT count(*) FROM tel_prompt`); got != rows {
		t.Errorf("stats index ingested: %d rows, was %d", got, rows)
	}
	sameStateTree(t, "stats index", before, stateTree(t, d))
}

// A corrupt index is reported, never set aside or rebuilt by looking.
func TestStatsIndexOnACorruptIndex(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	if err := os.MkdirAll(d.IndexDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.IndexDir(), "metrics.db"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeIndexLines(t, d, monthOf(time.Now()), 2, time.Now())
	// a stop since an automatic rebuild failed again
	stop := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(d.IndexDir(), "status.json"), []byte(`{"stopped_at":"`+stop+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := stateTree(t, d)
	code, out, errOut := run(t, "stats", "index")
	if code != output.OK || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"Index        metrics.db (14 bytes, unreadable)\n", "Stopped      unreadable since " + stop[:10], ": run backsight stats index rebuild\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if code, out, _ = run(t, "stats", "index", "--json"); code != output.OK || !strings.Contains(out, `"readable": false`) || !strings.Contains(out, `"stopped_at"`) {
		t.Errorf("exit %d\n%s", code, out)
	}
	sameStateTree(t, "stats index", before, stateTree(t, d))
}

// testRegistry is a registry of two migrations: the real first one, and
// a second that adds a table.
func testRegistry(t *testing.T, second string) fs.FS {
	t.Helper()
	first, err := fs.ReadFile(index.Embedded, "0001-initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{"0001-initial.sql": {Data: first}, "0002-test.sql": {Data: []byte(second)}}
}

func withRegistry(t *testing.T, reg fs.FS) {
	t.Helper()
	old := indexRegistry
	indexRegistry = func() fs.FS { return reg }
	t.Cleanup(func() { indexRegistry = old })
}

func TestStatsIndexNamesSideAndAsideFiles(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	// a newer backsight migrated metrics.db to schema 2
	withRegistry(t, testRegistry(t, "CREATE TABLE later(x)"))
	catchUpIndex(t, home)
	indexRegistry = func() fs.FS { return index.Embedded } // this binary: head 1
	catchUpIndex(t, home)                                  // builds its side file
	if err := os.WriteFile(filepath.Join(d.IndexDir(), "metrics.db.corrupt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := stateTree(t, d)
	code, out, _ := run(t, "stats", "index", "--json")
	var st index.Status
	jsonData(t, out, &st)
	if code != output.OK || !st.Newer || st.Using != "metrics-v1.db" || len(st.SideFiles) != 1 || st.SideFiles[0] != "metrics-v1.db" ||
		len(st.AsideFiles) != 1 || st.AsideFiles[0] != "metrics.db.corrupt" || st.Schema != 1 {
		t.Fatalf("exit %d %+v", code, st)
	}
	_, out, _ = run(t, "stats", "index")
	for _, want := range []string{
		"Index        metrics-v1.db (",
		"Newer        a newer backsight's index: this one uses metrics-v1.db\n",
		"Side files   metrics-v1.db\n",
		"Set aside    metrics.db.corrupt\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	sameStateTree(t, "stats index", before, stateTree(t, d))
}

// The text of a state the filesystem cannot give a test: a network or
// synced folder.
func TestIndexStatusTextNamesTheFolder(t *testing.T) {
	st := index.Status{Using: "metrics.db", File: "/x/metrics.db", Exists: true, Readable: true, Schema: 1, Head: 1, JournalMode: "delete",
		Filesystem: index.FSInfo{Kind: index.FSNetwork, Name: "nfs"}}
	out := indexStatusText(st)
	if !strings.Contains(out, "Folder       nfs (network or synced): the index runs without its write-ahead log\n") {
		t.Errorf("%s", out)
	}
	st.Filesystem = index.FSInfo{Kind: index.FSLocal}
	if out := indexStatusText(st); strings.Contains(out, "Folder") || strings.Contains(out, "Newer") || strings.Contains(out, "Stopped") {
		t.Errorf("lines that do not apply:\n%s", out)
	}
	if out := indexStatusText(st); !strings.Contains(out, "Ingested     nothing yet\n") {
		t.Errorf("%s", out)
	}
}

func TestIndexErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
		msg  string
	}{
		{index.ErrUpdateInProgress, output.Blocked, updateSentence},
		{index.ErrInUse, output.Blocked, index.ErrInUse.Error()},
		{index.ErrNeedsYes, output.NeedsUser, "rebuilding drops the months listed, whose files are gone: confirm with backsight stats index rebuild --yes"},
		{index.ErrUnreadable, output.Blocked, "the metrics index is unreadable and rebuilding it failed today: run backsight stats index rebuild"},
		{index.ErrDiskFull, output.Blocked, "the disk is full: the metrics index was left as it was"},
		{&fs.PathError{Op: "write", Path: "/home/someone/metrics.db", Err: syscall.ENOSPC}, output.Blocked, "the disk is full: the metrics index was left as it was"},
		{fmt.Errorf("ingest x: %w", index.ErrSchemaChanged), output.Blocked, "backsight was updated meanwhile: run the command again"},
		{errors.New("open /home/someone/secret/metrics.db: permission denied"), output.Blocked, "the metrics index could not be read (other): run backsight stats index rebuild"},
		{&fs.PathError{Op: "open", Path: "/home/someone/metrics.db", Err: fs.ErrPermission}, output.Blocked, "the metrics index could not be read (io): run backsight stats index rebuild"},
	}
	for _, c := range cases {
		err := indexError(c.err)
		var oe *output.Error
		if !errors.As(err, &oe) || oe.Code != c.code || oe.Message != c.msg {
			t.Errorf("%v: %#v", c.err, err)
			continue
		}
		if errors.Is(c.err, index.ErrNeedsYes) && (len(oe.Actions) != 1 || oe.Actions[0] != (output.Action{Kind: "run_as_user", Cmd: "backsight stats index rebuild --yes", Why: "rebuild without those months"})) {
			t.Errorf("actions %+v", oe.Actions)
		}
		if !errors.Is(c.err, index.ErrNeedsYes) && len(oe.Actions) != 0 {
			t.Errorf("%v: actions %+v", c.err, oe.Actions)
		}
	}
}

// rebuildFixture is an index whose older month file is gone after it was
// ingested, with the rows that loses.
func rebuildFixture(t *testing.T, bigLines int) (home string, d paths.Dirs, lostMonth string) {
	t.Helper()
	home = devInstall(t, true)
	d = pathsOf(home)
	old := time.Now().Add(-40 * 24 * time.Hour)
	lostMonth = monthOf(old)
	writeIndexLines(t, d, lostMonth, 3, old)
	writeIndexLines(t, d, monthOf(time.Now()), max(bigLines, 2), time.Now().Add(-time.Hour))
	catchUpIndex(t, home)
	if err := os.Remove(filepath.Join(d.TelemetryDir(), "events-"+lostMonth+".jsonl")); err != nil {
		t.Fatal(err)
	}
	return home, d, lostMonth
}

func TestStatsIndexRebuildPlan(t *testing.T) {
	_, d, lost := rebuildFixture(t, 0)
	before := stateTree(t, d)
	code, out, errOut := run(t, "stats", "index", "rebuild", "--plan")
	if code != output.OK || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if want := lost + "  telemetry/events-" + lost + ".jsonl  3 row(s) would be lost\n"; !strings.Contains(out, want) {
		t.Errorf("lacks %q:\n%s", want, out)
	}
	code, out, _ = run(t, "stats", "index", "rebuild", "--plan", "--json")
	var res index.RebuildResult
	jsonData(t, out, &res)
	if code != output.OK || !res.Plan || !res.NeedsYes || len(res.Lost) != 1 || res.Lost[0].Month != lost || res.Lost[0].Rows != 3 {
		t.Errorf("exit %d %+v", code, res)
	}
	sameStateTree(t, "rebuild --plan", before, stateTree(t, d))
}

func TestStatsIndexRebuildPlanWithNothingLost(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	writeIndexLines(t, d, monthOf(time.Now()), 2, time.Now())
	catchUpIndex(t, home)
	before := stateTree(t, d)
	code, out, _ := run(t, "stats", "index", "rebuild", "--plan")
	if code != output.OK || !strings.Contains(out, "no month would be lost\n") {
		t.Errorf("exit %d\n%s", code, out)
	}
	sameStateTree(t, "rebuild --plan", before, stateTree(t, d))
}

func TestStatsIndexRebuildNeedsYesWhenMonthsWouldBeLost(t *testing.T) {
	_, d, lost := rebuildFixture(t, 0)
	main := filepath.Join(d.IndexDir(), "metrics.db")
	was, _ := os.ReadFile(main)
	code, out, errOut := run(t, "stats", "index", "rebuild", "--json")
	if code != output.NeedsUser {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	oe := jsonError(t, out)
	if len(oe.Actions) != 1 || oe.Actions[0].Cmd != "backsight stats index rebuild --yes" || oe.Actions[0].Kind != "run_as_user" {
		t.Errorf("actions %+v", oe.Actions)
	}
	if now, _ := os.ReadFile(main); string(now) != string(was) {
		t.Error("the index changed without --yes")
	}
	// text: the months, then the sentence
	code, out, errOut = run(t, "stats", "index", "rebuild")
	if code != output.NeedsUser || !strings.Contains(out, lost+"  telemetry/") || !strings.Contains(errOut, "confirm with backsight stats index rebuild --yes") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if now, _ := os.ReadFile(main); string(now) != string(was) {
		t.Error("the index changed without --yes")
	}
}

func TestStatsIndexRebuildYes(t *testing.T) {
	home, d, lost := rebuildFixture(t, 9000) // over 1 MiB: the rebuild reports its progress
	code, out, errOut := run(t, "stats", "index", "rebuild", "--yes")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !regexp.MustCompile(`(?m)^metrics index rebuilt: 9000 lines in \d+\.\d s$`).MatchString(out) {
		t.Errorf("stdout:\n%s", out)
	}
	if !strings.Contains(errOut, "metrics index: ") || !strings.Contains(errOut, "MB") {
		t.Errorf("no progress on stderr:\n%s", errOut)
	}
	if strings.Contains(out, "metrics index: ready") {
		t.Errorf("progress on stdout:\n%s", out)
	}
	if got := indexCount(t, home, `SELECT count(*) FROM tel_prompt`); got != 9000 {
		t.Errorf("%d rows after the rebuild, want the 9000 of the file that is left", got)
	}
	if code, out, _ = run(t, "stats", "index", "rebuild", "--plan"); code != output.OK || !strings.Contains(out, "no month would be lost") || strings.Contains(out, lost) {
		t.Errorf("exit %d\n%s", code, out)
	}
	_ = d
}

func TestStatsIndexRebuildWithNothingLostNeedsNoYes(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	writeIndexLines(t, d, monthOf(time.Now()), 4, time.Now())
	catchUpIndex(t, home)
	code, out, errOut := run(t, "stats", "index", "rebuild", "--json")
	var res index.RebuildResult
	jsonData(t, out, &res)
	if code != output.OK || res.Lines != 4 || res.NeedsYes || res.Plan {
		t.Errorf("exit %d %+v\n%s", code, res, errOut)
	}
}

func TestStatsIndexRebuildWaitsForAnUpdate(t *testing.T) {
	home, d, _ := rebuildFixture(t, 0)
	markUpdate(t, home)
	before := stateTree(t, d)
	code, out, errOut := run(t, "stats", "index", "rebuild", "--yes")
	if code != output.Blocked || !strings.Contains(errOut, updateSentence) || strings.Contains(out, "rebuilt") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	code, out, _ = run(t, "stats", "index", "rebuild", "--yes", "--json")
	if oe := jsonError(t, out); code != output.Blocked || oe.Message != updateSentence {
		t.Errorf("exit %d %+v", code, oe)
	}
	sameStateTree(t, "rebuild", before, stateTree(t, d))
}

// Another handle on the index (the dashboard's, a tick's): the rebuild
// refuses, says what to do, and the old index stays.
func TestStatsIndexRebuildRefusesAnIndexInUse(t *testing.T) {
	home, d, _ := rebuildFixture(t, 0)
	ix, err := index.Open(context.Background(), indexOptions(d, home, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	main := filepath.Join(d.IndexDir(), "metrics.db")
	was, _ := os.ReadFile(main)
	code, out, errOut := run(t, "stats", "index", "rebuild", "--yes")
	if code != output.Blocked || !strings.Contains(errOut, "close the dashboard and run backsight stats index rebuild again; the old index is kept") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if now, _ := os.ReadFile(main); string(now) != string(was) {
		t.Error("the old index changed")
	}
	tmps, _ := filepath.Glob(main + ".tmp-*")
	if len(tmps) != 0 {
		t.Errorf("temp files left: %v", tmps)
	}
}

func TestStatsIndexMigrateIsHidden(t *testing.T) {
	devInstall(t, true)
	if _, out, _ := run(t, "stats", "index", "--help"); strings.Contains(out, "migrate") || !strings.Contains(out, "rebuild") {
		t.Errorf("help:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "--help"); strings.Contains(out, "migrate") {
		t.Errorf("stats help:\n%s", out)
	}
}

func TestStatsIndexMigrate(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	// states that migrate nothing
	if code, out, _ := run(t, "stats", "index", "migrate"); code != output.OK || out != "metrics index: not built yet\n" {
		t.Errorf("not built: exit %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(d.IndexDir(), "metrics.db")); err == nil {
		t.Fatal("migrate built the index")
	}
	writeIndexLines(t, d, monthOf(time.Now()), 3, time.Now().Add(-time.Hour))
	catchUpIndex(t, home)
	if code, out, _ := run(t, "stats", "index", "migrate"); code != output.OK || out != "metrics index: up to date\n" {
		t.Errorf("up to date: exit %d %q", code, out)
	}

	// a binary of head 2
	withRegistry(t, testRegistry(t, "CREATE TABLE added(x)"))
	writeIndexLines(t, d, monthOf(time.Now()), 2, time.Now()) // unread lines: migrate ingests nothing
	before := stateTree(t, d)
	code, out, _ := run(t, "stats", "index", "migrate", "--plan", "--json")
	var res index.MigrateResult
	jsonData(t, out, &res)
	if code != output.OK || !res.Built || len(res.Pending) != 1 || res.Pending[0] != "0002-test" || len(res.Applied) != 0 {
		t.Fatalf("plan: exit %d %+v", code, res)
	}
	if code, out, _ = run(t, "stats", "index", "migrate", "--plan"); code != output.OK || out != "metrics index: pending 0002-test\n" {
		t.Errorf("plan text: exit %d %q", code, out)
	}
	sameStateTree(t, "migrate --plan", before, stateTree(t, d))

	code, out, errOut := run(t, "stats", "index", "migrate")
	if code != output.OK || out != "metrics index: applied 0002-test\n" {
		t.Fatalf("exit %d %q %q", code, out, errOut)
	}
	if got := indexCount(t, home, `SELECT count(*) FROM tel_prompt`); got != 3 {
		t.Errorf("%d prompt rows: the migration ingested (3 were there)", got)
	}
	if got := indexCount(t, home, `SELECT count(*) FROM sqlite_schema WHERE name = 'added'`); got != 1 {
		t.Error("the migration did not run")
	}
	if code, out, _ = run(t, "stats", "index", "migrate"); code != output.OK || out != "metrics index: up to date\n" {
		t.Errorf("again: exit %d %q", code, out)
	}
}

func TestStatsIndexMigrateLeavesANewerIndex(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	withRegistry(t, testRegistry(t, "CREATE TABLE later(x)"))
	catchUpIndex(t, home)
	indexRegistry = func() fs.FS { return index.Embedded }
	before := stateTree(t, d)
	code, out, _ := run(t, "stats", "index", "migrate")
	if code != output.OK || out != "metrics index: a newer backsight's index: nothing to migrate\n" {
		t.Errorf("exit %d %q", code, out)
	}
	sameStateTree(t, "migrate", before, stateTree(t, d))
}

func TestStatsIndexMigrateWaitsForAnUpdate(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	catchUpIndex(t, home)
	withRegistry(t, testRegistry(t, "CREATE TABLE added(x)"))
	markUpdate(t, home)
	before := stateTree(t, d)
	code, _, errOut := run(t, "stats", "index", "migrate")
	if code != output.Blocked || !strings.Contains(errOut, updateSentence) {
		t.Errorf("exit %d %q", code, errOut)
	}
	code, out, _ := run(t, "stats", "index", "migrate", "--json")
	if oe := jsonError(t, out); code != output.Blocked || oe.Message != updateSentence {
		t.Errorf("exit %d %+v", code, oe)
	}
	sameStateTree(t, "migrate", before, stateTree(t, d))
}

func migrateStatus(t *testing.T, d paths.Dirs) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(d.IndexDir(), "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStatsIndexMigrateRecordsAFailure(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	catchUpIndex(t, home)
	withRegistry(t, testRegistry(t, "CREATE TABLE broken("))
	code, out, errOut := run(t, "stats", "index", "migrate")
	if code != output.Blocked || out != "" || !strings.Contains(errOut, "the metrics index could not be read (other): run backsight stats index rebuild") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if strings.Contains(errOut, "broken") || strings.Contains(errOut, home) {
		t.Errorf("raw error text:\n%s", errOut)
	}
	st := migrateStatus(t, d)
	if st["migrate_failed_at"] == nil || st["migrate_reason"] != "other" {
		t.Errorf("status.json: %v", st)
	}
	// stats index shows when it failed
	if _, out, _ := run(t, "stats", "index", "--json"); !strings.Contains(out, `"migrate_failed_at"`) {
		t.Errorf("stats index lacks the failure:\n%s", out)
	}
	// a later success clears it
	withRegistry(t, testRegistry(t, "CREATE TABLE fixed(x)"))
	if code, out, _ := run(t, "stats", "index", "migrate"); code != output.OK || out != "metrics index: applied 0002-test\n" {
		t.Fatalf("exit %d %q", code, out)
	}
	if st := migrateStatus(t, d); st["migrate_failed_at"] != nil || st["migrate_reason"] != nil {
		t.Errorf("not cleared: %v", st)
	}
}

// The command bounds the migration by indexMigrateBudget.
func TestStatsIndexMigrateIsBounded(t *testing.T) {
	devInstall(t, true)
	oldBudget, oldMigrate := indexMigrateBudget, indexMigrate
	t.Cleanup(func() { indexMigrateBudget, indexMigrate = oldBudget, oldMigrate })
	if oldBudget != 60*time.Second {
		t.Fatalf("budget %v", oldBudget)
	}
	indexMigrateBudget = 7 * time.Second
	var left time.Duration
	var has bool
	indexMigrate = func(ctx context.Context, o index.Options, plan bool) (index.MigrateResult, error) {
		dl, ok := ctx.Deadline()
		has, left = ok, time.Until(dl)
		return index.Migrate(ctx, o, plan)
	}
	if code, _, _ := run(t, "stats", "index", "migrate"); code != output.OK {
		t.Fatalf("exit %d", code)
	}
	if !has || left <= 5*time.Second || left > 7*time.Second {
		t.Errorf("deadline %v (set %v)", left, has)
	}
	// a migration past the budget is a failure, not a hang
	indexMigrateBudget = time.Nanosecond
	indexMigrate = func(ctx context.Context, o index.Options, plan bool) (index.MigrateResult, error) {
		<-ctx.Done()
		return index.MigrateResult{}, ctx.Err()
	}
	if code, _, errOut := run(t, "stats", "index", "migrate"); code != output.Blocked || !strings.Contains(errOut, "the metrics index could not be read (") {
		t.Errorf("exit %d %q", code, errOut)
	}
}

func TestStatsDaysStillWorks(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	if code, out, _ := run(t, "stats", "14"); code != output.OK || !strings.Contains(out, "last 14 day(s)") {
		t.Errorf("stats 14: exit %d\n%s", code, out)
	}
	code, out, errOut := run(t, "stats", "x")
	if code != output.Usage || !strings.Contains(errOut+out, `unknown view "x": one of overview, quality, cost, bank, pipeline, system, releases, projects, sessions, memories, days, defs, index`) {
		t.Errorf("stats x: exit %d\n%s%s", code, out, errOut)
	}
	if code, _, _ := run(t, "stats", "index", "extra"); code != output.Usage {
		t.Errorf("stats index extra: exit %d", code)
	}
}

// A last line without its newline is still being written: not behind.
func TestIndexStatusTextCountsOnlyCompleteLinesAsBehind(t *testing.T) {
	st := index.Status{Using: "metrics.db", Exists: true, Readable: true, Schema: 1, Head: 1, Sources: []index.SourceLag{
		{File: "a", Size: 100, Offset: 90},                              // partial tail
		{File: "b", Size: 100, Offset: 10, LagSeconds: 5, Behind: true}, // lines left
	}}
	if out := indexStatusText(st); !strings.Contains(out, "Sources      2 file(s), 1 behind, 0 gone") {
		t.Errorf("%s", out)
	}
}

// A full disk and a busy index keep their own sentences; any other class
// names itself in a plain sentence with the way out.
func TestIndexClassMessage(t *testing.T) {
	for class, want := range map[string]string{
		"full":    "the disk is full: the metrics index was left as it was",
		"busy":    index.ErrInUse.Error(),
		"corrupt": "the metrics index could not be read (corrupt): run backsight stats index rebuild",
		"io":      "the metrics index could not be read (io): run backsight stats index rebuild",
	} {
		if got := indexClassMessage(class); got != want {
			t.Errorf("%s: %q, want %q", class, got, want)
		}
	}
}
