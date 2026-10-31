package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const rebuildFix = "backsight stats index rebuild"

func TestMetricsIndexCheck(t *testing.T) {
	built := IndexFacts{Built: true, Schema: 3, Head: 3, Size: 3 << 20, LagSeconds: 42}
	at := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		f          IndexFacts
		err        error
		tick       bool
		status     Status
		summary    string
		fix        string
		detail     string
		notSummary string
	}{
		{name: "healthy", f: built, status: OK, summary: "schema 3, 3.0 MB, 42 s behind"},
		{name: "unreadable", f: built, err: errors.New("corrupt"), status: Warn, summary: "cannot read the metrics index (corrupt)", fix: rebuildFix},
		{name: "not built", f: IndexFacts{}, status: Info, summary: "not built yet: the next jobs tick builds it"},
		{name: "stopped", f: IndexFacts{Built: true, StoppedAt: at}, status: Warn, summary: "unreadable, and the automatic rebuild failed (2026-10-30T08:00:00Z)", fix: rebuildFix},
		{name: "migrate failed", f: IndexFacts{Built: true, Schema: 2, Head: 3, MigrateFailedAt: at}, status: Warn,
			summary: "not migrated to this backsight's schema (2 of 3): the last attempt failed at 2026-10-30T08:00:00Z", fix: rebuildFix},
		{name: "migrate failed but migrated since", f: IndexFacts{Built: true, Schema: 3, Head: 3, MigrateFailedAt: at}, status: OK, summary: "schema 3"},
		{name: "network folder", f: IndexFacts{Built: true, Schema: 3, Head: 3, Folder: "nfs"}, status: Warn,
			summary: "the state directory is on nfs (a network or synced folder): the index runs without its write-ahead log", fix: "keep backsight's state directory on a local disk"},
		{name: "errors", f: IndexFacts{Built: true, Schema: 3, Head: 3, Errors24h: 5, ErrorsByReason: map[string]int{"too_long": 1, "bad_type": 4}}, status: Warn,
			summary: "5 line(s) of telemetry or journal skipped in the last 24 h (bad_type 4, too_long 1)", fix: rebuildFix},
		{name: "lag with ticks", f: IndexFacts{Built: true, Schema: 3, Head: 3, LagSeconds: 7300}, tick: true, status: Warn, summary: "2 h behind while jobs tick runs", fix: rebuildFix},
		{name: "lag without ticks", f: IndexFacts{Built: true, Schema: 3, Head: 3, LagSeconds: 7300}, status: OK, summary: "7300 s behind"},
		{name: "lag at the limit", f: IndexFacts{Built: true, Schema: 3, Head: 3, LagSeconds: 3600}, tick: true, status: OK, summary: "3600 s behind"},
		{name: "newer", f: IndexFacts{Built: true, Newer: true, Schema: 5, Head: 3}, status: Info, summary: "a newer backsight's index: this one uses its own file beside it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkMetricsIndex(c.f, c.err, c.tick)
			if got.ID != "metrics-index" || got.Status != c.status || !strings.Contains(got.Summary, c.summary) || got.Fix != c.fix {
				t.Errorf("%+v", got)
			}
			if c.status == Warn && c.summary != got.Summary {
				t.Errorf("summary %q", got.Summary)
			}
		})
	}

	t.Run("first warning in the summary, the others in the detail", func(t *testing.T) {
		f := IndexFacts{Built: true, Schema: 3, Head: 3, Folder: "cifs", Errors24h: 2, ErrorsByReason: map[string]int{"schema": 2}, LagSeconds: 9000}
		got := checkMetricsIndex(f, nil, true)
		if got.Status != Warn || !strings.Contains(got.Summary, "cifs") || strings.Contains(got.Summary, "skipped") {
			t.Fatalf("summary %q", got.Summary)
		}
		if !strings.Contains(got.Detail, "2 line(s) of telemetry or journal skipped") || !strings.Contains(got.Detail, "behind while jobs tick runs") {
			t.Errorf("detail %q", got.Detail)
		}
	})
}
