package cli

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
)

func TestDoctorRunsTheMetricsIndexCheck(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)

	if c := findCheck(doctorReport(t), "metrics-index"); c == nil || c.Status != doctor.Info || !strings.Contains(c.Summary, "not built yet") {
		t.Errorf("before the first build: %+v", c)
	}
	if onDisk(d.IndexDir()) {
		t.Fatal("doctor built the index")
	}

	writeIndexLines(t, d, monthOf(time.Now()), 2, time.Now().Add(-time.Minute))
	catchUpIndex(t, home)
	if c := findCheck(doctorReport(t), "metrics-index"); c == nil || c.Status != doctor.OK || !strings.HasPrefix(c.Summary, "schema ") {
		t.Errorf("a built index: %+v", c)
	}

	stop := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(d.IndexDir(), "status.json"), []byte(`{"stopped_at":"`+stop+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := findCheck(doctorReport(t), "metrics-index")
	if c == nil || c.Status != doctor.Warn || !strings.Contains(c.Summary, "automatic rebuild failed ("+stop) || c.Fix != "backsight stats index rebuild" {
		t.Errorf("stopped: %+v", c)
	}
}

// A read failure shows its class, never an error's text: doctor output goes
// into the report bundles users paste into issues.
func TestDoctorMetricsIndexShowsOnlyTheFailureClass(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	if err := os.MkdirAll(d.IndexDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.IndexDir(), "metrics.db"), []byte("not a database, "+home), 0o600); err != nil {
		t.Fatal(err)
	}
	c := findCheck(doctorReport(t), "metrics-index")
	if c == nil || c.Status != doctor.Warn || c.Summary != "cannot read the metrics index (corrupt)" || c.Fix != "backsight stats index rebuild" {
		t.Fatalf("%+v", c)
	}
	if strings.Contains(c.Summary+c.Detail, home) {
		t.Errorf("a path in %+v", c)
	}
}

func TestDoctorMetricsIndexCountsTheSkippedLinesByReason(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	writeIndexLines(t, d, monthOf(time.Now()), 1, time.Now().Add(-time.Hour))
	appendTelemetry(t, d, monthOf(time.Now()), "not json\n"+`{"event":"prompt"}`+"\n")
	catchUpIndex(t, home)
	c := findCheck(doctorReport(t), "metrics-index")
	if c == nil || c.Status != doctor.Warn || !strings.Contains(c.Summary, "2 line(s) of telemetry or journal skipped in the last 24 h (") {
		t.Errorf("%+v", c)
	}
}

// Files that hold the canary under index/, metrics/ and dashboard/: report
// reads none of them.
func TestReportLeavesTheIndexOut(t *testing.T) {
	p := plantedMachine(t)
	d := pathsOf(p.home)
	const canary = "R3P0RT-CANARY"
	writeIndexLines(t, d, monthOf(time.Now()), 1, time.Now().Add(-time.Hour))
	catchUpIndex(t, p.home)
	db, err := sql.Open("sqlite", filepath.Join(d.IndexDir(), index.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO events (source_file, "offset", ts, kind) VALUES ('canary.jsonl', 0, ?, ?)`, time.Now().UnixMilli(), canary); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	for _, f := range []string{
		filepath.Join(d.IndexDir(), "status.json"), filepath.Join(d.MetricsDir(), "x.jsonl"), filepath.Join(d.DashboardDir(), "dashboard.json"),
	} {
		_ = os.MkdirAll(filepath.Dir(f), 0o700)
		if err := os.WriteFile(f, []byte(`{"kind":"`+canary+`"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"report"}, {"report", "--json"}} {
		code, out, errOut := run(t, args...)
		if code != output.OK {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
		if all := out + errOut + issueBodies(t, out); strings.Contains(all, canary) {
			t.Errorf("%v shows the index's canary:\n%s", args, out)
		}
	}
}

// A read of the index that hangs is bounded: doctor reports the class
// timeout instead of waiting.
func TestDoctorMetricsIndexReadIsBounded(t *testing.T) {
	prevInspect, prevTimeout := inspectIndex, indexFactsTimeout
	t.Cleanup(func() { inspectIndex, indexFactsTimeout = prevInspect, prevTimeout })
	indexFactsTimeout = 50 * time.Millisecond
	inspectIndex = func(ctx context.Context, _ index.Options) (index.Status, error) {
		select {
		case <-ctx.Done():
			return index.Status{}, &fs.PathError{Op: "open", Path: "/home/CANARY-index-path", Err: ctx.Err()}
		case <-time.After(10 * time.Second):
			return index.Status{}, nil
		}
	}
	start := time.Now()
	_, err := indexFacts(context.Background(), pathsOf(t.TempDir()), t.TempDir())
	if err == nil || err.Error() != "timeout" {
		t.Errorf("err %v, want the class timeout", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("waited %v", d)
	}
}
