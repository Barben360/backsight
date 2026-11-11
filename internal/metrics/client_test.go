package metrics

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The filter shadows every table that has a client column, and only those:
// a table 0006 gave the column and the list forgot would count every
// tool's rows under a filter.
func TestClientTablesAreTheSchemasClientColumns(t *testing.T) {
	x := newFx(t, fxNow)
	var got []string
	err := withTx(context.Background(), x.source(), x.query(7), nil, func(e env) error {
		rows, err := e.tx.QueryContext(e.ctx, `SELECT m.name FROM main.sqlite_master m, pragma_table_info(m.name) p WHERE m.type = 'table' AND p.name = 'client' ORDER BY 1`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			got = append(got, n)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := slices.Sorted(slices.Values(clientTables)); !slices.Equal(got, want) {
		t.Errorf("tables with a client column %v, the filter's %v", got, want)
	}
	if err := withTx(context.Background(), x.source(), Query{Window: x.window(7), Client: "cursor"}, nil, func(env) error { return nil }); err == nil {
		t.Error("an unknown client filtered nothing and said nothing")
	}
}

// clientPrompts writes Claude Code's 30 recalls (20 got memory) and
// Codex's 10 (2 got memory), each in its own session.
func clientPrompts(x *fx) {
	x.prompts("billing", "cc", x.now, 30, func(i int, p *telemetry.Prompt) {
		if i < 20 {
			p.Injected = 2
		}
	})
	x.client = harnessid.Codex
	x.prompts("billing", "cx", x.now.Add(-40*time.Minute), 10, func(i int, p *telemetry.Prompt) {
		if i < 2 {
			p.Injected = 2
		}
	})
	x.client = ""
}

func hit(t *testing.T, m map[string]Metric) (int64, int64) {
	t.Helper()
	h := m["recall.hit_rate"]
	if h.Num == nil || h.Den == nil {
		t.Fatalf("recall.hit_rate %+v", h)
	}
	return *h.Num, *h.Den
}

// A client filter counts that tool's activity alone, computed as the
// combined figure is; no filter counts both. The server's figures cannot
// be split: they say so (not_filtered).
func TestClientFilterCountsOneToolsActivity(t *testing.T) {
	x := newFx(t, fxNow)
	clientPrompts(x)
	for _, c := range []struct {
		client   string
		num, den int64
	}{{"", 22, 40}, {harnessid.ClaudeCode, 20, 30}, {harnessid.Codex, 2, 10}} {
		q := x.query(7)
		q.Client = c.client
		if num, den := hit(t, x.metrics([]string{"recall.hit_rate"}, q)); num != c.num || den != c.den {
			t.Errorf("client %q: recall %d/%d, want %d/%d", c.client, num, den, c.num, c.den)
		}
	}
	q := x.query(7)
	q.Client = harnessid.Codex
	m := x.metrics([]string{"extraction.tokens"}, q)
	if !slices.Contains(m["extraction.tokens"].Tags, tagNotFiltered) {
		t.Errorf("the server's tokens under a client filter: tags %v", m["extraction.tokens"].Tags)
	}
	if m := x.metrics([]string{"extraction.tokens"}, x.query(7)); slices.Contains(m["extraction.tokens"].Tags, tagNotFiltered) {
		t.Error("not_filtered without a filter")
	}
}

// The overview of a window two tools were active in splits it by tool:
// sessions, prompts, recalls and the recall figure of each; the scope
// names both. Filtered, or with one tool only, there is no split, and a
// PC with one tool names none in its scope.
func TestOverviewSplitsByClient(t *testing.T) {
	x := newFx(t, fxNow)
	clientPrompts(x)
	r := x.run("overview", x.query(7))
	if !slices.Equal(r.Scope.Clients, []string{harnessid.ClaudeCode, harnessid.Codex}) || r.Scope.Client != "" {
		t.Errorf("scope %+v", r.Scope)
	}
	if len(r.Tables.ByClient) != 2 {
		t.Fatalf("split %+v", r.Tables.ByClient)
	}
	for i, want := range []struct {
		client             string
		sessions, recalls  int64
		withHits, num, den int64
	}{{harnessid.ClaudeCode, 30, 30, 20, 20, 30}, {harnessid.Codex, 10, 10, 2, 2, 10}} {
		row := r.Tables.ByClient[i]
		num, den := hit(t, row.Metrics)
		if row.Client != want.client || row.Sessions != want.sessions || row.Recalls != want.recalls || row.WithHits != want.withHits || num != want.num || den != want.den {
			t.Errorf("row %d: %+v (recall %d/%d), want %+v", i, row, num, den, want)
		}
		if _, ok := row.Metrics["context.per_session"]; !ok {
			t.Errorf("row %d has no context figure", i)
		}
	}
	// The combined tiles are untouched by the split's own runs.
	if num, den := hit(t, r.Metrics); num != 22 || den != 40 {
		t.Errorf("combined recall %d/%d after the split", num, den)
	}
	q := x.query(7)
	q.Client = harnessid.Codex
	f := x.run("overview", q)
	if f.Tables.ByClient != nil || f.Scope.Client != harnessid.Codex || !slices.Contains(f.Scope.Clients, harnessid.Codex) {
		t.Errorf("filtered: split %v scope %+v", f.Tables.ByClient, f.Scope)
	}
	if num, den := hit(t, f.Metrics); num != 2 || den != 10 {
		t.Errorf("filtered recall %d/%d", num, den)
	}
	one := newFx(t, fxNow)
	one.prompts("billing", "cc", one.now, 5, nil)
	if r := one.run("overview", one.query(7)); r.Tables.ByClient != nil || r.Scope.Clients != nil {
		t.Errorf("one tool: split %v clients %v", r.Tables.ByClient, r.Scope.Clients)
	}
}

// A client filter counts this PC's activity: other PCs send no client, so
// all PCs is not offered under it.
func TestClientFilterIsThisPCs(t *testing.T) {
	x := newFx(t, fxNow)
	clientPrompts(x)
	q := x.query(7)
	q.Machines, q.Client = "all", harnessid.Codex
	if r := x.run("quality", q); r.Scope.Machines != "this" {
		t.Errorf("machines %q under a client filter", r.Scope.Machines)
	}
}
