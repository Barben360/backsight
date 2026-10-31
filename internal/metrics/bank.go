package metrics

import (
	"fmt"
	"strings"
	"time"
)

// The bank family: what the server holds (stats, time series, project tags;
// for every PC using it) and what this PC recorded of storing and retiring
// memories (the store events and the journal). User-visible wording says
// "memory".

var bankIDs = []string{"bank.size", "bank.growth", "bank.by_project", "bank.consolidation", "bank.churn", "notes.stored"}

func init() { register("bank", bankIDs, bank) }

// BankServerNeeds are the server reads of a view holding the bank family
// (loadServer keys): the stats, the memories time series and the project
// tags.
func BankServerNeeds() []string { return []string{"stats", "series", "tags"} }

// projectTagPrefix starts the tag a memory carries for its project.
const projectTagPrefix = "project:"

// tagSnapshot marks a figure as of now: the server keeps no history of it,
// so the view has no previous value to show (not "no change").
const tagSnapshot = "snapshot"

// maxSeriesDays is the longest period of the server's time series.
const maxSeriesDays = 90

// The retirement kinds of bank.churn, in the order of its catalog extras.
var churnKinds = []string{"forget", "retire", "hide", "unhide"}

// The fact types of the memories time series.
var seriesKinds = []string{"world", "experience", "observation"}

// sizeKeys are the server reads of bank.size: a project's size is its tag
// count alone.
func sizeKeys(q Query) []string {
	if q.Project != "" {
		return []string{"tags"}
	}
	return []string{"stats"}
}

func bank(e env, w Window) (map[string]Observation, map[string]Metric, error) {
	obs := make(map[string]Observation, len(bankIDs))
	met := make(map[string]Metric, len(bankIDs))
	absent := func(id, reason string) {
		obs[id], met[id] = Observation{Absent: true, Reason: reason}, Metric{}
	}
	// The server's figures are as of now: only the view's own window can be
	// read from them; the previous window reads no_events (no previous
	// value, no delta).
	current := w.Since.Equal(e.q.Window.Since) && w.Until.Equal(e.q.Window.Until)
	for _, s := range []struct {
		id   string
		keys []string
		fill func(env, Window, map[string]Observation, map[string]Metric)
	}{
		{"bank.size", sizeKeys(e.q), bankSize},
		{"bank.growth", []string{"series"}, bankGrowth},
		{"bank.by_project", []string{"tags"}, bankByProject},
		{"bank.consolidation", []string{"stats"}, bankConsolidation},
	} {
		reason := ""
		for _, k := range s.keys {
			if reason = serverReason(e, k); reason != "" {
				break
			}
		}
		if reason == "" && !current {
			reason = ReasonNoEvents
		}
		if reason != "" {
			absent(s.id, reason)
			continue
		}
		s.fill(e, w, obs, met)
	}
	if err := bankChurn(e, w, obs, met); err != nil {
		return nil, nil, err
	}
	if err := bankStored(e, w, obs, met); err != nil {
		return nil, nil, err
	}
	return obs, met, nil
}

// bankSize fills bank.size: the nodes of every fact type, or under a
// project filter that project's tag count.
func bankSize(e env, _ Window, obs map[string]Observation, met map[string]Metric) {
	const id = "bank.size"
	if p := e.q.Project; p != "" {
		var n int
		for _, t := range e.srv.Tags {
			if t.Tag == projectTagPrefix+p {
				n = t.Count
			}
		}
		extra := map[string]any{}
		for _, k := range bankSizeExtras {
			extra[k] = nil
		}
		obs[id], met[id] = Observation{Filtered: true}, Metric{Value: ptr(float64(n)), Extra: extra, Tags: []string{tagSnapshot}}
		return
	}
	st := e.srv.Stats
	var total int64
	for _, n := range st.NodesByFactType {
		total += int64(n)
	}
	obs[id] = Observation{}
	met[id] = Metric{Value: ptr(float64(total)), Extra: map[string]any{
		"world": int64(st.NodesByFactType["world"]), "experience": int64(st.NodesByFactType["experience"]),
		"observation": int64(st.NodesByFactType["observation"]),
		"documents":   int64(st.TotalDocuments), "links": int64(st.TotalLinks)},
		Tags: []string{tagSnapshot}}
}

