package metrics

import (
	"database/sql"
	"math"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/retain"
)

// The cost family: what backsight adds to Claude's context and to a
// prompt's wait (this PC's telemetry), and what the server's model spends
// to build memory (the server's LLM stats, for every PC using the server).

var costIDs = []string{"context.per_session", "context.per_prompt", "recall.latency", "search.latency",
	"extraction.tokens", "extraction.cost", "extraction.per_session"}

func init() { register("cost", costIDs, cost) }

// extractionOps are the server operations whose LLM use is named in
// extraction.tokens and extraction.cost; the rest of the total is "other".
// The names are those of the server's llm-requests operations, confirmed
// live on Hindsight 1.1.0. Recall is query-time, not extraction: it, and
// any other operation, stays in "other".
var extractionOps = []string{"retain", "consolidation", "refresh_mental_model"}

// CostServerNeeds are the server reads of a view holding the cost family
// (loadServer keys): the LLM stats of all operations and of each
// extraction operation over the view's period, and the 30-day stats of
// extraction.per_session.
func CostServerNeeds() []string {
	needs := []string{"llm:", llm30dKey}
	for _, op := range extractionOps {
		needs = append(needs, "llm:"+op)
	}
	return needs
}

// Sample sizes for the figures taken from a distribution: the p50 needs
// 20, the p95 needs 100 (below, the figure is null).
const (
	minForP50 = 20
	minForP95 = 100
)

// tagPartial marks an extraction figure whose per-operation split lacks an
// operation the server could not be asked for.
const tagPartial = "partial"

// sessionWindow is how far extraction.per_session looks back from now,
// always.
const sessionWindow = 28 * day

// histStep and histTop are the context histogram's bucket width and the
// start of its open bucket, in characters.
const (
	histStep = 1000
	histTop  = 20000
)

// histBucket is one bar of the context histogram; Hi is null for the open
// last bucket.
type histBucket struct {
	Lo int64  `json:"lo"`
	Hi *int64 `json:"hi"`
	N  int64  `json:"n"`
}

func cost(e env, w Window) (map[string]Observation, map[string]Metric, error) {
	obs := make(map[string]Observation, len(costIDs))
	met := make(map[string]Metric, len(costIDs))
	absent := func(ids []string, reason string) {
		for _, id := range ids {
			obs[id] = Observation{Absent: true, Reason: reason}
			met[id] = Metric{}
		}
	}
	// context.*: prompts and briefings; recall.latency: prompts;
	// search.latency: tool calls.
	for _, s := range []struct {
		ids    []string
		tables []string
		fill   func(env, Window, map[string]Observation, map[string]Metric) error
	}{
		{[]string{"context.per_session"}, []string{"tel_prompt", "tel_briefing"}, costSessions},
		{[]string{"context.per_prompt"}, []string{"tel_prompt"}, costPrompts},
		{[]string{"recall.latency"}, []string{"tel_prompt"}, costRecallLatency},
		{[]string{"search.latency"}, []string{"tel_tool"}, costSearchLatency},
	} {
		reason, none := "", true
		for _, t := range s.tables { // absent when every table is
			r, nn, err := reachAbsent(e, t)
			if err != nil {
				return nil, nil, err
			}
			if !nn {
				none = false
			} else if reason == "" {
				reason = r
			}
		}
		if none {
			absent(s.ids, reason)
			continue
		}
		if err := s.fill(e, w, obs, met); err != nil {
			return nil, nil, err
		}
	}
	costExtraction(e, w, obs, met)
	if err := costPerSession(e, w, obs, met); err != nil {
		return nil, nil, err
	}
	return obs, met, nil
}

// p50p95 are the percentiles of xs shown beside a value: the p50 from 20
// samples, the p95 from 100, null below.
func p50p95(xs []int64) (p50, p95 *float64) {
	if len(xs) >= minForP50 {
		v := Percentile(xs, 50)
		p50 = &v
	}
	if len(xs) >= minForP95 {
		v := Percentile(xs, 95)
		p95 = &v
	}
	return p50, p95
}

// tokensOf is chars / 4 rounded, null for a null figure.
func tokensOf(chars *float64) *int64 {
	if chars == nil {
		return nil
	}
	return i64(int64(math.Round(*chars / 4)))
}

