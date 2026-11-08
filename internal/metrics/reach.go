package metrics

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/stats"
)

// The reach family: does memory show up (recall.*), does Claude look for
// it (search.*). Everything is read from this PC's telemetry tables, in
// the window, for the session's project (tel_*.project) when the query
// names one.

var reachIDs = []string{"recall.hit_rate", "recall.empty_reasons", "recall.by_lang", "search.rate", "search.empty_rate"}

func init() { register("reach", reachIDs, reach) }

// memoryTools are the tool names (as suffixes: the MCP server prefixes
// them) whose use in a turn counts as Claude looking at memory.
var memoryTools = []string{"memory_search", "memory_read", "memory_store"}

// unknownKey labels a reason or a language the line did not carry.
const unknownKey = "unknown"

// langStat is one language of recall.by_lang.
type langStat struct {
	Num    int64       `json:"num"`
	Den    int64       `json:"den"`
	Value  float64     `json:"value"`
	CI     *[2]float64 `json:"ci"`
	Status Status      `json:"status"`
}

// promptAcc counts the prompts of one bucket (or of the whole window).
type promptAcc struct {
	recalls, injecting int64            // recall decisions, and those that injected
	empty              map[string]int64 // recalls that injected nothing, by empty reason
	skips              map[string]int64 // typed skips, by reason
	long, searched     int64            // typed prompts >= LongPrompt, and those whose turn searched
	langs              map[string][2]int64
}

func newPromptAcc() *promptAcc {
	return &promptAcc{empty: map[string]int64{}, skips: map[string]int64{}, langs: map[string][2]int64{}}
}

func (a *promptAcc) emptyTotal() int64 { return a.recalls - a.injecting }

// toolAcc counts memory_search calls.
type toolAcc struct{ ok, found0, failed int64 }

// pointOf is an empty point of the bucket key: hours for a window drawn
// by the hour, days otherwise.
func pointOf(w Window, key string) Point {
	if w.Length() <= hourlyMax {
		return Point{Hour: key}
	}
	return Point{Day: key}
}

// bucketIndex is the bucket holding t, -1 when none does.
func bucketIndex(bs []Bucket, t time.Time) int {
	i := sort.Search(len(bs), func(i int) bool { return t.Before(bs[i].Until) })
	if i == len(bs) || t.Before(bs[i].Since) {
		return -1
	}
	return i
}

func ratio(num, den int64) *float64 {
	if den <= 0 {
		return nil
	}
	v := float64(num) / float64(den)
	return &v
}

func i64(v int64) *int64 { return &v }

// projectClause is the project filter, two bind values.
const projectClause = `(? = '' OR project = ?)`

// versionClause is the binary-version filter of the releases view's
// metrics, two bind values (empty: no filter).
const versionClause = `(? = '' OR binary_version = ?)`