var bankSizeExtras = []string{"world", "experience", "observation", "documents", "links"}

// bankGrowth fills bank.growth from the server's time series, read for the
// period asked (seriesPeriod): the buckets overlapping the window, whole
// (as inWindow keeps them), summed by kind (extra) and re-bucketed to
// local days.
func bankGrowth(e env, w Window, obs map[string]Observation, met map[string]Metric) {
	const id = "bank.growth"
	buckets := w.Buckets()
	width := bucketWidth(e.srv.Series.Trunc)
	byDay := make([]map[string]int64, len(buckets))
	for i := range byDay {
		byDay[i] = map[string]int64{}
	}
	seen := map[string]bool{}
	var total int64
	byKind := map[string]int64{}
	var kept int
	for _, b := range e.srv.Series.Buckets {
		if !b.Time.Add(width).After(w.Since) || !b.Time.Before(w.Until) {
			continue
		}
		local := b.Time.In(w.Loc)
		seen[local.Format("2006-01-02")] = true
		kept++
		if local.Before(w.Since) { // a bucket straddling the start counts in the first day
			local = w.Since
		}
		i := bucketIndex(buckets, local)
		for k, n := range map[string]int64{"world": b.World, "experience": b.Experience, "observation": b.Observation} {
			total += n
			byKind[k] += n
			if i >= 0 {
				byDay[i][k] += n
			}
		}
	}
	covered := len(seen)
	if covered == 0 { // the server holds nothing for the window
		obs[id], met[id] = Observation{Absent: true, Reason: ReasonNoEvents}, Metric{}
		return
	}
	// The rate is over the time the counted buckets span (whole buckets,
	// the first one possibly before the window), not the window's length.
	days := float64(kept) * width.Hours() / 24
	m := Metric{Tags: []string{tagSnapshot}, Extra: map[string]any{}}
	for _, k := range seriesKinds { // the memories added over the buckets kept, by kind
		m.Extra[k] = byKind[k]
	}
	if w.Length() > maxSeriesDays*day { // the series cannot reach back further
		m.Extra["days_covered"] = covered
	}
	m.Value = ptr(float64(total) / days)
	for i, b := range buckets {
		p := pointOf(w, b.Key)
		p.Parts = map[string]int64{}
		var sum int64
		for _, k := range seriesKinds {
			p.Parts[k] = byDay[i][k]
			sum += byDay[i][k]
		}
		p.Value = ptr(float64(sum))
		m.Series = append(m.Series, p)
	}
	obs[id], met[id] = Observation{}, m
}

// bankByProject fills bank.by_project: the number of projects with a tag.
func bankByProject(e env, _ Window, obs map[string]Observation, met map[string]Metric) {
	const id = "bank.by_project"
	var n int
	for _, t := range e.srv.Tags {
		if strings.HasPrefix(t.Tag, projectTagPrefix) {
			n++
		}
	}
	obs[id], met[id] = Observation{}, Metric{Value: ptr(float64(n)), Tags: []string{tagSnapshot}}
}

// bankConsolidation fills bank.consolidation: the summaries pending, and
// the failed ones (the server's current total) that make it watched.
func bankConsolidation(e env, w Window, obs map[string]Observation, met map[string]Metric) {
	const id = "bank.consolidation"
	st := e.srv.Stats
	extra := map[string]any{"pending": int64(st.PendingConsolidation), "failed": int64(st.FailedConsolidation),
		"last_consolidated_at": nil}
	if !st.LastConsolidatedAt.IsZero() {
		extra["last_consolidated_at"] = st.LastConsolidatedAt.In(w.Loc)
	}
	obs[id] = Observation{Err: ruleConsolidation(int64(st.FailedConsolidation))}
	met[id] = Metric{Value: ptr(float64(st.PendingConsolidation)), Extra: extra, Tags: []string{tagSnapshot}}
}

