package metrics

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// The quality family: is the memory shown right? Everything is read from
// the journal's tables (turns, shows, settles, credits, stops) and the
// ledger, attributed to the show's time: a turn belongs to the window of
// its earliest show, a show to the window of its time (a settle's show_at
// is that time), a credit to the window of its memory's earliest show in
// its turn. Under a project filter every row takes the session's project
// from turns (joined on session and turn), never shows.project, which is
// the memory's. imports rows never count.

var qualityIDs = []string{
	"used.turn_rate", "declare.coverage", "used.rubber_stamp",
	"used.share_confident", "used.rank", "used.by_path", "search.rescue_rate",
	"verdict.wrong_rate",
	"stale.never_used", "stale.hidden", "stale.reported",
}

func init() { register("quality", qualityIDs, quality) }

// tagUnreliable marks used.turn_rate while too few turns declare.
const tagUnreliable = "unreliable"

// defaultSharePath is the path used.share_confident reads without --path.
const defaultSharePath = "prompt"

// sessionProject is the project filter of a journal row aliased a: the
// project of its turn (two bind values: the project twice).
func sessionProject(a string) string {
	return fmt.Sprintf(`(? = '' OR EXISTS (SELECT 1 FROM turns tp WHERE tp.session = %[1]s.session AND tp.turn = %[1]s.turn AND tp.project = ?))`, a)
}

// sessionVersion is the binary-version filter of a journal row aliased a:
// the version of its turn (two bind values: the version twice; empty: no
// filter).
func sessionVersion(a string) string {
	return fmt.Sprintf(`(? = '' OR EXISTS (SELECT 1 FROM turns tv WHERE tv.session = %[1]s.session AND tv.turn = %[1]s.turn AND tv.binary_version = ?))`, a)
}

// quality computes the quality metrics over w.
func quality(e env, w Window) (map[string]Observation, map[string]Metric, error) {
	obs := make(map[string]Observation, len(qualityIDs))
	met := make(map[string]Metric, len(qualityIDs))
	absent := func(ids []string, reason string) {
		for _, id := range ids {
			obs[id] = Observation{Absent: true, Reason: reason}
			met[id] = Metric{}
		}
	}
	switch {
	case e.src.Config.ServerOnly():
		absent(qualityIDs, ReasonServerOnlyHost)
		return obs, met, nil
	case !e.src.Config.Feedback.Enabled:
		absent(qualityIDs, ReasonFeedbackOff)
		return obs, met, nil
	}

	// hasRows: the source holds a row (of the project, for the journal
	// tables) at any time.
	hasRows := func(table string) (bool, error) {
		query, args := `SELECT EXISTS (SELECT 1 FROM ledger)`, []any(nil)
		if table != "ledger" {
			query = `SELECT EXISTS (SELECT 1 FROM ` + table + ` s WHERE ` + sessionProject("s") + `)`
			args = []any{e.q.Project, e.q.Project}
		}
		var found bool
		err := e.tx.QueryRowContext(e.ctx, query, args...).Scan(&found)
		return found, err
	}
	for _, s := range []struct {
		ids   []string
		table string
		fill  func(e env, w Window, obs map[string]Observation, met map[string]Metric) error
	}{
		{[]string{"used.turn_rate", "declare.coverage", "used.rubber_stamp"}, "shows", qualityTurns},
		{[]string{"used.share_confident", "used.rank", "used.by_path", "search.rescue_rate"}, "shows", qualityShows},
		{[]string{"verdict.wrong_rate"}, "credits", qualityVerdicts},
		{[]string{"stale.never_used", "stale.hidden", "stale.reported"}, "ledger", qualityStale},
	} {
		found, err := hasRows(s.table)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			absent(s.ids, ReasonNoEvents)
			continue
		}
		if err := s.fill(e, w, obs, met); err != nil {
			return nil, nil, err
		}
	}
	return obs, met, nil
}

// turnAcc counts the turns that showed memory, of the window or of one of
// its buckets.
type turnAcc struct {
	declared, used, undeclared, pending, skipped int64
	stampDen, stampNum                           int64 // declared turns with ≥ 2 memories, and those crediting all
	subagent                                     int64
}

