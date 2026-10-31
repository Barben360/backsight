package metrics

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
)

// releaseNote is what the releases view says of its own numbers.
const releaseNote = "observational: weeks, projects and memory differ"

// ReleaseFlagship are the metrics judged per version, in the order the
// releases table shows them.
var ReleaseFlagship = []string{"recall.hit_rate", "used.turn_rate", "declare.coverage", "verdict.wrong_rate", "context.per_session", "recall.latency"}

// Releases is the developer view of the binary versions in the window: per
// version, attributed per event (each prompt by its own row's version; a
// journal row by its turn's), the span of its prompts, the typed prompts
// (n), the flagship metrics judged on that version's rows alone (absolute
// thresholds; no previous window), the share of its prompts per project,
// the prompts per active day (days with a typed prompt), and the bank's
// size at its last day when the server's series reaches it (90 days; null
// otherwise and under a project filter). Newest first version first (by
// first prompt, then version), paged. settings are the numeric fields of
// the [recall] and [feedback] tables.
func Releases(ctx context.Context, s Source, q Query) (Result, error) {
	var need []string
	if q.Project == "" {
		need = []string{"stats", "series"}
	}
	return runView(ctx, s, q, "releases", need, func(e env, r *Result) error {
		rows, err := releaseRows(e, e.q.Window)
		if err != nil {
			return err
		}
		slices.SortFunc(rows, func(a, b ReleaseRow) int {
			if c := b.First.Compare(a.First); c != 0 {
				return c
			}
			return strings.Compare(a.Version, b.Version)
		})
		lo, hi, page := pageBounds(e.q, len(rows))
		r.Tables.Releases, r.Tables.Page = rows[lo:hi], &page
		r.Tables.Settings = settingsOf(e.src.Config)
		r.Tables.Note = releaseNote
		return nil
	})
}

// releaseAcc counts one version's prompts.
type releaseAcc struct {
	first, last time.Time
	n           int64
	days        map[string]bool
	projects    map[string]int64
}

// NoProject is the project_shares key of prompts that have no project: the
// parentheses never appear in a project tag, so it cannot collide with a
// project that is named "none".
const NoProject = "(none)"

func releaseRows(e env, w Window) ([]ReleaseRow, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT binary_version, ts, COALESCE(project, ''), reason IS NULL OR reason <> 'machine' FROM tel_prompt
WHERE ts >= ? AND ts < ? AND COALESCE(binary_version, '') <> '' AND `+projectClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	accs := map[string]*releaseAcc{}
	for rows.Next() {
		var version, project string
		var ts int64
		var typed bool
		if err := rows.Scan(&version, &ts, &project, &typed); err != nil {
			return nil, err
		}
		a := accs[version]
		if a == nil {
			a = &releaseAcc{days: map[string]bool{}, projects: map[string]int64{}}
			accs[version] = a
		}
		at := time.UnixMilli(ts).In(w.Loc)
		if a.first.IsZero() || at.Before(a.first) {
			a.first = at
		}
		if at.After(a.last) {
			a.last = at
		}
		if typed {
			a.n++
			a.days[at.Format("2006-01-02")] = true
			a.projects[project]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ReleaseRow, 0, len(accs))
	for version, a := range accs {
		r := ReleaseRow{Version: version, First: a.first, Last: a.last, N: a.n, ProjectShares: map[string]float64{}}
		for p, n := range a.projects {
			if p == "" {
				p = NoProject
			}
			r.ProjectShares[p] = float64(n) / float64(a.n)
		}
		if len(a.days) > 0 {
			r.PromptsPerDay = float64(a.n) / float64(len(a.days))
		}
		if r.Flagship, err = flagship(e, w, version); err != nil {
			return nil, err
		}
		r.BankSize = bankSizeAt(e, a.last)
		out = append(out, r)
	}
	return out, nil
}

// flagship judges the flagship metrics on one version's rows: each family
// run once for the window in an environment of its own (no memo shared with
// the other versions), the metrics read as finish reads them with no
// previous window.
func flagship(e env, w Window, version string) (map[string]Metric, error) {
	ve := e
	ve.q.version = version
	ve.memo = newMemo()
	out := make(map[string]Metric, len(ReleaseFlagship))
	for _, id := range ReleaseFlagship {
		d, _ := DefOf(id)
		fam := families.byID[id]
		obs, met, err := ve.run(fam, w)
		if err != nil {
			return nil, err
		}
		out[id] = finish(d, obs[id], met[id], nil, ve.q)
	}
	return out, nil
}

// bankSizeAt is the bank's size at the end of day last: the server's total
// less what the series says was added after that day. Null when the total
// or the series was not read, last is more than 90 days old, or the view
// is filtered by project (the series covers every project).
func bankSizeAt(e env, last time.Time) *int64 {
	if e.q.Project != "" || e.srv == nil || serverReason(e, "stats") != "" || serverReason(e, "series") != "" ||
		e.now.Sub(last) > maxSeriesDays*day {
		return nil
	}
	var total int64
	for _, n := range e.srv.Stats.NodesByFactType {
		total += int64(n)
	}
	lastDay := last.In(viewLoc(e)).Format("2006-01-02")
	for _, b := range e.srv.Series.Buckets {
		if b.Time.In(viewLoc(e)).Format("2006-01-02") > lastDay {
			total -= b.World + b.Experience + b.Observation
		}
	}
	return ptr(max(0, total))
}

// settingsOf is every numeric field of the [recall] and [feedback] tables,
// keyed recall.<key> and feedback.<key> (their toml keys); a boolean reads 0
// or 1 and a string is left out.
func settingsOf(cfg config.Config) map[string]float64 {
	out := map[string]float64{}
	for _, t := range []struct {
		prefix string
		v      any
	}{{"recall.", cfg.Recall}, {"feedback.", cfg.Feedback}} {
		rv := reflect.ValueOf(t.v)
		for i := range rv.NumField() {
			key, _, _ := strings.Cut(rv.Type().Field(i).Tag.Get("toml"), ",")
			if key == "" || key == "-" {
				continue
			}
			f := rv.Field(i)
			switch f.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				out[t.prefix+key] = float64(f.Int())
			case reflect.Float32, reflect.Float64:
				out[t.prefix+key] = f.Float()
			case reflect.Bool:
				if f.Bool() {
					out[t.prefix+key] = 1
				} else {
					out[t.prefix+key] = 0
				}
			}
		}
	}
	return out
}
