package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// The --has filters of the sessions view.
const (
	hasUsed       = "used"
	hasWrong      = "wrong"
	hasUndeclared = "undeclared"
	hasMemory     = "memory"
)

// sessionTables are the index tables that name sessions.
var sessionTables = []string{"turns", "shows", "credits", "tel_prompt", "tel_briefing"}

// Sessions is the sessions view: the sessions active in the window (the
// session's project is the filter), latest activity first, then by id,
// paged. --has keeps those with a used credit (used), a wrong or outdated
// credit (wrong), a turn that ended undeclared under the two-hour rule
// (undeclared) or a non-briefing show (memory). `sent` is the state of the
// session's documents (ok only when all are ok). With q.ID (an exact id or
// a unique prefix of at least 8 characters) it is that session's page, the
// turn rail of all of it.
func Sessions(ctx context.Context, s Source, q Query) (Result, error) {
	if id := strings.TrimSpace(q.ID); id != "" {
		return sessionPage(ctx, s, q, id)
	}
	if _, err := checkSort(q, "latest", "latest"); err != nil {
		return Result{}, err
	}
	if q.Has != "" && !slices.Contains([]string{hasUsed, hasWrong, hasUndeclared, hasMemory}, q.Has) {
		return Result{}, fmt.Errorf("--has: one of %s, %s, %s, %s", hasUsed, hasWrong, hasUndeclared, hasMemory)
	}
	return runView(ctx, s, q, "sessions", nil, func(e env, r *Result) error {
		rows, err := sessionRows(e, e.q.Window)
		if err != nil {
			return err
		}
		if rows, err = hasFilter(e, rows); err != nil {
			return err
		}
		sortSessions(rows)
		lo, hi, page := pageBounds(e.q, len(rows))
		r.Tables.Sessions, r.Tables.Page = orEmpty(rows[lo:hi]), &page
		return withSent(e, r.Tables.Sessions)
	})
}

