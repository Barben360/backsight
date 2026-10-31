package metrics

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Projects is the projects view: one row per project the window's sessions
// belong to (the session's project, as the activity metrics read it), with
// the pooled totals. With q.ID it is one project's page.
//
// Order: --sort last_activity (the default), sessions or prompts, largest
// or latest first, then by project name; or project, by name. Rows are
// paged by limit and offset; the pooled row covers every project, not the
// page.
func Projects(ctx context.Context, s Source, q Query) (Result, error) {
	if id := strings.TrimSpace(q.ID); id != "" {
		return projectPage(ctx, s, q, id)
	}
	key, err := checkSort(q, "last_activity", "last_activity", "sessions", "prompts", "project")
	if err != nil {
		return Result{}, err
	}
	return runView(ctx, s, q, "projects", []string{"tags"}, func(e env, r *Result) error {
		rows, err := projectRows(e, e.q.Window)
		if err != nil {
			return err
		}
		pooled := pooledRow(rows)
		sortProjects(rows, key)
		lo, hi, page := pageBounds(e.q, len(rows))
		r.Tables.Projects, r.Tables.Pooled, r.Tables.Page = rows[lo:hi], &pooled, &page
		return nil
	})
}

// sortProjects orders rows by key (see Projects), ties by project name.
func sortProjects(rows []ProjectRow, key string) {
	slices.SortFunc(rows, func(a, b ProjectRow) int {
		switch key {
		case "sessions":
			if a.Sessions != b.Sessions {
				return int(b.Sessions - a.Sessions)
			}
		case "prompts":
			if a.Prompts != b.Prompts {
				return int(b.Prompts - a.Prompts)
			}
		case "last_activity":
			if c := cmpTimeDesc(a.LastActivity, b.LastActivity); c != 0 {
				return c
			}
		}
		return strings.Compare(a.Project, b.Project)
	})
}

// pooledColumns are the numeric columns of a project row that the pooled
// row gives the extremes of, by their JSON names.
var pooledColumns = []struct {
	name string
	of   func(ProjectRow) int64
}{
	{"sessions", func(r ProjectRow) int64 { return r.Sessions }},
	{"prompts", func(r ProjectRow) int64 { return r.Prompts }},
	{"prompts_with_memory", func(r ProjectRow) int64 { return r.PromptsWithMemory }},
	{"used_turns", func(r ProjectRow) int64 { return r.UsedTurns }},
	{"reported_wrong", func(r ProjectRow) int64 { return r.ReportedWrong }},
}

// pooledLabel names the pooled row.
const pooledLabel = "all projects pooled"

// pooledRow totals rows (the projects with at least one session) and gives
// each numeric column's smallest and largest value among them.
func pooledRow(rows []ProjectRow) PooledRow {
	p := PooledRow{Label: pooledLabel, Min: map[string]float64{}, Max: map[string]float64{}}
	for _, r := range rows {
		p.Totals.Sessions += r.Sessions
		p.Totals.Prompts += r.Prompts
		p.Totals.PromptsWithMemory += r.PromptsWithMemory
		p.Totals.UsedTurns += r.UsedTurns
		p.Totals.ReportedWrong += r.ReportedWrong
		if r.LastActivity != nil && (p.Totals.LastActivity == nil || r.LastActivity.After(*p.Totals.LastActivity)) {
			p.Totals.LastActivity = r.LastActivity
		}
		for i, pt := range r.Series {
			if i == len(p.Totals.Series) {
				p.Totals.Series = append(p.Totals.Series, Point{Day: pt.Day, Hour: pt.Hour, Value: ptr(0.0), Partial: pt.Partial})
			}
			*p.Totals.Series[i].Value += *pt.Value
		}
	}
	if p.Totals.Series == nil {
		p.Totals.Series = []Point{}
	}
	for _, c := range pooledColumns {
		for i, r := range rows {
			v := float64(c.of(r))
			if i == 0 || v < p.Min[c.name] {
				p.Min[c.name] = v
			}
			if i == 0 || v > p.Max[c.name] {
				p.Max[c.name] = v
			}
		}
	}
	return p
}

