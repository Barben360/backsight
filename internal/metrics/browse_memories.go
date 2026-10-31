package metrics

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
)

// The memory states of the memories view.
const (
	stateHidden    = "hidden"
	stateWrong     = "wrong"
	stateNeverUsed = "never_used"
	memoryIDColumn = "memory_id"
)

// memoryTables are the index tables that name memories.
var memoryTables = []string{"ledger", "shows", "credits", "retirements"}

// errTextNeedsID: `memories --text` names a memory.
var errTextNeedsID = errors.New("--text needs a memory id")

// Memories is the memories view: the feedback ledger's memories, each with
// its counts (recent-weighted: credits and misses decayed to now), rate,
// boost, project (the project of its latest show) and state. Order:
// --sort shown (the default), used, rate or last_shown, largest or latest
// first (a memory with no rate or no show last), then by id. --state keeps
// hidden, wrong or never_used ones (as `memory stale` sorts them); the
// project filter keeps the memories of that project. With q.ID (an exact id
// or a unique prefix of at least 8 characters) it is that memory's page; with
// q.Text it also carries the memory's text, read from the server for this
// call only.
func Memories(ctx context.Context, s Source, q Query) (Result, error) {
	id := strings.TrimSpace(q.ID)
	if q.Text && id == "" {
		return Result{}, errTextNeedsID
	}
	if id != "" {
		return memoryPage(ctx, s, q, id)
	}
	key, err := checkSort(q, "shown", "shown", "used", "rate", "last_shown")
	if err != nil {
		return Result{}, err
	}
	if q.State != "" && !slices.Contains([]string{stateHidden, stateWrong, stateNeverUsed}, q.State) {
		return Result{}, errors.New("--state: one of hidden, wrong, never_used")
	}
	return runView(ctx, s, q, "memories", nil, func(e env, r *Result) error {
		rows, err := memoryRows(e)
		if err != nil {
			return err
		}
		if e.q.Project != "" { // the filter needs every memory's project; a plain page, only its own
			if err := fillProjects(e, rows); err != nil {
				return err
			}
		}
		rows = slices.DeleteFunc(rows, func(m MemoryRow) bool {
			return (e.q.State != "" && m.State != e.q.State) || (e.q.Project != "" && m.Project != e.q.Project)
		})
		sortMemories(rows, key)
		lo, hi, page := pageBounds(e.q, len(rows))
		shown := rows[lo:hi]
		if e.q.Project == "" {
			if err := fillProjects(e, shown); err != nil {
				return err
			}
		}
		r.Tables.Memories, r.Tables.Page = shown, &page
		r.Metrics, err = computeMetrics(e, e.q.Window, staleIDs)
		return err
	})
}

