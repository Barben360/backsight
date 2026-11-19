package metrics

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// TestReadsUseTheirIndexes: the reads migration 0007 indexed use those
// indexes. The days calendar's grouped read of 13 months of prompts
// stays in the covering index tel_prompt_calendar (it never visits
// tel_prompt's wide rows), with or without a project; document_status's
// window reads tel_retain_document in its order (no sort, no visit of the
// table's rows), and the backlog age's waits read tel_retain by document
// through it, without visiting its rows either. The plans are of the SQL the code runs
// (calendarPromptsSQL, documentStatusSQL, currentWaitsSQL), so a read that
// loses its index, or an index that stops covering it, turns this red.
func TestReadsUseTheirIndexes(t *testing.T) {
	x := newFx(t, fxNow)
	src := x.source()
	start := x.now.AddDate(0, -calendarMonths, -7)
	day := localDayExpr(start, x.now, fxParis)("ts")
	cases := []struct {
		name, sql string
		args      []any
		want      []string // plan lines that must be there
		never     string   // a plan line prefix that must not be (a scan, a sort)
	}{
		{"calendar, every project", calendarPromptsSQL(day), []any{start.UnixMilli(), x.now.UnixMilli(), "", ""},
			[]string{"SEARCH tel_prompt USING COVERING INDEX tel_prompt_calendar (ts>? AND ts<?)"}, "SCAN tel_prompt"},
		{"calendar, one project", calendarPromptsSQL(day), []any{start.UnixMilli(), x.now.UnixMilli(), "p", "p"},
			[]string{"SEARCH tel_prompt USING COVERING INDEX tel_prompt_calendar (ts>? AND ts<?)"}, "SCAN tel_prompt"},
		{"document_status", documentStatusSQL, nil,
			[]string{"SEARCH tel_retain USING COVERING INDEX tel_retain_document (document>?)"}, "USE TEMP B-TREE"},
		{"backlog age's waits", currentWaitsSQL, nil,
			[]string{
				"SEARCH tel_retain USING COVERING INDEX tel_retain_document (document>?)", // the last end of a wait, per document
				"SEARCH r USING COVERING INDEX tel_retain_document (document>?)",
			}, "SCAN r"},
	}
	err := src.Index.ReadTx(context.Background(), func(tx *sql.Tx) error {
		for _, c := range cases {
			plan := explain(t, tx, c.sql, c.args)
			for _, w := range c.want {
				if !slices.Contains(plan, w) {
					t.Errorf("%s: plan %q, want the line %q", c.name, plan, w)
				}
			}
			if slices.ContainsFunc(plan, func(l string) bool { return strings.HasPrefix(l, c.never) }) {
				t.Errorf("%s: plan %q has a %q", c.name, plan, c.never)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// explain is the detail lines of q's EXPLAIN QUERY PLAN with args.
func explain(t *testing.T, tx *sql.Tx, q string, args []any) []string {
	t.Helper()
	rows, err := tx.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}
