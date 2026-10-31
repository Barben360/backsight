package metrics

import (
	"database/sql"
	"fmt"
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

// reachPrompts reads the window's prompts once and fills the four prompt
// metrics.
func reachPrompts(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	var names, args []any
	var cond []string
	for _, n := range memoryTools {
		cond = append(cond, fmt.Sprintf(`substr(name, -%d) = ?`, len(n)))
		names = append(names, n)
	}
	args = append(args, names...)
	args = append(args, w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT p.ts, p.decision, p.reason, p.plen, p.lang, p.injected, p.empty, s.n IS NOT NULL
FROM tel_prompt p
LEFT JOIN (SELECT DISTINCT session, n FROM tel_tool_use
           WHERE COALESCE(subagent, 0) = 0 AND (`+strings.Join(cond, " OR ")+`)) s
       ON s.session = p.session AND s.n = p.n
WHERE p.ts >= ? AND p.ts < ? AND (? = '' OR p.project = ?)`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	buckets := w.Buckets()
	total := newPromptAcc()
	days := make([]*promptAcc, len(buckets))
	for i := range days {
		days[i] = newPromptAcc()
	}
	for rows.Next() {
		var (
			ts                          int64
			decision, reason, lang, emp sql.NullString
			plen, injected              sql.NullInt64
			searched                    bool
		)
		if err := rows.Scan(&ts, &decision, &reason, &plen, &lang, &injected, &emp, &searched); err != nil {
			return err
		}
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
	if err := rows.Err(); err != nil {
		return err
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
	// hit rate, so the metric reads too_few or ok; it is not a per-language
	// verdict. Each language carries its own status below, judged with the
	// same min_n.
	def, _ := DefOf("recall.by_lang")
	byLang := make(map[string]langStat, len(total.langs))
	for lang, c := range total.langs {
		lo, hi := Wilson(c[0], c[1])
		j := Judge(def, Observation{Num: c[0], Den: c[1]})
		byLang[lang] = langStat{Num: c[0], Den: c[1], Value: float64(c[0]) / float64(c[1]), CI: &[2]float64{lo, hi}, Status: j.Status}
	}
	obs["recall.by_lang"] = Observation{Num: total.injecting, Den: total.recalls}
	met["recall.by_lang"] = Metric{Num: i64(total.injecting), Den: i64(total.recalls), Value: ratio(total.injecting, total.recalls),
		Extra: map[string]any{"by_lang": byLang}}
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
SELECT ts, ok, hits FROM tel_tool
WHERE name = 'memory_search' AND ts >= ? AND ts < ? AND (? = '' OR project = ?)`,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	buckets := w.Buckets()
	var total toolAcc
	days := make([]toolAcc, len(buckets))
	for rows.Next() {
		var ts int64
		var ok, hits sql.NullInt64
		if err := rows.Scan(&ts, &ok, &hits); err != nil {
			return err
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
	m := Metric{Num: i64(total.found0), Den: i64(total.ok), Value: ratio(total.found0, total.ok),
		Extra: map[string]any{"failed": total.failed}}
	for i, b := range buckets {
		p := pointOf(w, b.Key)
		p.Value, p.Num, p.Den = ratio(days[i].found0, days[i].ok), i64(days[i].found0), i64(days[i].ok)
		m.Series = append(m.Series, p)
	}
	obs["search.empty_rate"] = Observation{Num: total.found0, Den: total.ok}
	met["search.empty_rate"] = m
	return nil
}