// projectRows are the window's projects, one per project of its sessions
// (a session with no project is in none): its sessions, typed prompts and
// those that got memory, the turns where Claude used memory, the memories
// reported wrong or outdated, the latest activity, the memories the server
// holds for it (null when the tags were not read), and a series of its
// typed prompts per bucket (value; the sum of the series is the prompts).
func projectRows(e env, w Window) ([]ProjectRow, error) {
	sessions, err := sessionRows(e, w)
	if err != nil {
		return nil, err
	}
	buckets := w.Buckets()
	byProject := map[string]*ProjectRow{}
	of := map[string]*ProjectRow{} // session → its project's row
	for _, s := range sessions {
		if s.Project == "" {
			continue
		}
		r := byProject[s.Project]
		if r == nil {
			r = &ProjectRow{Project: s.Project, Series: make([]Point, len(buckets))}
			for i, b := range buckets {
				r.Series[i] = pointOf(w, b.Key)
				r.Series[i].Value = ptr(0.0)
				r.Series[i].Partial = b.Partial
			}
			byProject[s.Project] = r
		}
		of[s.ID] = r
		r.Sessions++
		r.Prompts += s.Prompts
		r.PromptsWithMemory += s.PromptsWithMemory
		r.ReportedWrong += s.Wrong
		if r.LastActivity == nil || s.Last.After(*r.LastActivity) {
			last := s.Last
			r.LastActivity = &last
		}
	}
	prompts, err := e.tx.QueryContext(e.ctx, `
SELECT ts, session FROM tel_prompt
WHERE ts >= ? AND ts < ? AND COALESCE(session, '') <> '' AND (reason IS NULL OR reason <> 'machine') AND `+projectClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = prompts.Close() }()
	for prompts.Next() {
		var ts int64
		var session string
		if err := prompts.Scan(&ts, &session); err != nil {
			return nil, err
		}
		if r := of[session]; r != nil {
			if i := bucketIndex(buckets, time.UnixMilli(ts).In(w.Loc)); i >= 0 {
				*r.Series[i].Value++
			}
		}
	}
	if err := prompts.Err(); err != nil {
		return nil, err
	}
	turns, err := mainTurns(e, w)
	if err != nil {
		return nil, err
	}
	for _, t := range turns {
		if r := of[t.session]; r != nil && t.outcome == "declared" && t.anyUsed {
			r.UsedTurns++
		}
	}
	tags := tagCounts(e)
	out := make([]ProjectRow, 0, len(byProject))
	for _, r := range byProject {
		if tags != nil {
			r.MemoriesOnServer = ptr(tags[r.Project])
		}
		out = append(out, *r)
	}
	return out, nil
}

// tagCounts is the server's memory count per project tag, nil when the
// tags were not read. A project with no tag has 0.
func tagCounts(e env) map[string]int64 {
	if serverReason(e, "tags") != "" {
		return nil
	}
	out := map[string]int64{}
	for _, t := range e.srv.Tags {
		if p, ok := strings.CutPrefix(t.Tag, projectTagPrefix); ok {
			out[p] = int64(t.Count)
		}
	}
	return out
}