// costSessions fills context.per_session: per session active in the window
// (a typed prompt or a briefing), the characters of its prompt recalls and
// briefings. Value and watch rule on the p95, from 100 sessions. The watch
// threshold is 1.5 times recall.session_budget_chars, set here at run time
// (the catalog has none for this metric).
func costSessions(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	// Typed prompts only (machine prompts are not the user's), as
	// internal/stats counts the prompt-hook text per session.
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT ts, session, COALESCE(chars, 0), 1 FROM tel_prompt
WHERE ts >= ? AND ts < ? AND COALESCE(session, '') <> '' AND COALESCE(reason, '') <> 'machine' AND `+projectClause+` AND `+versionClause+`
UNION ALL
SELECT ts, session, COALESCE(chars, 0), 0 FROM tel_briefing
WHERE ts >= ? AND ts < ? AND COALESCE(session, '') <> '' AND `+projectClause+` AND `+versionClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project, e.q.version, e.q.version,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project, e.q.version, e.q.version)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	buckets := w.Buckets()
	perSession := map[string]int64{}
	type dayAcc struct {
		briefing, prompts int64
		sessions          map[string]bool
	}
	days := make([]dayAcc, len(buckets))
	for i := range days {
		days[i].sessions = map[string]bool{}
	}
	for rows.Next() {
		var ts, chars int64
		var session string
		var prompt bool
		if err := rows.Scan(&ts, &session, &chars, &prompt); err != nil {
			return err
		}
		perSession[session] += chars
		if i := bucketIndex(buckets, time.UnixMilli(ts).In(w.Loc)); i >= 0 {
			if prompt {
				days[i].prompts += chars
			} else {
				days[i].briefing += chars
			}
			days[i].sessions[session] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	totals := make([]int64, 0, len(perSession))
	hist := make([]histBucket, histTop/histStep+1)
	for i := range hist {
		hist[i].Lo = int64(i * histStep)
		if i < histTop/histStep {
			hist[i].Hi = i64(int64((i + 1) * histStep))
		}
	}
	for _, c := range perSession {
		totals = append(totals, c)
		hist[min(c/histStep, histTop/histStep)].N++
	}
	n := int64(len(totals))
	p50, p95 := p50p95(totals)
	m := Metric{Den: i64(n), Value: p95,
		Extra: map[string]any{"p50": p50, "p95": p95, "tokens_p50": tokensOf(p50), "tokens_p95": tokensOf(p95), "histogram": hist}}
	for i, b := range buckets {
		p := pointOf(w, b.Key)
		p.N = i64(int64(len(days[i].sessions)))
		p.Parts = map[string]int64{"briefing": days[i].briefing, "prompts": days[i].prompts}
		m.Series = append(m.Series, p)
	}
	o := Observation{Den: n, P95: p95}
	if b := e.src.Config.Recall.SessionBudgetChars; b > 0 {
		wt := 1.5 * float64(b)
		o.Watch = &wt
	}
	obs["context.per_session"], met["context.per_session"] = o, m
	return nil
}

// costPrompts fills context.per_prompt: the characters of the recalls that
// injected something; the value is their p50 from 20 of them.
func costPrompts(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	xs, _, err := costSamples(e, w, `
SELECT ts, chars FROM tel_prompt
WHERE decision = 'recall' AND injected > 0 AND chars IS NOT NULL AND ts >= ? AND ts < ? AND `+projectClause+` AND `+versionClause)
	if err != nil {
		return err
	}
	p50, p95 := p50p95(xs.all)
	obs["context.per_prompt"] = Observation{Den: int64(len(xs.all))}
	met["context.per_prompt"] = Metric{Den: i64(int64(len(xs.all))), Value: p50, Extra: map[string]any{"p50": p50, "p95": p95}}
	return nil
}

// samples are the values of a query, whole and by bucket.
type samples struct {
	all   []int64
	byDay [][]int64
}

// costSamples reads (ts, value) rows of query (binding since, until, the
// project twice and the version twice) into samples.
func costSamples(e env, w Window, query string) (samples, []Bucket, error) {
	rows, err := e.tx.QueryContext(e.ctx, query, w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project, e.q.version, e.q.version)
	if err != nil {
		return samples{}, nil, err
	}
	defer func() { _ = rows.Close() }()
	buckets := w.Buckets()
	s := samples{byDay: make([][]int64, len(buckets))}
	for rows.Next() {
		var ts, v int64
		if err := rows.Scan(&ts, &v); err != nil {
			return samples{}, nil, err
		}
		s.all = append(s.all, v)
		if i := bucketIndex(buckets, time.UnixMilli(ts).In(w.Loc)); i >= 0 {
			s.byDay[i] = append(s.byDay[i], v)
		}
	}
	return s, buckets, rows.Err()
}

// latency fills a latency metric from the (ts, ms) rows of query: the
// value is the p95 from 100 samples; the series has a point per day with
// its own p50, p95 and n whatever n.
func latency(e env, w Window, id, query string, obs map[string]Observation, met map[string]Metric) error {
	xs, buckets, err := costSamples(e, w, query)
	if err != nil {
		return err
	}
	p50, p95 := p50p95(xs.all)
	n := int64(len(xs.all))
	m := Metric{Den: i64(n), Value: p95, Extra: map[string]any{"p50": p50, "p95": p95}}
	for i, b := range buckets {
		p := pointOf(w, b.Key)
		p.N = i64(int64(len(xs.byDay[i])))
		if len(xs.byDay[i]) > 0 {
			a, c := Percentile(xs.byDay[i], 50), Percentile(xs.byDay[i], 95)
			p.P50, p.P95 = &a, &c
		}
		m.Series = append(m.Series, p)
	}
	obs[id], met[id] = Observation{Den: n, P95: p95}, m
	return nil
}

func costRecallLatency(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	return latency(e, w, "recall.latency", `
SELECT ts, ms FROM tel_prompt
WHERE decision = 'recall' AND ms IS NOT NULL AND ts >= ? AND ts < ? AND `+projectClause+` AND `+versionClause, obs, met)
}

func costSearchLatency(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	return latency(e, w, "search.latency", `
SELECT ts, ms FROM tel_tool
WHERE name = 'memory_search' AND ok = 1 AND ms IS NOT NULL AND ts >= ? AND ts < ? AND `+projectClause+` AND `+versionClause, obs, met)
}

// tokens are the LLM tokens of a set of buckets. The one reading, here and
// nowhere else: the server's input INCLUDES the cached tokens (as
// retain.Prices.Cost, which prices input - cached fresh, and the stats
// printout, "input (cached)", assume), so cached is reported as a subset
// and the total is input + output. Output holds the thoughts too: they are
// billed as output (internal/cli/stats.go:311-348, extractionOver, L343).
type tokens struct{ input, cached, output int64 }

func (t tokens) total() int64 { return t.input + t.output }

func (t *tokens) add(b hindsight.LLMBucket) {
	t.input += b.Input
	t.cached += b.Cached
	t.output += b.Output + b.Thoughts
}

// bucketWidth is the width of a server bucket from the response's trunc:
// "hour" is an hour, "day" a day, anything else (or none) a day.
func bucketWidth(trunc string) time.Duration {
	if trunc == "hour" {
		return time.Hour
	}
	return day
}

// llmWidth is the bucket width of an LLM stats response, read from its
// buckets (every bucket of a response carries the same trunc).
func llmWidth(bs []hindsight.LLMBucket) time.Duration {
	if len(bs) == 0 {
		return day
	}
	return bucketWidth(bs[0].Trunc)
}

// inWindow sums the buckets that overlap [since, until), in total and by
// bucket of buckets (as wide as the response's trunc says), and counts the distinct local days they cover. A
// bucket straddling the start is kept whole, as extractionOver keeps whole
// UTC days, so the dashboard and `backsight stats` agree on one window, and
// counts in the first local day (as bank.growth's), so the days sum to the
// total. A bucket is placed by its start instant: west of UTC a UTC day's
// bucket starts on the previous local day.
func inWindow(bs []hindsight.LLMBucket, since, until time.Time, loc *time.Location, buckets []Bucket) (total tokens, byDay []tokens, covered int) {
	byDay = make([]tokens, len(buckets))
	width := llmWidth(bs)
	seen := map[string]bool{}
	for _, b := range bs {
		if !b.Time.Add(width).After(since) || !b.Time.Before(until) {
			continue
		}
		total.add(b)
		local := b.Time.In(loc)
		seen[local.Format("2006-01-02")] = true
		if local.Before(since) { // a bucket straddling the start counts in the first day
			local = since.In(loc)
		}
		if i := bucketIndex(buckets, local); i >= 0 {
			byDay[i].add(b)
		}
	}
	return total, byDay, len(seen)
}

// llmRead is the server's LLM buckets of key ("" = all operations, or
// llm30dKey), or the no_data reason of its failure.
func llmRead(e env, key string) ([]hindsight.LLMBucket, string) {
	if r := serverReason(e, key); r != "" {
		return nil, r
	}
	if key == llm30dKey {
		return e.srv.LLM30, ""
	}
	return e.srv.LLM[key[len("llm:"):]], ""
}

// byOperation is {op: {input, cached, output, cost}} for the named
// operations read and, when withOther, "other" = total − named; cost is null when no price is set.
func byOperation(named map[string]tokens, total tokens, p retain.Prices, withOther bool) map[string]any {
	costOf := func(t tokens) *float64 {
		if !p.Known() {
			return nil
		}
		c := p.Cost(t.input, t.cached, t.output, 0)
		return &c
	}
	entry := func(t tokens) map[string]any {
		return map[string]any{"input": t.input, "cached": t.cached, "output": t.output, "cost": costOf(t)}
	}
	out := map[string]any{}
	other := total
	for op, t := range named {
		out[op] = entry(t)
		other.input -= t.input
		other.cached -= t.cached
		other.output -= t.output
	}
	other.input, other.cached, other.output = max(0, other.input), max(0, other.cached), max(0, other.output)
	if withOther { // with an operation unread, the rest is not "other"
		out["other"] = entry(other)
	}
	return out
}

// costExtraction fills extraction.tokens and extraction.cost from the
// server's LLM stats over the view's period. A failed read of the total or
// of an operation makes both metrics no_data with the read's reason.
func costExtraction(e env, w Window, obs map[string]Observation, met map[string]Metric) {
	ids := []string{"extraction.tokens", "extraction.cost"}
	fail := func(reason string) {
		for _, id := range ids {
			obs[id] = Observation{Absent: true, Reason: reason}
			met[id] = Metric{}
		}
	}
	buckets := w.Buckets()
	all, reason := llmRead(e, "llm:")
	if reason != "" {
		fail(reason)
		return
	}
	total, byDay, covered := inWindow(all, w.Since, w.Until, w.Loc, buckets)
	// The total has its own read. An operation that cannot be read (the
	// server refuses it) only shrinks by_operation, even when every
	// operation fails: the metric is tagged partial and the failed reads
	// are counted.
	named := map[string]tokens{}
	failedOps := 0
	for _, op := range extractionOps {
		bs, reason := llmRead(e, "llm:"+op)
		if reason != "" {
			failedOps++
			continue
		}
		named[op], _, _ = inWindow(bs, w.Since, w.Until, w.Loc, buckets)
	}
	if covered == 0 { // the server holds nothing for the window
		fail(ReasonNoEvents)
		return
	}
	cfg := e.src.Config.LLM
	prices := retain.PricesFor(cfg.Model, cfg.PriceInput, cfg.PriceCached, cfg.PriceOutput, cfg.Currency)
	ops := byOperation(named, total, prices, failedOps == 0)

	tm := Metric{Value: ptr(float64(total.total())),
		Extra: map[string]any{"input": total.input, "cached": total.cached, "output": total.output,
			"by_operation": ops, "days_covered": covered}}
	cm := Metric{Extra: map[string]any{"price_set": prices.Known(), "currency": prices.Currency, "by_operation": ops}}
	if failedOps > 0 {
		tm.Tags, cm.Tags = []string{tagPartial}, []string{tagPartial}
		tm.Extra["failed_operations"], cm.Extra["failed_operations"] = failedOps, failedOps
	}
	if prices.Known() {
		cm.Value = ptr(prices.Cost(total.input, total.cached, total.output, 0))
	}
	for i, b := range buckets {
		p := pointOf(w, b.Key)
		p.Value = ptr(float64(byDay[i].total()))
		tm.Series = append(tm.Series, p)
		p = pointOf(w, b.Key)
		if prices.Known() {
			p.Value = ptr(prices.Cost(byDay[i].input, byDay[i].cached, byDay[i].output, 0))
		}
		cm.Series = append(cm.Series, p)
	}
	obs["extraction.tokens"], met["extraction.tokens"] = Observation{}, tm
	obs["extraction.cost"], met["extraction.cost"] = Observation{}, cm
}

func ptr[T any](v T) *T { return &v }

// costPerSession fills extraction.per_session, as of now: the extraction
// tokens of the last 28 days before now (the server's 30-day stats count
// back from now, so they serve no other span) over this PC's session
// documents whose last status is ok and which were sent in those same days.
// The tokens are every PC's on the server, the sessions this PC's. A
// window that is not the view's own reads no_events (no previous value).
func costPerSession(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	const id = "extraction.per_session"
	if !isCurrent(e, w) {
		obs[id], met[id] = Observation{Absent: true, Reason: ReasonNoEvents}, Metric{}
		return nil
	}
	since, until := e.now.Add(-sessionWindow), e.now
	bs, reason := llmRead(e, llm30dKey)
	if reason != "" {
		obs[id], met[id] = Observation{Absent: true, Reason: reason}, Metric{}
		return nil
	}
	var sent int64
	err := e.tx.QueryRowContext(e.ctx,
		`SELECT COUNT(*) FROM document_status WHERE status = 'ok' AND document LIKE 'conversation:%' AND ts >= ? AND ts < ?`,
		since.UnixMilli(), until.UnixMilli()).Scan(&sent)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	total, _, covered := inWindow(bs, since, until, w.Loc, nil)
	m := Metric{Extra: map[string]any{"sessions_sent": sent, "window_days": 28}}
	if sent == 0 || covered == 0 { // no document, or nothing of the server in the 28 days: no 0
		obs[id], met[id] = Observation{Absent: true, Reason: ReasonNoEvents}, m
		return nil
	}
	m.Value = ptr(float64(total.total()) / float64(sent))
	obs[id], met[id] = Observation{Den: sent}, m
	return nil
}
