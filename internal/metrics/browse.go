package metrics

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// The browse views (projects, sessions, memories, days, releases, defs)
// list rows rather than judge a verdict. They share what is here: the
// not-found error, the paging and the sort check, the id lookups, and the
// send state and the turn outcomes of a session.
//
// Paging: every list takes Query.Limit and Query.Offset (EffectiveLimit and
// 0 by default), and says its total in Tables.Page. Every list has a
// stable order: the sort key, then a named tie-breaker, so a page is the
// same page twice. A view never prints memory text, prompts, notes or
// server error messages: ids, counts, times and enums only.

// NotFoundError says an id given to a browse view names nothing. Kind is
// session, memory, project or metric. TooShort: the id is a prefix of fewer
// than minPrefix characters. Matches: the ids a prefix matched when it
// matched several (at most maxMatches, sorted). The callers show it as
// no_data; it carries no free text.
type NotFoundError struct {
	Kind, ID string
	TooShort bool
	Matches  []string
}

func (e NotFoundError) Error() string {
	switch {
	case e.TooShort:
		return fmt.Sprintf("%s id: give at least %d characters", e.Kind, minPrefix)
	case len(e.Matches) > 1:
		return fmt.Sprintf("%s id: %d matches", e.Kind, len(e.Matches))
	}
	return e.Kind + " not found"
}

// minPrefix is the shortest id prefix a lookup accepts; maxMatches how many
// matches a NotFoundError lists.
const (
	minPrefix  = 8
	maxMatches = 10
)

// pageBounds is the slice [lo, hi) of a list of total rows that Query's
// limit and offset ask for, with the Page to report.
func pageBounds(q Query, total int) (lo, hi int, p Page) {
	limit, off := q.EffectiveLimit(), max(q.Offset, 0)
	lo = min(off, total)
	return lo, min(lo+limit, total), Page{Limit: limit, Offset: off, Total: total}
}

// checkSort is Query.Sort when it is one of allowed, def when empty, else
// an error naming the allowed keys.
func checkSort(q Query, def string, allowed ...string) (string, error) {
	switch {
	case q.Sort == "":
		return def, nil
	case slices.Contains(allowed, q.Sort):
		return q.Sort, nil
	}
	return "", fmt.Errorf("--sort: one of %s", strings.Join(allowed, ", "))
}

// allTime is a window holding every event of the index, for the pages of
// one session (which shows all of it, whatever the view's window).
func allTime(loc *time.Location) Window {
	w := Window{Since: time.UnixMilli(0).In(loc), Until: time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC).In(loc), TZ: loc.String(), Loc: loc}
	w.Days = ceilDays(w.Length())
	return w
}