// reachAbsent reports why the family has nothing to read: a server-only
// host, or no row at all in table (any time) for the project.
func reachAbsent(e env, table string) (string, bool, error) {
	if e.src.Config.ServerOnly() {
		return ReasonServerOnlyHost, true, nil
	}
	var any bool
	err := e.tx.QueryRowContext(e.ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE %s)`, table, projectClause),
		e.q.Project, e.q.Project).Scan(&any)
	if err != nil {
		return "", false, err
	}
	if !any {
		return ReasonNoEvents, true, nil
	}
	return "", false, nil
}

// reach computes the five reach metrics over w.
func reach(e env, w Window) (map[string]Observation, map[string]Metric, error) {
	obs := make(map[string]Observation, len(reachIDs))
	met := make(map[string]Metric, len(reachIDs))
	absent := func(ids []string, reason string) {
		for _, id := range ids {
			obs[id] = Observation{Absent: true, Reason: reason}
			met[id] = Metric{}
		}
	}

	promptIDs := []string{"recall.hit_rate", "recall.empty_reasons", "recall.by_lang", "search.rate"}
	toolIDs := []string{"search.empty_rate"}
	reason, none, err := reachAbsent(e, "tel_prompt")
	if err != nil {
		return nil, nil, err
	}
	if none {
		absent(promptIDs, reason)
	} else if err := reachPrompts(e, w, obs, met); err != nil {
		return nil, nil, err
	}
	reason, none, err = reachAbsent(e, "tel_tool")
	if err != nil {
		return nil, nil, err
	}
	if none {
		absent(toolIDs, reason)
	} else if err := reachSearches(e, w, obs, met); err != nil {
		return nil, nil, err
	}
	return obs, met, nil
}

// promptRow is one prompt as reachPrompts reads it. lastSearch is the time
// of the latest main-agent memory-tool use of the prompt (session, n), 0
// for none: the prompt is searched for a window when it is at or after
// the window's start.
type promptRow struct {
	ts                          int64
	decision, reason, lang, emp sql.NullString
	plen, injected              sql.NullInt64
	lastSearch                  int64
}

// promptRead is the prompts of [since, until) (ms), of the env's project
// and version, in time order.
type promptRead struct {
	since, until int64
	rows         []promptRow
}

// readPrompts is the prompts covering w, read once per transaction: the
// first read spans the view's window, its previous one and its four
// baselines (what the families ask of reach), a window outside it widens
// the read to the union. The memo is keyed by the binary version (releases
// vary it), the rest of the Query being fixed for the env.
func readPrompts(e env, w Window) (*promptRead, error) {
	lo, hi := w.Since.UnixMilli(), w.Until.UnixMilli()
	if e.memo == nil {
		return loadPrompts(e, lo, hi)
	}
	if r, ok := e.memo.prompts[e.q.version]; ok && r.since <= lo && hi <= r.until {
		return r, nil
	}
	if r, ok := e.memo.prompts[e.q.version]; ok {
		lo, hi = min(lo, r.since), max(hi, r.until)
	} else if q := e.q.Window; !q.Since.IsZero() {
		lo, hi = min(lo, q.Previous().Since.UnixMilli(), q.Baselines()[3].Since.UnixMilli()), max(hi, q.Until.UnixMilli())
	}
	r, err := loadPrompts(e, lo, hi)
	if err != nil {
		return nil, err
	}
	if e.memo.prompts == nil {
		e.memo.prompts = map[string]*promptRead{}
	}
	e.memo.prompts[e.q.version] = r
	return r, nil
}

// loadPrompts reads the prompts of [lo, hi) with the time of their last
// memory-tool use. The tool uses are read to the end of time, not to hi: a
// prompt's search may come after the end of its window.
func loadPrompts(e env, lo, hi int64) (*promptRead, error) {
	var names, args []any
	var cond []string
	for _, n := range memoryTools {
		cond = append(cond, fmt.Sprintf(`substr(name, -%d) = ?`, len(n)))
		names = append(names, n)
	}
	args = append(args, lo)
	args = append(args, names...)
	args = append(args, lo, hi, e.q.Project, e.q.Project, e.q.version, e.q.version)
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT p.ts, p.decision, p.reason, p.plen, p.lang, p.injected, p.empty, COALESCE(s.last, 0)
FROM tel_prompt p
LEFT JOIN (SELECT session, n, MAX(ts) AS last FROM tel_tool_use
           WHERE ts >= ? AND COALESCE(subagent, 0) = 0 AND (`+strings.Join(cond, " OR ")+`)
           GROUP BY session, n) s
       ON s.session = p.session AND s.n = p.n
WHERE p.ts >= ? AND p.ts < ? AND (? = '' OR p.project = ?) AND (? = '' OR p.binary_version = ?)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	r := &promptRead{since: lo, until: hi}
	for rows.Next() {
		var p promptRow
		if err := rows.Scan(&p.ts, &p.decision, &p.reason, &p.plen, &p.lang, &p.injected, &p.emp, &p.lastSearch); err != nil {
			return nil, err
		}
		r.rows = append(r.rows, p)
	}
	return r, rows.Err()
}

// reachPrompts buckets the window's prompts (read once for all the
// windows) and fills the four prompt metrics.
func reachPrompts(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	read, err := readPrompts(e, w)
	if err != nil {
		return err
	}
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()

	buckets := w.Buckets()
	total := newPromptAcc()
	days := make([]*promptAcc, len(buckets))
	for i := range days {
		days[i] = newPromptAcc()
	}
	for _, p := range read.rows {
		if p.ts < since || p.ts >= until {
			continue
		}
		ts, decision, reason, lang, emp, plen, injected := p.ts, p.decision, p.reason, p.lang, p.emp, p.plen, p.injected
		searched := p.lastSearch >= since && p.lastSearch > 0
		accs := []*promptAcc{total}
		if i := bucketIndex(buckets, time.UnixMilli(ts).In(w.Loc)); i >= 0 {
			accs = append(accs, days[i])
		}
		typed := !reason.Valid || reason.String != "machine"
		for _, a := range accs {
			switch decision.String {
			case "recall":
				a.recalls++
				if injected.Int64 > 0 {
					a.injecting++
				} else {
					a.empty[orUnknown(emp.String)]++
				}
				l := a.langs[orUnknown(lang.String)]
				l[1]++
				if injected.Int64 > 0 {
					l[0]++
				}
				a.langs[orUnknown(lang.String)] = l
			case "skip":
				if typed {
					a.skips[orUnknown(reason.String)]++
				}
			}
			if typed && plen.Int64 >= stats.LongPrompt {
				a.long++
				if searched {
					a.searched++
				}
			}
		}
	}

	// recall.hit_rate
	hit := Metric{Num: i64(total.injecting), Den: i64(total.recalls), Value: ratio(total.injecting, total.recalls)}
	obs["recall.hit_rate"] = Observation{Num: total.injecting, Den: total.recalls}
	// search.rate
	rate := Metric{Num: i64(total.searched), Den: i64(total.long), Value: ratio(total.searched, total.long)}
	obs["search.rate"] = Observation{Num: total.searched, Den: total.long}
	for i, b := range buckets {
		d := days[i]
		p := pointOf(w, b.Key)
		p.Value, p.Num, p.Den = ratio(d.injecting, d.recalls), i64(d.injecting), i64(d.recalls)
		hit.Series = append(hit.Series, p)
		p = pointOf(w, b.Key)
		p.Value, p.Num, p.Den = ratio(d.searched, d.long), i64(d.searched), i64(d.long)
		rate.Series = append(rate.Series, p)
	}
	met["recall.hit_rate"], met["search.rate"] = hit, rate

	// recall.empty_reasons
	v := float64(total.emptyTotal())
	er := Metric{Value: &v, Extra: map[string]any{"empty": total.empty, "skips": total.skips}}
	obs["recall.empty_reasons"] = Observation{Den: total.recalls}
	for i, b := range buckets {
		p := pointOf(w, b.Key)
		dv := float64(days[i].emptyTotal())
		p.Value = &dv
		if len(days[i].empty) > 0 {
			p.Parts = days[i].empty
		}
		er.Series = append(er.Series, p)
	}
	met["recall.empty_reasons"] = er

	// recall.by_lang: the top-level status is the envelope of the overall
	// hit rate, so the metric reads too_few or ok, or watch when the language
	// test finds a language far behind. Each language carries its own status
	// below, judged with the same min_n.
	def, _ := DefOf("recall.by_lang")
	byLang := make(map[string]langStat, len(total.langs))
	for lang, c := range total.langs {
		lo, hi := Wilson(c[0], c[1])
		j := Judge(def, Observation{Num: c[0], Den: c[1]})
		byLang[lang] = langStat{Num: c[0], Den: c[1], Value: float64(c[0]) / float64(c[1]), CI: &[2]float64{lo, hi}, Status: j.Status}
	}
	// The language test (Absolute.On "test"): watch when a language is far
	// behind the most used one.
	test, behind := langTestOf(total.langs, int64(def.MinN))
	testStatus := StatusOK
	if behind {
		testStatus = StatusWatch
	}
	obs["recall.by_lang"] = Observation{Num: total.injecting, Den: total.recalls, Test: testStatus}
	met["recall.by_lang"] = Metric{Num: i64(total.injecting), Den: i64(total.recalls), Value: ratio(total.injecting, total.recalls),
		Extra: map[string]any{"by_lang": byLang, "test": test}}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return unknownKey
	}
	return s
}

// reachSearches reads the window's memory_search calls and fills
// search.empty_rate: calls that succeeded with no hit, out of calls that
// succeeded; failed calls are counted apart.
func reachSearches(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT ts, ok, hits, queries_n, queries_empty, result_chars FROM tel_tool
WHERE name = 'memory_search' AND ts >= ? AND ts < ? AND (? = '' OR project = ?)`,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	buckets := w.Buckets()
	var total toolAcc
	days := make([]toolAcc, len(buckets))
	// Of the successful calls that recorded them: the queries, those that
	// found none of the hits returned, and the answers' characters.
	var queries, emptyQueries, chars, sized int64
	for rows.Next() {
		var ts int64
		var ok, hits, queriesN, queriesEmpty, resultChars sql.NullInt64
		if err := rows.Scan(&ts, &ok, &hits, &queriesN, &queriesEmpty, &resultChars); err != nil {
			return err
		}
		if ok.Valid && ok.Int64 == 1 {
			queries += queriesN.Int64
			emptyQueries += queriesEmpty.Int64
			if resultChars.Valid {
				chars, sized = chars+resultChars.Int64, sized+1
			}
		}
		accs := []*toolAcc{&total}
		if i := bucketIndex(buckets, time.UnixMilli(ts).In(w.Loc)); i >= 0 {
			accs = append(accs, &days[i])
		}
		for _, a := range accs {
			switch {
			case ok.Valid && ok.Int64 == 1:
				a.ok++
				if hits.Valid && hits.Int64 == 0 {
					a.found0++
				}
			case ok.Valid && ok.Int64 == 0:
				a.failed++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var perSearch any // none recorded: no average
	if sized > 0 {
		perSearch = int64(math.Round(float64(chars) / float64(sized)))
	}
	m := Metric{Num: i64(total.found0), Den: i64(total.ok), Value: ratio(total.found0, total.ok),
		Extra: map[string]any{"failed": total.failed, "queries": queries, "empty_queries": emptyQueries, "chars_per_search": perSearch}}
	for i, b := range buckets {
		p := pointOf(w, b.Key)
		p.Value, p.Num, p.Den = ratio(days[i].found0, days[i].ok), i64(days[i].found0), i64(days[i].ok)
		m.Series = append(m.Series, p)
	}
	obs["search.empty_rate"] = Observation{Num: total.found0, Den: total.ok}
	met["search.empty_rate"] = m
	return nil
}