// sortMemories orders rows by key (see Memories), ties by id.
func sortMemories(rows []MemoryRow, key string) {
	slices.SortFunc(rows, func(a, b MemoryRow) int {
		switch key {
		case "shown":
			if a.Shown != b.Shown {
				return int(b.Shown - a.Shown)
			}
		case "used":
			if a.Used != b.Used {
				return cmpFloatDesc(a.Used, b.Used)
			}
		case "rate":
			switch {
			case a.Rate == nil && b.Rate != nil:
				return 1
			case a.Rate != nil && b.Rate == nil:
				return -1
			case a.Rate != nil && *a.Rate != *b.Rate:
				return cmpFloatDesc(*a.Rate, *b.Rate)
			}
		case "last_shown":
			if c := b.LastShown.Compare(a.LastShown); c != 0 {
				return c
			}
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// cmpFloatDesc orders larger first.
func cmpFloatDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

// ledgerParams is the [feedback] table as the ledger's parameters, as the
// recall and `memory` commands read it.
func ledgerParams(cfg config.Config) feedback.Params {
	const day = 24 * time.Hour
	return feedback.Params{
		ConfidentRank:  cfg.Feedback.ConfidentRank,
		MissHalfLife:   time.Duration(cfg.Feedback.MissHalfLifeDays) * day,
		CreditHalfLife: time.Duration(cfg.Feedback.CreditHalfLifeDays) * day,
		TurnTimeout:    feedback.TurnTimeout,
		Weight:         cfg.Feedback.BoostWeight,
		PriorCredits:   cfg.Feedback.PriorCredits,
		PriorMisses:    cfg.Feedback.PriorMisses,
	}
}

// memoryProjects is the project of every memory that has one: that of its
// latest show carrying one (memoryProject, for all of them at once), read
// once per transaction. One pass over the shows, keeping each memory's
// latest by time and journal position, is cheaper than ranking them in SQL.
func (e env) memoryProjects() (map[string]string, error) {
	if e.memo != nil && e.memo.projects != nil {
		return e.memo.projects, nil
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT memory_id, project, ts, source_file, "offset", pos FROM shows WHERE memory_id IS NOT NULL AND project IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type at struct {
		ts, offset, pos int64
		file, project   string
	}
	latest := map[string]at{}
	for rows.Next() {
		var id string
		var a at
		if err := rows.Scan(&id, &a.project, &a.ts, &a.file, &a.offset, &a.pos); err != nil {
			return nil, err
		}
		if old, ok := latest[id]; !ok || a.ts > old.ts || (a.ts == old.ts && (a.file > old.file ||
			(a.file == old.file && (a.offset > old.offset || (a.offset == old.offset && a.pos > old.pos))))) {
			latest[id] = a
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(latest))
	for id, a := range latest {
		out[id] = a.project
	}
	if e.memo != nil {
		e.memo.projects = out
	}
	return out, nil
}

// fillProjects sets each row's project: for many rows from every memory's
// at once (memoryProjects), for a few from each one's own latest show.
func fillProjects(e env, rows []MemoryRow) error {
	if len(rows) > projectsBulk || (e.memo != nil && e.memo.projects != nil) {
		projects, err := e.memoryProjects()
		if err != nil {
			return err
		}
		for i := range rows {
			rows[i].Project = projects[rows[i].ID]
		}
		return nil
	}
	for i := range rows {
		rows[i].Project = memoryProject(e.tx, rows[i].ID)
	}
	return nil
}

// projectsBulk is how many rows are read for their project from one pass
// over the shows rather than one lookup each.
const projectsBulk = 200

// memoryRows are the ledger's memories as rows without their project (an
// entry that only holds a restore is not one), in no order.
func memoryRows(e env) ([]MemoryRow, error) {
	l, err := e.ledger()
	if err != nil {
		return nil, err
	}
	lists, err := e.staleLists()
	if err != nil {
		return nil, err
	}
	state := map[string]string{}
	for _, c := range []struct {
		ids   []string
		state string
	}{{lists.NeverUsed, stateNeverUsed}, {lists.Reported, stateWrong}, {lists.Hidden, stateHidden}} {
		for _, id := range c.ids {
			state[id] = c.state
		}
	}
	p := ledgerParams(e.src.Config)
	out := make([]MemoryRow, 0, len(l.Memories))
	for id, m := range l.Memories {
		if m.RestoreOnly() {
			continue
		}
		out = append(out, memoryRow(e, id, m, "", state[id], p))
	}
	return out, nil
}

// memoryRow is the row of memory id's ledger entry m.
func memoryRow(e env, id string, m feedback.Entry, project, state string, p feedback.Params) MemoryRow {
	d := m.DecayedTo(e.now, p)
	r := MemoryRow{ID: id, Project: project, Shown: int64(m.Shown), Used: d.Credits, Ignored: d.Misses,
		Boost: m.Boost(p, e.now), State: state}
	if n := d.Credits + d.Misses; n > 0 {
		r.Rate = ptr(d.Credits / n)
	}
	if !m.LastShown.IsZero() {
		r.LastShown = m.LastShown.In(viewLoc(e))
	}
	if !m.LastCredited.IsZero() {
		r.LastUsed = m.LastCredited.In(viewLoc(e))
	}
	return r
}

// memoryPage is `memories <id>`. The id is resolved first (its own read
// transaction: the server read of the memory needs the full id before the
// view's transaction opens), then the view runs with that one server read.
func memoryPage(ctx context.Context, s Source, q Query, id string) (Result, error) {
	if s.Index == nil {
		return Result{}, errNoIndex
	}
	var full string
	if err := s.Index.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		full, err = resolveID(tx, "memory", id, memoryIDColumn, memoryTables)
		return err
	}); err != nil {
		return Result{}, err
	}
	key := memoryKeyPrefix + full
	return runView(ctx, s, q, "memories", []string{key}, func(e env, r *Result) error {
		page := MemoryPage{Shows: []ShowMark{}, Retirements: []RetireMark{}, RankHistogram: map[string]int64{},
			Commands: []string{"backsight read " + full, "backsight forget " + full, "backsight forget --restore " + full}}
		l, err := e.ledger()
		if err != nil {
			return err
		}
		lists, err := e.staleLists()
		if err != nil {
			return err
		}
		projects, err := e.memoryProjects()
		if err != nil {
			return err
		}
		state := ""
		for _, c := range []struct {
			ids   []string
			state string
		}{{lists.NeverUsed, stateNeverUsed}, {lists.Reported, stateWrong}, {lists.Hidden, stateHidden}} {
			if slices.Contains(c.ids, full) {
				state = c.state
			}
		}
		page.Row = memoryRow(e, full, l.Memories[full], projects[full], state, ledgerParams(e.src.Config))
		if serverReason(e, key) == "" {
			m := e.srv.Memory
			page.Kind, page.ServerState = m.Type, m.State
			if t := hindsight.ServerTime(m.MentionedAt); !t.IsZero() {
				page.Created = t.In(viewLoc(e))
			}
			if e.q.Text {
				r.Text = &MemoryText{ID: full, Text: m.Text}
			}
		}
		var total int
		if page.Shows, total, err = memoryShows(e, full); err != nil {
			return err
		}
		if page.RankHistogram, err = rankHistogram(e, full); err != nil {
			return err
		}
		if page.Retirements, err = memoryRetirements(e, full); err != nil {
			return err
		}
		pg := Page{Limit: e.q.EffectiveLimit(), Offset: max(e.q.Offset, 0), Total: total}
		r.Tables.Memory, r.Tables.Page = &page, &pg
		return nil
	})
}

// memoryShows are the shows of memory id (the sources answers are not
// shows), latest first, then by the journal position, paged; with the
// settle outcome of each (pending until settled) and the total.
func memoryShows(e env, id string) ([]ShowMark, int, error) {
	var total int
	if err := e.tx.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM shows WHERE memory_id = ? AND COALESCE(path, '') <> ?`,
		id, feedback.PathSources).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT s.ts, COALESCE(s.path, ''), COALESCE(s.turn, ''), s.rank, s.rank_without_boost, s.score_without_boost,
       (SELECT st.outcome FROM settles st WHERE st.memory_id = s.memory_id AND st.key = s.key ORDER BY st.settle_ts LIMIT 1)
FROM shows s WHERE s.memory_id = ? AND COALESCE(s.path, '') <> ?
ORDER BY s.ts DESC, s.source_file DESC, s."offset" DESC, s.pos DESC LIMIT ? OFFSET ?`,
		id, feedback.PathSources, e.q.EffectiveLimit(), max(e.q.Offset, 0))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []ShowMark{}
	for rows.Next() {
		var (
			m                 ShowMark
			ts                int64
			rank, rankNoBoost sql.NullInt64
			score             sql.NullFloat64
			outcome           sql.NullString
		)
		if err := rows.Scan(&ts, &m.Path, &m.Turn, &rank, &rankNoBoost, &score, &outcome); err != nil {
			return nil, 0, err
		}
		m.At = time.UnixMilli(ts).In(viewLoc(e))
		m.Outcome = outcome.String
		if !outcome.Valid {
			m.Outcome = pendingOutcome
		}
		if rank.Valid {
			m.Rank = ptr(rank.Int64)
		}
		if rankNoBoost.Valid {
			m.RankWithoutBoost = ptr(rankNoBoost.Int64)
		}
		if score.Valid {
			m.Score = ptr(score.Float64)
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// pendingOutcome is the outcome of a show no settle has decided yet.
const pendingOutcome = "pending"

// rankHistogram counts the shows of memory id (all of them, not the page)
// by rank, in used.rank's buckets.
func rankHistogram(e env, id string) (map[string]int64, error) {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT rank, COUNT(*) FROM shows WHERE memory_id = ? AND COALESCE(path, '') <> ? GROUP BY rank`,
		id, feedback.PathSources)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	hist := make(map[string]int64, len(rankKeys))
	for _, k := range rankKeys {
		hist[k] = 0
	}
	for rows.Next() {
		var rank sql.NullInt64
		var n int64
		if err := rows.Scan(&rank, &n); err != nil {
			return nil, err
		}
		hist[rankKey(rank)] += n
	}
	return hist, rows.Err()
}

// memoryRetirements are the forgets, retirements, hides and restores of
// memory id, oldest first, then by the journal position.
func memoryRetirements(e env, id string) ([]RetireMark, error) {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT ts, COALESCE(kind, '') FROM retirements WHERE memory_id = ?
ORDER BY ts, source_file, "offset", pos`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RetireMark{}
	for rows.Next() {
		var m RetireMark
		var ts int64
		if err := rows.Scan(&ts, &m.Kind); err != nil {
			return nil, err
		}
		m.At = time.UnixMilli(ts).In(viewLoc(e))
		out = append(out, m)
	}
	return out, rows.Err()
}
