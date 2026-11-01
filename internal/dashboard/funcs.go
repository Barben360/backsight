package dashboard

import (
	"errors"
	"html/template"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// MetricView is one metric of a result with its catalog entry, as the
// metric partial shows it. Found: the result holds the metric; ShowID: the
// reader asked for developer detail, so the partial names the metric.
type MetricView struct {
	ID     string
	Def    metrics.Def
	M      metrics.Metric
	Found  bool
	ShowID bool           // the reader asked for developer detail
	Loc    *time.Location // the zone the metric's times are shown in
}

// Tabled is a result's tables with the zone their times are shown in, the
// dot of the shared table partials.
type Tabled struct {
	T   metrics.Tables
	Loc *time.Location
}

// dash stands for a value that is not there.
const dash = "—"

// funcs are the template functions. Their results are strings unless said;
// none changes its arguments, which belong to the shared caches.
var funcs = template.FuncMap{
	"metric":   metricOf,
	"value":    valueOf,
	"status":   func(v MetricView) string { return foundOr(v, "no data", views.StatusText) },
	"delta":    func(v MetricView) string { return foundOr(v, "", views.DeltaText) },
	"interval": func(v MetricView) string { return foundOr(v, "", views.IntervalText) },
	"framed":   func(v MetricView, dev bool, loc *time.Location) MetricView { v.ShowID, v.Loc = dev, loc; return v },
	"tabled": func(r *metrics.Result, loc *time.Location) Tabled {
		if r == nil {
			return Tabled{Loc: loc}
		}
		return Tabled{T: r.Tables, Loc: loc}
	},
	"since":     since,
	"details":   detailsOf,
	"extras":    func(v MetricView) []string { return foundLines(v, views.ExtraLines) },
	"job":       func(s string) string { return views.Word(views.JobWords, s) },
	"jobstatus": func(s string) string { return views.Word(views.JobStatusWords, s) },
	"dstatus":   func(s string) string { return views.Word(views.DoctorStatusWords, s) },
	"operation": func(s string) string { return views.Word(views.OperationWords, s) },
	"sysrows":   systemRows,
	"axes":      func() []struct{ Key, Name string } { return views.AxisNames },
	"n":         views.Grouped,
	"doctor":    metrics.DoctorName,
	"money":     money,
	"currency":  func(v MetricView) string { c, _ := v.M.Extra["currency"].(string); return c },
	"axis":      func(s metrics.AxisState) string { return views.Word(wordsOfAxis, string(s)) },
	"nodata":    func(s string) string { return views.Word(views.NoDataWords, s) },
	"servererr": func(s string) string { return views.Word(views.ServerErrorWords, s) },
	"outcome":   func(s string) string { return views.Word(views.OutcomeWords, s) },
	"path":      func(s string) string { return views.Word(views.PathWords, s) },
	"sent":      func(s string) string { return views.Word(views.SentWords, s) },
	"verdict":   func(s string) string { return views.Word(views.VerdictWords, s) },
	"check":     func(s string) string { return views.Word(views.CheckWords, s) },
	"memstate":  func(s string) string { return views.Word(views.MemoryStateWords, s) },
	"kind":      func(s string) string { return views.Word(views.KindWords, s) },
	"empty":     views.EmptyWords.Replace,
	"unit":      func(s string) string { return views.Word(views.UnitWords, s) },
	"short":     views.ShortID,
	"clip":      views.Clip,
	"plain":     func(s string) string { return views.Plain(s, "") },
	"age":       views.Age,
	"when":      when,
	"esc":       url.PathEscape,
	"asset":     assetOf,
	"sentence":  sentence,
	"pathOnly":  func(p string) string { base, _ := splitPath(p); return base },

	"decision":    func(s string) string { return views.Word(views.DecisionWords, s) },
	"declared":    func(raw []string) string { return words(views.DeclaredWords, raw) },
	"retired":     func(s string) string { return views.Word(views.RetireWords, s) },
	"failkind":    func(s string) string { return wordOr(views.FailureKindWords, s, views.FailureKindWords["unknown"]) },
	"serverstate": func(s string) string { return views.Word(views.ServerStateWords, s) },
	"skipped":     func(s string) string { return views.Word(views.SkipWords, s) },
	"pager":       pagerOf,
	"links":       filterLinks,
	"rowmetric":   rowMetric,
	"flagships":   flagships,
	"shares":      shares,
	"settings":    settingLines,
	"opt":         optInt,
	"optpct":      optPercent,
	"weighted":    weighted,
	"signed":      signed,
	"count":       roundInt,
	"ranks":       rankLines,

	"banner":      bannerOf,
	"summaryline": summaryPage,
	"attention":   attentionPage,
	"tiles":       tilesOf,
	"dash":        func() string { return dash },
	"glance":      glancePage,
	"quality":     qualityOf,
	"cost":        costOf,
	"bankpage":    bankOf,
	"pipepage":    pipelineOf,
	"systempage":  systemOf,
	"latelyhead":  latelyHead,

	"head":     headOf,
	"fixed":    fixedHead,
	"withinfo": withInfo,
	"level":    level,
	"fig":      figOf,
	"drill":    drillHref,
	"clifor":   cliFor,
	"statusword": func(d metrics.Def, m metrics.Metric, learning bool) Word {
		w, c := statusWord(d, m, learning)
		return Word{w, c}
	},
	"nodatum": func(v MetricView, f Frame, l format.Locale, loc *time.Location) NoData {
		n, _ := noDataOf(v, f, l, loc) // a metric with a number has no sentence: Text is empty
		return n
	},

	"anyCurrent": func(cs []Choice) bool {
		return slices.ContainsFunc(cs, func(c Choice) bool { return c.Current })
	},
	"iso": func(loc *time.Location, t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(zoneOr(loc)).Format(time.RFC3339)
	},
	// The locale is an argument, never a method: {{num .L 1234}}.
	"num": func(l format.Locale, n int64) string { return l.Int(n) },
	"rel": func(now time.Time, at any, loc *time.Location) format.Time {
		t, ok := instant(at)
		if !ok {
			return format.Time{Text: dash}
		}
		return format.Rel(now, t, loc)
	},
	"clock": func(now time.Time, at any, loc *time.Location) string {
		t, ok := instant(at)
		if !ok {
			return dash
		}
		return format.Clock(now, t, loc)
	},
}

// instant is at (a time.Time or a *time.Time) as a time; ok is false for
// nil, zero or anything else.
func instant(at any) (time.Time, bool) {
	var t time.Time
	switch x := at.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x != nil {
			t = *x
		}
	}
	return t, !t.IsZero()
}