// qualityTurns reads the turns that showed memory (a non-briefing show)
// whose earliest such show is in w, and fills used.turn_rate,
// declare.coverage and used.rubber_stamp. A main-agent turn's outcome is
// its final stop (turn_final_stop: journal order, a last blocked reads
// undeclared); with no stop it is pending until feedback.OpenTurnLimit
// after its latest show, then undeclared (the settle marks its shows
// unknown then). Subagent turns are counted apart.
func qualityTurns(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	rows, err := e.tx.QueryContext(e.ctx, `
WITH cand AS (
  SELECT DISTINCT session, turn, COALESCE(agent, '') AS agent FROM shows
  WHERE ts >= ? AND ts < ? AND COALESCE(path, '') <> 'briefing'),
t AS (
  SELECT s.session, s.turn, cand.agent, MIN(s.ts) AS first, MAX(s.ts) AS last,
         COUNT(DISTINCT s.memory_id) AS mems,
         COUNT(DISTINCT CASE WHEN EXISTS (SELECT 1 FROM credits c WHERE c.session = s.session AND c.turn = s.turn
                AND c.memory_id = s.memory_id AND c.verdict = 'used') THEN s.memory_id END) AS credited
  FROM shows s JOIN cand ON cand.session = s.session AND cand.turn = s.turn AND cand.agent = COALESCE(s.agent, '')
  WHERE COALESCE(s.path, '') <> 'briefing'
  GROUP BY s.session, s.turn, cand.agent)
SELECT t.agent, t.first, t.last, t.mems, t.credited, f.outcome,
       EXISTS (SELECT 1 FROM credits c WHERE c.session = t.session AND c.turn = t.turn AND c.verdict = 'used')
FROM t LEFT JOIN turn_final_stop f ON f.session = t.session AND f.turn = t.turn AND COALESCE(f.agent, '') = ''
WHERE t.first >= ? AND t.first < ? AND `+sessionProject("t")+` AND `+sessionVersion("t"),
		since, until, since, until, e.q.Project, e.q.Project, e.q.version, e.q.version)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	buckets := w.Buckets()
	var total turnAcc
	days := make([]turnAcc, len(buckets))
	for rows.Next() {
		var (
			agent          string
			first, last    int64
			mems, credited int64
			outcome        sql.NullString
			anyUsed        bool
		)
		if err := rows.Scan(&agent, &first, &last, &mems, &credited, &outcome, &anyUsed); err != nil {
			return err
		}
		accs := []*turnAcc{&total}
		if i := bucketIndex(buckets, time.UnixMilli(first).In(w.Loc)); i >= 0 {
			accs = append(accs, &days[i])
		}
		out := outcome.String
		if !outcome.Valid {
			out = "undeclared"
			if e.now.Sub(time.UnixMilli(last)) < feedback.OpenTurnLimit {
				out = "pending"
			}
		}
		for _, a := range accs {
			if agent != "" {
				a.subagent++
				continue
			}
			switch out {
			case "declared":
				a.declared++
				if anyUsed {
					a.used++
				}
				if mems >= 2 {
					a.stampDen++
					if credited == mems {
						a.stampNum++
					}
				}
			case "undeclared":
				a.undeclared++
			case "pending":
				a.pending++
			case "check_skipped":
				a.skipped++
			} // an outcome outside the closed set (other) counts nowhere
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	decided := total.declared + total.undeclared
	cov := Metric{Num: i64(total.declared), Den: i64(decided), Value: ratio(total.declared, decided),
		Extra: map[string]any{"pending": total.pending, "check_skipped": total.skipped}}
	rate := Metric{Num: i64(total.used), Den: i64(total.declared), Value: ratio(total.used, total.declared),
		Extra: map[string]any{"worst_case": ratio(total.used, decided), "pending": total.pending,
			"check_skipped": total.skipped, "subagent_turns": total.subagent}}
	for i, b := range buckets {
		d := days[i]
		p := pointOf(w, b.Key)
		p.Value, p.Num, p.Den = ratio(d.used, d.declared), i64(d.used), i64(d.declared)
		rate.Series = append(rate.Series, p)
		p = pointOf(w, b.Key)
		p.Value, p.Num, p.Den = ratio(d.declared, d.declared+d.undeclared), i64(d.declared), i64(d.declared+d.undeclared)
		cov.Series = append(cov.Series, p)
	}
	// unreliable while the coverage's upper bound is under its watch
	// threshold (80%), once the coverage has the turns to be judged: a
	// bound from a handful of turns says nothing.
	covDef, _ := DefOf("declare.coverage")
	if _, hi := Wilson(total.declared, decided); decided >= int64(covDef.MinN) && hi < *covDef.Absolute.Watch {
		rate.Tags = append(rate.Tags, tagUnreliable)
	}
	obs["used.turn_rate"] = Observation{Num: total.used, Den: total.declared}
	obs["declare.coverage"] = Observation{Num: total.declared, Den: decided}
	obs["used.rubber_stamp"] = Observation{Num: total.stampNum, Den: total.stampDen}
	met["used.turn_rate"], met["declare.coverage"] = rate, cov
	met["used.rubber_stamp"] = Metric{Num: i64(total.stampNum), Den: i64(total.stampDen), Value: ratio(total.stampNum, total.stampDen)}
	return nil
}

// shareAcc counts the shows of one path by settle outcome.
type shareAcc struct{ credit, miss, unknown, shown, forgotten, pending int64 }

func (a shareAcc) decided() int64 { return a.credit + a.miss }

// pathStat is one path of used.by_path.
type pathStat struct {
	Num    int64    `json:"num"`
	Den    int64    `json:"den"`
	Value  *float64 `json:"value"`
	Status Status   `json:"status"`
}

// rankKeys are used.rank's histogram buckets.
var rankKeys = []string{"1", "2", "3", "4", "5", "6+", "unranked"}

func rankKey(rank sql.NullInt64) string {
	switch {
	case !rank.Valid || rank.Int64 <= 0:
		return "unranked"
	case rank.Int64 >= 6:
		return "6+"
	}
	return fmt.Sprint(rank.Int64)
}

// qualityShows reads the shows of w (any agent, any path but the never
// settled sources) with their settle outcome, joined by key, and fills
// used.share_confident, used.by_path, used.rank and search.rescue_rate. A
// show with no settle row yet is pending.
func qualityShows(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	rows, err := e.tx.QueryContext(e.ctx, `
WITH st AS (
  SELECT key, outcome, MIN(settle_ts) AS at FROM settles
  WHERE key IS NOT NULL AND show_at >= ? AND show_at < ? GROUP BY key)
SELECT s.key, COALESCE(s.session, ''), COALESCE(s.turn, ''), COALESCE(s.path, ''), s.rank, s.ts, st.outcome,
       EXISTS (SELECT 1 FROM shows p WHERE p.session = s.session AND p.turn = s.turn AND p.path = 'prompt')
FROM shows s LEFT JOIN st ON st.key = s.key
WHERE s.ts >= ? AND s.ts < ? AND s.key IS NOT NULL AND COALESCE(s.path, '') <> ? AND `+sessionProject("s"),
		since, until, since, until, feedback.PathSources, e.q.Project, e.q.Project)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	path := e.q.Path
	if path == "" {
		path = defaultSharePath
	}
	buckets := w.Buckets()
	var share shareAcc
	days := make([]shareAcc, len(buckets))
	byPath := map[string]*shareAcc{}
	hist := make(map[string]int64, len(rankKeys))
	for _, k := range rankKeys {
		hist[k] = 0
	}
	var credited int64
	bestRank := map[string]int64{} // session|turn with a credited show: its best rank, 0 when none ranked
	var rescueNum, rescueDen int64
	seen := map[string]bool{}
	for rows.Next() {
		var (
			key, session, turn, p string
			rank                  sql.NullInt64
			ts                    int64
			outcome               sql.NullString
			hasPrompt             bool
		)
		if err := rows.Scan(&key, &session, &turn, &p, &rank, &ts, &outcome, &hasPrompt); err != nil {
			return err
		}
		if seen[key] { // one show per key, as the settle decides it
			continue
		}
		seen[key] = true
		add := func(a *shareAcc) {
			switch {
			case !outcome.Valid:
				a.pending++
			case outcome.String == feedback.OutcomeCredit:
				a.credit++
			case outcome.String == feedback.OutcomeMiss:
				a.miss++
			case outcome.String == feedback.OutcomeUnknown:
				a.unknown++
			case outcome.String == feedback.OutcomeShown:
				a.shown++
			case outcome.String == feedback.OutcomeForgotten:
				a.forgotten++
			}
		}
		if p == path {
			add(&share)
			if i := bucketIndex(buckets, time.UnixMilli(ts).In(w.Loc)); i >= 0 {
				add(&days[i])
			}
		}
		if byPath[p] == nil {
			byPath[p] = &shareAcc{}
		}
		add(byPath[p])
		if !outcome.Valid || outcome.String != feedback.OutcomeCredit {
			continue
		}
		credited++
		hist[rankKey(rank)]++
		t := session + "|" + turn
		best := bestRank[t]
		if r := rank.Int64; rank.Valid && r > 0 && (best == 0 || r < best) {
			best = r
		}
		bestRank[t] = best
		if hasPrompt {
			rescueDen++
			if p == "search" {
				rescueNum++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// used.share_confident
	sm := Metric{Num: i64(share.credit), Den: i64(share.decided()), Value: ratio(share.credit, share.decided()),
		Extra: map[string]any{"unknown": share.unknown, "shown": share.shown, "forgotten": share.forgotten,
			"pending": share.pending, "path": path}}
	for i, b := range buckets {
		pt := pointOf(w, b.Key)
		pt.Value, pt.Num, pt.Den = ratio(days[i].credit, days[i].decided()), i64(days[i].credit), i64(days[i].decided())
		sm.Series = append(sm.Series, pt)
	}
	obs["used.share_confident"] = Observation{Num: share.credit, Den: share.decided()}
	met["used.share_confident"] = sm

	// used.by_path: each path with a decided show, judged on its own
	// trials; the value is the prompt path's.
	def, _ := DefOf("used.by_path")
	paths := make(map[string]pathStat, len(byPath))
	for p, a := range byPath {
		if a.decided() == 0 {
			continue
		}
		j := Judge(def, Observation{Num: a.credit, Den: a.decided()})
		paths[p] = pathStat{Num: a.credit, Den: a.decided(), Value: ratio(a.credit, a.decided()), Status: j.Status}
	}
	prompt := paths[defaultSharePath]
	obs["used.by_path"] = Observation{Num: prompt.Num, Den: prompt.Den}
	met["used.by_path"] = Metric{Num: i64(prompt.Num), Den: i64(prompt.Den), Value: ratio(prompt.Num, prompt.Den),
		Extra: map[string]any{"by_path": paths}}

	// used.rank: the value counts the credited shows; the trials are the
	// turns with one.
	turns := int64(len(bestRank))
	var at1, at3 int64
	for _, r := range bestRank {
		if r == 1 {
			at1++
		}
		if r >= 1 && r <= 3 {
			at3++
		}
	}
	v := float64(credited)
	obs["used.rank"] = Observation{Den: turns}
	met["used.rank"] = Metric{Value: &v, Den: i64(turns),
		Extra: map[string]any{"histogram": hist, "hit_at_1": ratio(at1, turns), "hit_at_3": ratio(at3, turns)}}

	// search.rescue_rate
	obs["search.rescue_rate"] = Observation{Num: rescueNum, Den: rescueDen}
	met["search.rescue_rate"] = Metric{Num: i64(rescueNum), Den: i64(rescueDen), Value: ratio(rescueNum, rescueDen)}
	return nil
}

// qualityVerdicts fills verdict.wrong_rate: the per-memory credits used,
// wrong or outdated (a credit naming no memory is none of them), each at
// the time of its memory's earliest show in its turn, else at its own
// time. A memory is credited after it is shown, so a credit before the
// window's start cannot belong to it. A credit has no upper bound: one
// written after the window's end for a show inside it still counts.
//
// The show join is the same (session, turn, memory). A credit that
// feedback.Decide attached to the latest earlier show of its session, in an
// earlier turn than the credit's own, finds no show here and falls back to
// the credit's own time: that is the rule as specified, not a lost row.
func qualityVerdicts(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT verdict, at FROM (
  SELECT c.verdict, COALESCE((SELECT MIN(s.ts) FROM shows s
           WHERE s.session = c.session AND s.turn = c.turn AND s.memory_id = c.memory_id), c.ts) AS at
  FROM credits c
  WHERE c.memory_id IS NOT NULL AND c.verdict IN ('used', 'wrong', 'outdated') AND c.ts >= ? AND `+sessionProject("c")+` AND `+sessionVersion("c")+`)
WHERE at >= ? AND at < ?`,
		since, e.q.Project, e.q.Project, e.q.version, e.q.version, since, until)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	buckets := w.Buckets()
	type acc struct{ used, wrong, outdated int64 }
	var total acc
	days := make([]acc, len(buckets))
	for rows.Next() {
		var verdict string
		var at int64
		if err := rows.Scan(&verdict, &at); err != nil {
			return err
		}
		accs := []*acc{&total}
		if i := bucketIndex(buckets, time.UnixMilli(at).In(w.Loc)); i >= 0 {
			accs = append(accs, &days[i])
		}
		for _, a := range accs {
			switch verdict {
			case "used":
				a.used++
			case "wrong":
				a.wrong++
			case "outdated":
				a.outdated++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	num, den := total.wrong+total.outdated, total.used+total.wrong+total.outdated
	m := Metric{Num: i64(num), Den: i64(den), Value: ratio(num, den),
		Extra: map[string]any{"wrong": total.wrong, "outdated": total.outdated, "used": total.used}}
	for i, b := range buckets {
		d := days[i]
		pn, pd := d.wrong+d.outdated, d.used+d.wrong+d.outdated
		p := pointOf(w, b.Key)
		p.Value, p.Num, p.Den = ratio(pn, pd), i64(pn), i64(pd)
		m.Series = append(m.Series, p)
	}
	obs["verdict.wrong_rate"] = Observation{Num: num, Den: den}
	met["verdict.wrong_rate"] = m
	return nil
}

// qualityStale fills the stale counts: the ledger rows rebuilt as entries
// and sorted by feedback.Stale, memory stale's own definition, at now
// (the ledger has no window). The ledger has no project: under a project
// filter the counts are tagged not filtered.
func qualityStale(e env, _ Window, obs map[string]Observation, met map[string]Metric) error {
	lists, err := e.staleLists()
	if err != nil {
		return err
	}
	var tags []string
	if e.q.Project != "" {
		tags = []string{tagNotFiltered}
	}
	for id, ids := range map[string][]string{"stale.never_used": lists.NeverUsed, "stale.hidden": lists.Hidden, "stale.reported": lists.Reported} {
		v := float64(len(ids))
		obs[id] = Observation{Den: int64(len(ids)), Value: &v}
		met[id] = Metric{Value: &v, Tags: tags}
	}
	return nil
}

// memoryProject is a memory's project for the memory tables: the project
// of its latest show that carries one (shows.project is the memory's;
// the ledger has none), "" when none is known. The bank metrics filter
// retirements by it (bank.churn does the same in SQL, falling back to the
// session's project); activity metrics filter on the session's project.
func memoryProject(tx *sql.Tx, id string) string {
	var p string
	err := tx.QueryRow(`SELECT project FROM shows WHERE memory_id = ? AND project IS NOT NULL
		ORDER BY ts DESC, source_file DESC, "offset" DESC, pos DESC LIMIT 1`, id).Scan(&p)
	if err != nil {
		return ""
	}
	return p
}

// ledger is the index's ledger rows as a feedback.Ledger (what
// feedback.Stale reads), read once per transaction.
func (e env) ledger() (feedback.Ledger, error) {
	if e.memo != nil && e.memo.ledger != nil {
		return *e.memo.ledger, nil
	}
	l, err := readLedger(e)
	if err == nil && e.memo != nil {
		e.memo.ledger = &l
	}
	return l, err
}

func readLedger(e env) (feedback.Ledger, error) {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT memory_id, shown, credits, credits_at, misses, misses_at, last_shown, last_credited,
       wrong, hidden_since, unhidden_at, imported_shown, imported_credits FROM ledger`)
	if err != nil {
		return feedback.Ledger{}, err
	}
	defer func() { _ = rows.Close() }()
	l := feedback.Ledger{Memories: map[string]feedback.Entry{}}
	for rows.Next() {
		var (
			id                                                        string
			shown, wrong, imported                                    sql.NullInt64
			credits, misses, importedCredits                          sql.NullFloat64
			creditsAt, missesAt, lastShown, lastCredited, hiddenSince sql.NullInt64
			unhiddenAt                                                sql.NullInt64
		)
		if err := rows.Scan(&id, &shown, &credits, &creditsAt, &misses, &missesAt, &lastShown, &lastCredited,
			&wrong, &hiddenSince, &unhiddenAt, &imported, &importedCredits); err != nil {
			return feedback.Ledger{}, err
		}
		m := feedback.Entry{Shown: int(shown.Int64), Credits: credits.Float64, Misses: misses.Float64, Wrong: int(wrong.Int64),
			Imported:  feedback.Imported{Shown: int(imported.Int64), Credits: importedCredits.Float64},
			CreditsAt: msTime(creditsAt), MissesAt: msTime(missesAt), LastShown: msTime(lastShown),
			LastCredited: msTime(lastCredited), UnhiddenAt: msTime(unhiddenAt)}
		if hiddenSince.Valid {
			m.Hidden = &feedback.Hidden{Since: time.UnixMilli(hiddenSince.Int64)}
		}
		l.Memories[id] = m
	}
	return l, rows.Err()
}

// msTime is a nullable unix-millisecond column as a time, zero when null.
func msTime(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64)
}

// staleLists is feedback.Stale of the index's ledger as of now, read once
// per transaction (the stale counts do not depend on the window).
func (e env) staleLists() (feedback.StaleLists, error) {
	if e.memo != nil && e.memo.stale != nil {
		return *e.memo.stale, nil
	}
	l, err := e.ledger()
	if err != nil {
		return feedback.StaleLists{}, err
	}
	lists := feedback.Stale(l, e.src.Config.Feedback.StaleShows, e.now)
	if e.memo != nil {
		e.memo.stale = &lists
	}
	return lists, nil
}