// hasFilter keeps the rows with what q.Has names.
func hasFilter(e env, rows []SessionRow) ([]SessionRow, error) {
	var keep func(SessionRow) bool
	switch e.q.Has {
	case "":
		return rows, nil
	case hasUsed:
		keep = func(s SessionRow) bool { return s.Used > 0 }
	case hasWrong:
		keep = func(s SessionRow) bool { return s.Wrong > 0 }
	case hasUndeclared:
		turns, err := mainTurns(e, e.q.Window)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, t := range turns {
			if t.outcome == "undeclared" {
				set[t.session] = true
			}
		}
		keep = func(s SessionRow) bool { return set[s.ID] }
	case hasMemory:
		set, err := memorySessions(e, e.q.Window)
		if err != nil {
			return nil, err
		}
		keep = func(s SessionRow) bool { return set[s.ID] }
	}
	out := rows[:0:0]
	for _, s := range rows {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// memorySessions are the sessions with a show other than a briefing's (or a
// sources answer) in w.
func memorySessions(e env, w Window) (map[string]bool, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT DISTINCT s.session FROM shows s
WHERE s.ts >= ? AND s.ts < ? AND COALESCE(s.session, '') <> '' AND COALESCE(s.path, '') NOT IN ('briefing', ?) AND `+sessionProject("s"),
		w.Since.UnixMilli(), w.Until.UnixMilli(), feedback.PathSources, e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// sessionPage is `sessions <id>`: the session's totals over all it ever did
// and one row per main-agent turn.
func sessionPage(ctx context.Context, s Source, q Query, id string) (Result, error) {
	q.Project = "" // an id names its session whatever the project filter
	return runView(ctx, s, q, "sessions", nil, func(e env, r *Result) error {
		full, err := resolveID(e.tx, "session", id, "session", sessionTables)
		if err != nil {
			return err
		}
		rows, err := sessionRowsOf(e, allTime(viewLoc(e)), full)
		if err != nil {
			return err
		}
		page := SessionPage{Totals: SessionRow{ID: full}, Turns: []TurnRow{}}
		if len(rows) > 0 {
			page.Totals = rows[0]
		}
		sent, err := sentBySession(e)
		if err != nil {
			return err
		}
		page.Totals.Sent = sent[full]
		if page.Turns, err = turnRail(e, full); err != nil {
			return err
		}
		r.Tables.Session = &page
		return nil
	})
}

// railPrompt is a typed or machine prompt of the rail.
type railPrompt struct {
	at                    time.Time
	n                     sql.NullInt64
	decision, reason, emp string
}

// railTurn is a main-agent turn of the journal.
type railTurn struct {
	id string
	n  sql.NullInt64
	at time.Time
}

// turnRail is the session's main-agent turns in order of time: one row per
// prompt of the telemetry (matched to the journal's turn of the same n),
// then the journal turns no prompt names. Each row has its shows with their
// settle outcome, the memory tool calls of its turn, the verdicts of its
// credits and its final outcome.
func turnRail(e env, session string) ([]TurnRow, error) {
	prompts, err := railPrompts(e, session)
	if err != nil {
		return nil, err
	}
	turns, err := railTurns(e, session)
	if err != nil {
		return nil, err
	}
	shows, lastShow, err := railShows(e, session)
	if err != nil {
		return nil, err
	}
	searches, err := railSearches(e, session)
	if err != nil {
		return nil, err
	}
	declared, err := railCredits(e, session)
	if err != nil {
		return nil, err
	}
	stops, err := railStops(e, session)
	if err != nil {
		return nil, err
	}
	loc := viewLoc(e)
	byN := map[int64]*railTurn{}
	for i := range turns {
		if t := &turns[i]; t.n.Valid && byN[t.n.Int64] == nil {
			byN[t.n.Int64] = t
		}
	}
	taken := map[string]bool{}
	out := []TurnRow{}
	add := func(row TurnRow, turn *railTurn) {
		row.Shows, row.Declared = []ShowChip{}, []string{}
		if turn != nil {
			row.Turn = turn.id
			row.Shows = orEmpty(shows[turn.id])
			row.Declared = orEmpty(declared[turn.id])
			row.Check = railCheck(e.now, stops, lastShow, turn.id)
		}
		if row.N != nil {
			row.Searches = searches[*row.N]
		}
		out = append(out, row)
	}
	for _, p := range prompts {
		row := TurnRow{At: p.at.In(loc), Decision: p.decision, Reason: p.reason, Empty: p.emp}
		var turn *railTurn
		if p.n.Valid {
			row.N = ptr(p.n.Int64)
			if t := byN[p.n.Int64]; t != nil && !taken[t.id] {
				turn = t
				taken[t.id] = true
			}
		}
		add(row, turn)
	}
	for i := range turns {
		if t := &turns[i]; !taken[t.id] {
			row := TurnRow{At: t.at.In(loc)}
			if t.n.Valid {
				row.N = ptr(t.n.Int64)
			}
			add(row, t)
		}
	}
	slices.SortStableFunc(out, func(a, b TurnRow) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		switch {
		case a.N == nil && b.N != nil:
			return 1
		case a.N != nil && b.N == nil:
			return -1
		case a.N != nil && *a.N != *b.N:
			return int(*a.N - *b.N)
		}
		return strings.Compare(a.Turn, b.Turn)
	})
	return out, nil
}

// railCheck is a turn's final outcome: its stop's, else, for a turn that
// showed memory (other than in a briefing), pending or undeclared by the
// age of its latest show; a turn that showed none has nothing to check.
func railCheck(now time.Time, stops map[string]string, lastShow map[string]time.Time, turn string) string {
	if o, ok := stops[turn]; ok {
		return o
	}
	if last, ok := lastShow[turn]; ok {
		return turnOutcome(now, sql.NullString{}, last)
	}
	return ""
}

func railPrompts(e env, session string) ([]railPrompt, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT ts, n, COALESCE(decision, ''), COALESCE(reason, ''), COALESCE(empty, '') FROM tel_prompt
WHERE session = ? ORDER BY ts, source_file, "offset"`, session)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []railPrompt
	for rows.Next() {
		var p railPrompt
		var ts int64
		if err := rows.Scan(&ts, &p.n, &p.decision, &p.reason, &p.emp); err != nil {
			return nil, err
		}
		p.at = time.UnixMilli(ts)
		out = append(out, p)
	}
	return out, rows.Err()
}

func railTurns(e env, session string) ([]railTurn, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT turn, MIN(n), MIN(ts) FROM turns
WHERE session = ? AND COALESCE(agent, '') = '' AND turn IS NOT NULL GROUP BY turn ORDER BY MIN(ts), turn`, session)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []railTurn
	for rows.Next() {
		var t railTurn
		var ts int64
		if err := rows.Scan(&t.id, &t.n, &ts); err != nil {
			return nil, err
		}
		t.at = time.UnixMilli(ts)
		out = append(out, t)
	}
	return out, rows.Err()
}

// railShows are the main agent's shows of the session by turn, in journal
// order (the sources answers are not shows), each with its settle outcome
// (pending until settled), and the time of each turn's latest show other
// than a briefing's.
func railShows(e env, session string) (map[string][]ShowChip, map[string]time.Time, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT COALESCE(s.turn, ''), COALESCE(s.memory_id, ''), COALESCE(s.path, ''), s.rank,
       (SELECT st.outcome FROM settles st WHERE st.memory_id = s.memory_id AND st.key = s.key ORDER BY st.settle_ts LIMIT 1),
       s.ts
FROM shows s WHERE s.session = ? AND COALESCE(s.agent, '') = '' AND COALESCE(s.path, '') <> ?
ORDER BY s.ts, s.source_file, s."offset", s.pos`, session, feedback.PathSources)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	chips, last := map[string][]ShowChip{}, map[string]time.Time{}
	for rows.Next() {
		var turn string
		var c ShowChip
		var rank sql.NullInt64
		var outcome sql.NullString
		var ts int64
		if err := rows.Scan(&turn, &c.MemoryID, &c.Path, &rank, &outcome, &ts); err != nil {
			return nil, nil, err
		}
		c.Outcome = outcome.String
		if !outcome.Valid {
			c.Outcome = pendingOutcome
		}
		if rank.Valid {
			c.Rank = ptr(rank.Int64)
		}
		chips[turn] = append(chips[turn], c)
		if c.Path != "briefing" {
			last[turn] = time.UnixMilli(ts)
		}
	}
	return chips, last, rows.Err()
}