// resolveID is the full id that id names in column col of tables, which the
// index holds: an exact id first, else the one id id is a prefix of (at
// least minPrefix characters). NotFoundError otherwise. tables and col are
// constants of the callers.
func resolveID(tx *sql.Tx, kind, id, col string, tables []string) (string, error) {
	for _, t := range tables {
		var found bool
		if err := tx.QueryRow(fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE %s = ?)`, t, col), id).Scan(&found); err != nil {
			return "", err
		}
		if found {
			return id, nil
		}
	}
	if len([]rune(id)) < minPrefix {
		return "", NotFoundError{Kind: kind, ID: id, TooShort: true}
	}
	hi := id + "\U0010FFFF"
	var union []string
	var args []any
	for _, t := range tables {
		union = append(union, fmt.Sprintf(`SELECT %[1]s AS id FROM %[2]s WHERE %[1]s >= ? AND %[1]s < ?`, col, t))
		args = append(args, id, hi)
	}
	rows, err := tx.Query(`SELECT DISTINCT id FROM (`+strings.Join(union, " UNION ")+`) ORDER BY id LIMIT ?`, append(args, maxMatches)...)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var matches []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return "", err
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", NotFoundError{Kind: kind, ID: id}
	case 1:
		return matches[0], nil
	}
	return "", NotFoundError{Kind: kind, ID: id, Matches: matches}
}

// sentBySession is each session's send state from the last status of its
// documents (conversation:<sid>, conversation:<sid>@<project> and their #n
// successors): ok only when all are ok, else the worst of the others
// (failed, quarantined, retry, queued, then any other); a session with no
// document is absent from the map.
func sentBySession(e env) (map[string]string, error) {
	docs, err := e.documentStatus()
	if err != nil {
		return nil, err
	}
	all := map[string][]string{}
	for doc, st := range docs {
		rest, ok := strings.CutPrefix(doc, "conversation:")
		if !ok {
			continue
		}
		if i := strings.IndexAny(rest, "@#"); i >= 0 {
			rest = rest[:i]
		}
		all[rest] = append(all[rest], st)
	}
	out := make(map[string]string, len(all))
	for sid, sts := range all {
		out[sid] = worstSent(sts)
	}
	return out, nil
}

// sentWorst orders the statuses that make a session not ok, worst first.
var sentWorst = []string{"failed", "quarantined", "retry", "queued"}

func worstSent(sts []string) string {
	for _, w := range sentWorst {
		if slices.Contains(sts, w) {
			return w
		}
	}
	slices.Sort(sts)
	for _, s := range sts {
		if s != "ok" {
			if s == "" {
				return "unknown"
			}
			return s
		}
	}
	return "ok"
}

// withSent fills the Sent of rows from the documents' statuses.
func withSent(e env, rows []SessionRow) error {
	sent, err := sentBySession(e)
	if err != nil {
		return err
	}
	for i := range rows {
		rows[i].Sent = sent[rows[i].ID]
	}
	return nil
}

// timeIn is t in loc, nil for the zero time: a time never set marshals as
// null, never as year 1.
func timeIn(t time.Time, loc *time.Location) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.In(loc)
	return &t
}

// cmpTimeDesc orders later times first and a missing time last.
func cmpTimeDesc(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return b.Compare(*a)
}

// orEmpty is xs, or an empty list when xs is nil: a list never marshals as
// null.
func orEmpty[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

// sortSessions orders rows latest activity first, then by id.
func sortSessions(rows []SessionRow) {
	slices.SortFunc(rows, func(a, b SessionRow) int {
		if c := b.Last.Compare(a.Last); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// mainTurn is a main-agent turn that showed memory in a window: its
// earliest and latest non-briefing show, its final outcome and whether it
// credited a memory used.
type mainTurn struct {
	session, turn string
	first, last   time.Time
	outcome       string // declared, undeclared, pending, check_skipped or another stop outcome
	anyUsed       bool
}

// mainTurns are the main-agent turns whose earliest non-briefing show is in
// w, for the query's project (the turn's own). The outcome follows the
// quality family: the final stop (journal order, a last blocked reads
// undeclared); with no stop, pending until feedback.OpenTurnLimit after the
// latest show, then undeclared.
func mainTurns(e env, w Window) ([]mainTurn, error) {
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	rows, err := e.tx.QueryContext(e.ctx, `
WITH cand AS (
  SELECT DISTINCT session, turn FROM shows
  WHERE ts >= ? AND ts < ? AND COALESCE(path, '') <> 'briefing' AND COALESCE(agent, '') = ''),
t AS (
  SELECT s.session, s.turn, MIN(s.ts) AS first, MAX(s.ts) AS last
  FROM shows s JOIN cand ON cand.session = s.session AND cand.turn = s.turn
  WHERE COALESCE(s.path, '') <> 'briefing' AND COALESCE(s.agent, '') = ''
  GROUP BY s.session, s.turn)
SELECT COALESCE(t.session, ''), COALESCE(t.turn, ''), t.first, t.last, f.outcome,
       EXISTS (SELECT 1 FROM credits c WHERE c.session = t.session AND c.turn = t.turn AND c.verdict = 'used')
FROM t LEFT JOIN turn_final_stop f ON f.session = t.session AND f.turn = t.turn AND COALESCE(f.agent, '') = ''
WHERE t.first >= ? AND t.first < ? AND `+sessionProject("t"),
		since, until, since, until, e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []mainTurn
	for rows.Next() {
		var t mainTurn
		var first, last int64
		var stop sql.NullString
		if err := rows.Scan(&t.session, &t.turn, &first, &last, &stop, &t.anyUsed); err != nil {
			return nil, err
		}
		t.first, t.last = time.UnixMilli(first).In(w.Loc), time.UnixMilli(last).In(w.Loc)
		t.outcome = turnOutcome(e.now, stop, t.last)
		out = append(out, t)
	}
	return out, rows.Err()
}

// turnOutcome is a turn's final outcome: the stop's, else pending or
// undeclared by the age of its latest show.
func turnOutcome(now time.Time, stop sql.NullString, lastShow time.Time) string {
	switch {
	case stop.Valid:
		return stop.String
	case now.Sub(lastShow) < feedback.OpenTurnLimit:
		return "pending"
	}
	return "undeclared"
}