// projectPage is `projects <name>`: the six tile metrics for the project,
// its most used and never used memories (by the memory's project), its
// sessions, how many of its turns the path rule would file elsewhere, and
// the command to give it memories when the server holds none.
func projectPage(ctx context.Context, s Source, q Query, id string) (Result, error) {
	q.Project = id
	return runView(ctx, s, q, "projects", []string{"tags"}, func(e env, r *Result) error {
		tags := tagCounts(e)
		known, err := projectKnown(e, id, tags)
		if err != nil {
			return err
		}
		if !known {
			return NotFoundError{Kind: "project", ID: id}
		}
		if r.Metrics, err = computeMetrics(e, e.q.Window, overviewTiles); err != nil {
			return err
		}
		page := ProjectPage{Project: id}
		if page.TopUsed, page.NeverUsed, err = projectMemories(e, id); err != nil {
			return err
		}
		sessions, err := sessionRows(e, e.q.Window)
		if err != nil {
			return err
		}
		sortSessions(sessions)
		lo, hi, pg := pageBounds(e.q, len(sessions))
		page.Sessions = orEmpty(sessions[lo:hi])
		if err := withSent(e, page.Sessions); err != nil {
			return err
		}
		if err := e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(*) FROM tel_attribution
WHERE ts >= ? AND ts < ? AND COALESCE(subagent, 0) = 0 AND applied = ? AND COALESCE(path_rule, '') <> applied`,
			e.q.Window.Since.UnixMilli(), e.q.Window.Until.UnixMilli(), id).Scan(&page.AttributionDisagreements); err != nil {
			return err
		}
		if tags != nil {
			if _, has := tags[id]; !has {
				page.NoMemories = true
				page.Command = "backsight retain backfill --project " + id + " --plan"
			}
		}
		r.Tables.Project, r.Tables.Page = &page, &pg
		return nil
	})
}

// projectKnown reports whether the index or the server's tags know project
// id: a prompt, briefing or turn of it at any time, or a tag.
func projectKnown(e env, id string, tags map[string]int64) (bool, error) {
	if _, ok := tags[id]; ok {
		return true, nil
	}
	var known bool
	err := e.tx.QueryRowContext(e.ctx, `
SELECT EXISTS (SELECT 1 FROM turns WHERE project = ?)
    OR EXISTS (SELECT 1 FROM tel_prompt WHERE project = ?)
    OR EXISTS (SELECT 1 FROM tel_briefing WHERE project = ?)`, id, id, id).Scan(&known)
	return known, err
}

// projectMemories are the project's memories (by the memory's project, the
// latest show's) most used (recent-weighted credits, most shown then id
// first; only those used at all) and the never used ones of memory stale,
// at most the query's limit each.
func projectMemories(e env, id string) (top, never []ReviewRow, err error) {
	l, err := e.ledger()
	if err != nil {
		return nil, nil, err
	}
	lists, err := e.staleLists()
	if err != nil {
		return nil, nil, err
	}
	projects, err := e.memoryProjects()
	if err != nil {
		return nil, nil, err
	}
	p := ledgerParams(e.src.Config)
	limit := e.q.EffectiveLimit()
	row := func(mid, command string) ReviewRow {
		m := l.Memories[mid]
		r := ReviewRow{ID: mid, Project: id, Shown: int64(m.Shown), Command: command}
		r.LastShown = timeIn(m.LastShown, viewLoc(e))
		return r
	}
	type used struct {
		id      string
		credits float64
		shown   int
	}
	var us []used
	for mid, m := range l.Memories {
		if projects[mid] != id || m.RestoreOnly() || m.Hidden != nil {
			continue
		}
		if c := m.DecayedTo(e.now, p).Credits; c > 0 {
			us = append(us, used{mid, c, m.Shown})
		}
	}
	slices.SortFunc(us, func(a, b used) int {
		switch {
		case a.credits != b.credits:
			if a.credits > b.credits {
				return -1
			}
			return 1
		case a.shown != b.shown:
			return b.shown - a.shown
		}
		return strings.Compare(a.id, b.id)
	})
	top, never = []ReviewRow{}, []ReviewRow{}
	for _, u := range us[:min(limit, len(us))] {
		top = append(top, row(u.id, "backsight read "+u.id))
	}
	for _, mid := range lists.NeverUsed {
		if projects[mid] == id && len(never) < limit {
			never = append(never, row(mid, "backsight forget "+mid))
		}
	}
	return top, never, nil
}
