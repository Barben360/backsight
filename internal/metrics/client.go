package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/Barben360/backsight/internal/harnessid"
)

// The client filter and the split by client (block 8,
// https://github.com/Barben360/backsight/issues/283): which coding tool's
// activity the figures count. Every table fed by a telemetry or journal
// line has a client column (migration 0006); the filter shadows each of
// them, in the view's read transaction, with a temporary view of the same
// name holding that tool's rows alone. Every family then reads one tool's
// activity through its own queries, unchanged, so a filtered figure is
// computed exactly as the combined one. The transaction is read-only and
// always rolled back, which drops the temporary views with it (SQLite's
// temp schema is transactional). The server's figures, and the tables no
// line feeds (memory documents, the ledger), have no client: they stay
// whole, and a server-scope metric wears not_filtered.

// clientTables are the index tables with a client column (migration 0006);
// a test holds the list equal to the schema's.
var clientTables = []string{
	"tel_prompt", "tel_outcome", "tel_reply", "tel_briefing", "tel_briefing_outcome", "tel_tool", "tel_tool_use",
	"tel_credit", "tel_credit_check", "tel_stop_block", "tel_retire", "tel_feedback_settle", "tel_feedback_error",
	"tel_gate", "tel_retain", "tel_hook_failure", "tel_job", "tel_attribution", "tel_store", "tel_dashboard", "tel_config",
	"events", "turns", "show_sets", "shows", "settles", "credits", "stops", "retirements", "recalls", "imports", "journal_other",
}

// ClientIDs are the coding tools a client filter names, in display order.
var ClientIDs = []string{harnessid.ClaudeCode, harnessid.Codex}

// filterClient shadows every client table, in tx, with client's rows of
// it. client must be one of ClientIDs (it is written into the views'
// SQL, which takes no parameter).
func filterClient(ctx context.Context, tx *sql.Tx, client string) error {
	if !slices.Contains(ClientIDs, client) {
		return fmt.Errorf("metrics: unknown client %q", client)
	}
	for _, t := range clientTables {
		if _, err := tx.ExecContext(ctx, `CREATE TEMP VIEW `+t+` AS SELECT * FROM main.`+t+` WHERE client = '`+client+`'`); err != nil {
			return fmt.Errorf("metrics: filter %s by client: %w", t, err)
		}
	}
	return nil
}

// unfilterClient drops filterClient's views.
func unfilterClient(ctx context.Context, tx *sql.Tx) error {
	for _, t := range clientTables {
		if _, err := tx.ExecContext(ctx, `DROP VIEW IF EXISTS temp.`+t); err != nil {
			return err
		}
	}
	return nil
}

// clientsIn are the coding tools with a typed prompt or a memory tool call
// in w, read from the whole tables (never through a filter), in
// ClientIDs' order.
func clientsIn(e env, w Window) ([]string, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT DISTINCT client FROM main.tel_prompt WHERE ts >= ? AND ts < ?
UNION SELECT DISTINCT client FROM main.tel_tool WHERE ts >= ? AND ts < ?`,
		w.Since.UnixMilli(), w.Until.UnixMilli(), w.Since.UnixMilli(), w.Until.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		seen[harnessid.Or(c)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range ClientIDs {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// splitIDs are the metrics the split by client gives per tool: recall
// (memory got), credits (memory used) and the cost in context.
var splitIDs = []string{"recall.hit_rate", "used.turn_rate", "context.per_session"}

// ClientRow is one coding tool's share of the window, on the overview of
// a window where more than one tool was active: its sessions and prompts,
// and the recall, credit and context figures computed for its activity
// alone, as the tiles are for all of it.
type ClientRow struct {
	Client   string            `json:"client"`
	Sessions int64             `json:"sessions"`
	Prompts  int64             `json:"prompts"`
	Recalls  int64             `json:"recalls"`
	WithHits int64             `json:"with_hits"`
	Credits  map[string]int64  `json:"credits"`
	Metrics  map[string]Metric `json:"metrics"`
}

// byClient is the split of w by coding tool, nil when fewer than two
// were active in it. Each tool's figures are computed under its filter,
// in this transaction, with a memo of their own (the env's holds the
// combined figures).
func byClient(e env, w Window) ([]ClientRow, error) {
	clients, err := clientsIn(e, w)
	if err != nil || len(clients) < 2 {
		return nil, err
	}
	var out []ClientRow
	for _, c := range clients {
		if err := filterClient(e.ctx, e.tx, c); err != nil {
			return nil, err
		}
		ce := e
		ce.q.Client, ce.memo = c, newMemo()
		row, err := clientRow(ce, w)
		if uerr := unfilterClient(e.ctx, e.tx); err == nil {
			err = uerr
		}
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// clientRow is e's tool's row over w (e reads that tool's rows alone).
func clientRow(e env, w Window) (ClientRow, error) {
	row := ClientRow{Client: e.q.Client}
	m, err := computeMetrics(e, w, splitIDs)
	if err != nil {
		return row, err
	}
	for id, met := range m {
		met.Series = nil
		m[id] = met
	}
	row.Metrics = m
	a, err := activity(e, w)
	if err != nil {
		return row, err
	}
	row.Prompts, row.Recalls, row.WithHits, row.Credits = a.Prompts, a.Recalls, a.WithHits, a.Credits
	err = e.tx.QueryRowContext(e.ctx, `SELECT COUNT(DISTINCT session) FROM tel_prompt WHERE ts >= ? AND ts < ? AND (reason IS NULL OR reason <> 'machine') AND `+projectClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project).Scan(&row.Sessions)
	return row, err
}

// scopeClients are the tools the scope names: those active in w when more
// than one was, or a filter names one; nil otherwise (a PC with one tool
// reads as before).
func scopeClients(e env, w Window) ([]string, error) {
	cs, err := clientsIn(e, w)
	if err != nil {
		return nil, err
	}
	if len(cs) < 2 && e.q.Client == "" {
		return nil, nil
	}
	if e.q.Client != "" && !slices.Contains(cs, e.q.Client) {
		cs = append(cs, e.q.Client)
	}
	return cs, nil
}