// bankChurn fills bank.churn: the retire, forget, hide and unhide rows of
// the journal in the window (one per memory), for the memories of the
// project when the query names one, in ISO weeks of the window's zone.
func bankChurn(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	const id = "bank.churn"
	absent := func(reason string) {
		obs[id], met[id] = Observation{Absent: true, Reason: reason}, Metric{}
	}
	switch {
	case e.src.Config.ServerOnly():
		absent(ReasonServerOnlyHost)
		return nil
	case !e.src.Config.Feedback.Enabled:
		absent(ReasonFeedbackOff)
		return nil
	}
	// The project is the memory's: that of its latest show carrying one (as
	// memoryProject), else, for a memory never shown, its session's (the
	// retirements hold no project).
	const inProject = `(? = '' OR COALESCE(
 (SELECT sh.project FROM shows sh WHERE sh.memory_id = r.memory_id AND sh.project IS NOT NULL
    ORDER BY sh.ts DESC, sh.source_file DESC, sh."offset" DESC, sh.pos DESC LIMIT 1),
 (SELECT tp.project FROM turns tp WHERE tp.session = r.session AND tp.project IS NOT NULL
    ORDER BY tp.ts DESC, tp.source_file DESC, tp."offset" DESC LIMIT 1)) = ?)`
	var found bool
	if err := e.tx.QueryRowContext(e.ctx, `SELECT EXISTS (SELECT 1 FROM retirements r WHERE `+inProject+`)`,
		e.q.Project, e.q.Project).Scan(&found); err != nil {
		return err
	}
	if !found {
		absent(ReasonNoEvents)
		return nil
	}
	rows, err := e.tx.QueryContext(e.ctx, `SELECT ts, kind FROM retirements r WHERE ts >= ? AND ts < ? AND `+inProject,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	byKind := map[string]int64{}
	byWeek := map[string]map[string]int64{}
	var total int64
	for rows.Next() {
		var ts int64
		var kind string
		if err := rows.Scan(&ts, &kind); err != nil {
			return err
		}
		wk := isoWeek(time.UnixMilli(ts).In(w.Loc))
		if byWeek[wk] == nil {
			byWeek[wk] = map[string]int64{}
		}
		byWeek[wk][kind]++
		byKind[kind]++
		total++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m := Metric{Value: ptr(float64(total)), Extra: map[string]any{}}
	for _, k := range churnKinds {
		m.Extra[k] = byKind[k]
	}
	// Every week the window touches, Monday to Monday; a week the window
	// starts or ends inside is partial.
	y, mo, d := w.Since.In(w.Loc).Date()
	start := time.Date(y, mo, d, 0, 0, 0, 0, w.Loc)
	start = time.Date(y, mo, d-(int(start.Weekday())+6)%7, 0, 0, 0, 0, w.Loc)
	for t := start; t.Before(w.Until); t = time.Date(t.Year(), t.Month(), t.Day()+7, 0, 0, 0, 0, w.Loc) {
		wk := isoWeek(t)
		next := time.Date(t.Year(), t.Month(), t.Day()+7, 0, 0, 0, 0, w.Loc)
		p := Point{Week: wk, Partial: t.Before(w.Since) || next.After(w.Until)}
		var sum int64
		for _, n := range byWeek[wk] {
			sum += n
		}
		p.Value = ptr(float64(sum))
		if sum > 0 {
			p.Parts = byWeek[wk]
		}
		m.Series = append(m.Series, p)
	}
	obs[id], met[id] = Observation{}, m
	return nil
}

// isoWeek is t's ISO week as 2026-W44.
func isoWeek(t time.Time) string {
	y, wk := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, wk)
}

// bankStored fills notes.stored: the store events of the window (this PC),
// saved and updated ones as the value, all of them by result and by kind.
func bankStored(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	const id = "notes.stored"
	reason, none, err := reachAbsent(e, "tel_store")
	if err != nil {
		return err
	}
	if none {
		obs[id], met[id] = Observation{Absent: true, Reason: reason}, Metric{}
		return nil
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT COALESCE(result, ''), COALESCE(kind, '') FROM tel_store
WHERE ts >= ? AND ts < ? AND `+projectClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	byResult := map[string]int64{"saved": 0, "updated": 0, "refused": 0}
	byKind := map[string]int64{}
	var n int64
	for rows.Next() {
		var result, kind string
		if err := rows.Scan(&result, &kind); err != nil {
			return err
		}
		if kind == "" {
			kind = unknownKey
		}
		byResult[result]++
		byKind[kind]++
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	obs[id] = Observation{Den: n}
	met[id] = Metric{Value: ptr(float64(byResult["saved"] + byResult["updated"])),
		Extra: map[string]any{"by_result": byResult, "by_kind": byKind}}
	return nil
}