// assetOf is the path an embedded file is served at; a name that is not
// one fails the page, so a typo shows in tests.
func assetOf(name string) (string, error) {
	p, ok := assetPaths[name]
	if !ok {
		return "", errors.New("no asset " + strconv.Quote(name))
	}
	return p, nil
}

// wordsOfAxis is views.AxisStateWords keyed by plain strings.
var wordsOfAxis = func() map[string]string {
	m := map[string]string{}
	for k, w := range views.AxisStateWords {
		m[string(k)] = w
	}
	return m
}()

// metricOf is metric id of r with its catalog entry. An id the catalog
// does not hold is a template's mistake; a metric r lacks is not Found.
func metricOf(r *metrics.Result, id string) (MetricView, error) {
	d, ok := metrics.DefOf(id)
	if !ok {
		return MetricView{}, errors.New("no metric " + strconv.Quote(id) + " in the catalog")
	}
	v := MetricView{ID: id, Def: d}
	if r != nil {
		v.M, v.Found = r.Metrics[id]
	}
	return v, nil
}

// foundOr is f of v's definition and metric, or none when r lacks it.
func foundOr(v MetricView, none string, f func(metrics.Def, metrics.Metric) string) string {
	if !v.Found {
		return none
	}
	return f(v.Def, v.M)
}

// when is t (a time.Time or a *time.Time) in loc, a nil loc being UTC;
// "—" for nil, zero or anything else.
func when(loc *time.Location, t any) string {
	var at time.Time
	switch x := t.(type) {
	case time.Time:
		at = x
	case *time.Time:
		if x != nil {
			at = *x
		}
	}
	if at.IsZero() {
		return dash
	}
	if loc == nil {
		loc = time.UTC
	}
	return at.In(loc).Format("2006-01-02 15:04 MST")
}

// systemRows are the lines of the system block, times in loc.
func systemRows(s *metrics.SystemInfo, loc *time.Location) []views.SystemRow {
	if s == nil {
		return nil
	}
	plain := *s // a copy: the cached result keeps its raw words
	plain.Runtime, plain.Mode = views.Word(views.RuntimeWords, s.Runtime), views.Word(views.ModeWords, s.Mode)
	return views.SystemRows(plain, func(t time.Time) string { return when(loc, t) })
}

// money is a price in the currency; "price not set" for none.
func money(v *float64, currency string) string {
	if v == nil {
		return "price not set"
	}
	return views.ValueIn("money", *v, metrics.Metric{Extra: map[string]any{"currency": currency}})
}

// valueOf is the metric's value in its unit; the last run of the jobs is
// shown as how long ago it was.
func valueOf(v MetricView) string {
	if v.ID == "jobs.last_run" && v.Found && v.M.Value != nil {
		return views.Age(int64(*v.M.Value)) + " ago"
	}
	return foundOr(v, dash, views.FormatValue)
}

// foundLines is f of the metric's definition and metric; nil when the
// result lacks it or it has no data.
func foundLines(v MetricView, f func(metrics.Def, metrics.Metric) []string) []string {
	if !v.Found || v.M.Status == metrics.StatusNoData {
		return nil
	}
	return f(v.Def, v.M)
}

// detailsOf are the lines under a metric that its number does not say.
func detailsOf(v MetricView) []string {
	return foundLines(v, func(d metrics.Def, m metrics.Metric) []string {
		loc := v.Loc
		if loc == nil {
			loc = time.UTC
		}
		return views.PageDetails(d, m, loc)
	})
}

// since is how long before now at was, in words; "" for no time.
func since(now time.Time, at *time.Time) string {
	if at == nil || at.IsZero() {
		return ""
	}
	return views.Age(max(0, now.Sub(*at).Milliseconds()))
}
