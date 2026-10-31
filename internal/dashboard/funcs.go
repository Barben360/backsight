package dashboard

import (
	"errors"
	"html/template"
	"net/url"
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// MetricView is one metric of a result with its catalog entry, as the
// metric partial shows it. Found: the result holds the metric.
type MetricView struct {
	ID    string
	Def   metrics.Def
	M     metrics.Metric
	Found bool
}

// dash stands for a value that is not there.
const dash = "—"

// funcs are the template functions. Their results are strings unless said;
// none changes its arguments, which belong to the shared caches.
var funcs = template.FuncMap{
	"metric":    metricOf,
	"value":     func(v MetricView) string { return foundOr(v, dash, views.FormatValue) },
	"status":    func(v MetricView) string { return foundOr(v, "no data", views.StatusText) },
	"delta":     func(v MetricView) string { return foundOr(v, "", views.DeltaText) },
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
	"pathOnly":  func(p string) string { base, _ := splitPath(p); return base },
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