// railSearches are the memory tool calls (search, read, store) of the main
// agent by prompt number.
func railSearches(e env, session string) (map[int64]int64, error) {
	var cond []string
	args := []any{session}
	for _, n := range memoryTools {
		cond = append(cond, fmt.Sprintf(`substr(name, -%d) = ?`, len(n)))
		args = append(args, n)
	}
	rows, err := e.tx.QueryContext(e.ctx, `SELECT n, COUNT(*) FROM tel_tool_use
WHERE session = ? AND n IS NOT NULL AND COALESCE(subagent, 0) = 0 AND (`+strings.Join(cond, " OR ")+`) GROUP BY n`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int64{}
	for rows.Next() {
		var n, c int64
		if err := rows.Scan(&n, &c); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, rows.Err()
}

// railCredits are the main agent's credit verdicts by turn, each verdict
// once, in the order first given.
func railCredits(e env, session string) (map[string][]string, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT COALESCE(turn, ''), verdict FROM credits
WHERE session = ? AND COALESCE(agent, '') = '' AND verdict IS NOT NULL
ORDER BY ts, source_file, "offset", pos`, session)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var turn, verdict string
		if err := rows.Scan(&turn, &verdict); err != nil {
			return nil, err
		}
		if !slices.Contains(out[turn], verdict) {
			out[turn] = append(out[turn], verdict)
		}
	}
	return out, rows.Err()
}

// railStops are the main agent's final stop outcome by turn.
func railStops(e env, session string) (map[string]string, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT COALESCE(turn, ''), outcome FROM turn_final_stop
WHERE session = ? AND COALESCE(agent, '') = '' AND outcome IS NOT NULL`, session)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var turn, outcome string
		if err := rows.Scan(&turn, &outcome); err != nil {
			return nil, err
		}
		out[turn] = outcome
	}
	return out, rows.Err()
}
