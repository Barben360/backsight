package views

import (
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
)

// codeOf is the exit code of an *output.Error, failing the test for any
// other error.
func codeOf(t *testing.T, err error) int {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("%v is not an *output.Error", err)
	}
	return oe.Code
}

func TestFromValues(t *testing.T) {
	for _, c := range []struct {
		name, view, query string
		want              Params
		err               string
	}{
		{"days", "overview", "days=30", Params{Days: 30, DaysSet: true, Limit: 50}, ""},
		{"paging and has", "sessions", "has=used&limit=2&offset=1", Params{Has: "used", Limit: 2, Offset: 1}, ""},
		{"memories options", "memories", "sort=rate&state=hidden", Params{Sort: "rate", State: "hidden", Limit: 50}, ""},
		{"dev true", "overview", "dev=true", Params{Dev: true, DevSet: true, Limit: 50}, ""},
		{"dev 0 is given", "overview", "dev=0", Params{DevSet: true, Limit: 50}, ""},
		{"a view's own key on another view", "overview", "has=used", Params{},
			`unknown parameter "has" for overview: one of days, since, until, tz, project, path, dev, limit, offset, machines`},
		{"text is CLI only", "memories", "text=1", Params{}, `unknown parameter "text" for memories`},
		{"machines", "overview", "machines=all", Params{Machines: "all", Limit: 50}, ""},
		{"code", "overview", "code=x", Params{}, `unknown parameter "code" for overview`},
		{"twice", "overview", "days=1&days=2", Params{}, "give days once"},
		{"days not a number", "overview", "days=abc", Params{}, "--days: a whole number from 1 to 365"},
		{"limit not a number", "overview", "limit=x", Params{}, "--limit: a whole number from 1 to 500"},
		{"offset not a number", "overview", "offset=x", Params{}, "--offset: a whole number, 0 or more"},
		{"dev yes", "overview", "dev=yes", Params{}, "--dev: 1, true, 0 or false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, err := url.ParseQuery(c.query)
			if err != nil {
				t.Fatal(err)
			}
			got, err := FromValues(c.view, v)
			if c.err != "" {
				if err == nil || !strings.HasPrefix(err.Error(), c.err) {
					t.Fatalf("error %v, want %q", err, c.err)
				}
				if code := codeOf(t, err); code != output.Usage {
					t.Errorf("code %d, want %d", code, output.Usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestQueryChecksTheDayFirst(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	_, err := Query("days", Params{Limit: 0}, "notaday", now)
	if err == nil || err.Error() != "a day is written YYYY-MM-DD" {
		t.Errorf("error %v, want the day's message before the limit's", err)
	}
	if !errors.Is(err, ErrMalformedID) || codeOf(t, err) != output.Usage {
		t.Errorf("error %v: want a malformed id, code %d", err, output.Usage)
	}
	if _, err := Query("overview", Params{Limit: 0}, "", now); errors.Is(err, ErrMalformedID) {
		t.Errorf("a bad limit is no malformed id: %v", err)
	}
	q, err := Query("days", Params{Limit: 50}, "2026-10-08", now)
	if err != nil || q.ID != "2026-10-08" {
		t.Errorf("query %+v, error %v; want ID 2026-10-08", q, err)
	}
	if _, err := Query("sessions", Params{Limit: 50}, "2026-13-01", now); err != nil {
		t.Errorf("a session id is not a day: %v", err)
	}
}

func TestErrorMapsNotFound(t *testing.T) {
	err := Error(metrics.NotFoundError{Kind: "session", ID: "sess-lon", Matches: []string{"sess-long-1", "sess-long-2"}})
	if want := `ambiguous session id "sess-lon": sess-long-1, sess-long-2`; err.Error() != want {
		t.Errorf("message %q, want %q", err, want)
	}
	if code := codeOf(t, err); code != output.Usage {
		t.Errorf("code %d, want %d", code, output.Usage)
	}
	err = Error(index.ErrUpdateInProgress)
	if err.Error() != UpdatingSentence {
		t.Errorf("message %q, want %q", err, UpdatingSentence)
	}
	if code := codeOf(t, err); code != output.Blocked {
		t.Errorf("code %d, want %d", code, output.Blocked)
	}
}

func TestUnknownViewLists(t *testing.T) {
	if got := UnknownView("x", true).Error(); !strings.HasSuffix(got, "days, defs, index") {
		t.Errorf("with index: %q", got)
	}
	if got := UnknownView("x", false).Error(); !strings.HasSuffix(got, "days, defs") {
		t.Errorf("without index: %q", got)
	}
}

func TestTakesID(t *testing.T) {
	for _, v := range metrics.Views {
		want := v == "projects" || v == "sessions" || v == "memories" || v == "days" || v == "defs"
		if TakesID(v) != want {
			t.Errorf("TakesID(%q) = %v, want %v", v, !want, want)
		}
	}
}

// machines=all is a parameter of every view; anything but this or all, and a
// repeated key, is a usage error.
func TestMachinesParam(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, view := range metrics.Views {
		v, err := url.ParseQuery("machines=all")
		if err != nil {
			t.Fatal(err)
		}
		p, err := FromValues(view, v)
		if err != nil {
			t.Fatalf("%s: machines=all: %v", view, err)
		}
		q, err := Query(view, p, "", now)
		if err != nil || q.Machines != "all" {
			t.Errorf("%s: machines=all gave %q, %v", view, q.Machines, err)
		}
	}
	for _, c := range []struct{ query, want string }{{"", "this"}, {"machines=this", "this"}, {"machines=", "this"}} {
		v, _ := url.ParseQuery(c.query)
		p, err := FromValues("overview", v)
		if err != nil {
			t.Fatal(err)
		}
		if q, err := Query("overview", p, "", now); err != nil || q.Machines != c.want {
			t.Errorf("%q: machines %q, %v, want %q", c.query, q.Machines, err, c.want)
		}
	}
	v, _ := url.ParseQuery("machines=both")
	p, err := FromValues("overview", v)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Query("overview", p, "", now)
	if err == nil || err.Error() != "--machines: this or all" || codeOf(t, err) != output.Usage {
		t.Errorf("machines=both: %v", err)
	}
	if _, err := FromValues("overview", url.Values{"machines": {"this", "all"}}); err == nil || err.Error() != "give machines once" {
		t.Errorf("machines twice: %v", err)
	}
}

// A window is at most 400 calendar days and lies within 2000-2099: a longer
// or wilder one is a usage error with its own sentence, before any bucket
// is built. Exactly 400 days is taken, across a DST change too.
func TestQueryBoundsTheWindow(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	const (
		tooLong = "--since/--until: a window of at most 400 days"
		outside = "--since and --until: dates from 2000-01-01 to 2099-12-31"
	)
	for _, c := range []struct {
		name, since, until, tz, err string
	}{
		{"a century", "2000-01-01", "2099-12-31", "UTC", tooLong},
		{"until in 9999", "2026-01-03", "9999-12-31", "UTC", outside},
		{"since in year 1", "0001-01-01", "", "UTC", outside},
		{"since in 1900", "1900-01-01", "", "UTC", outside},
		{"a week in 9999", "9999-12-01", "9999-12-08", "UTC", outside},
		{"until a second past 2099", "2099-12-01", "2100-01-01T00:00:01Z", "UTC", outside},
		{"one day over", "2025-09-01", "2026-10-07", "UTC", tooLong},
		{"one day over in Paris", "2025-09-01", "2026-10-07", "Europe/Paris", tooLong},
		{"at the cap", "2025-09-01", "2026-10-06", "UTC", ""},
		{"at the cap across DST", "2025-10-01", "2026-11-05", "Europe/Paris", ""},
		{"one day over across DST", "2025-10-01", "2026-11-06", "Europe/Paris", tooLong},
		{"first day", "2000-01-01", "2000-01-02", "UTC", ""},
		{"last day", "2099-12-31", "2100-01-01", "UTC", ""},
		{"since alone, within 400 days of now", "2025-09-26", "", "UTC", ""},
		{"since alone, over 400 days before now", "2025-09-25", "", "UTC", tooLong},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, err := Query("overview", Params{Since: c.since, Until: c.until, TZ: c.tz, Limit: 50}, "", now)
			if c.err != "" {
				if err == nil || err.Error() != c.err {
					t.Fatalf("error %v, want %q", err, c.err)
				}
				if code := codeOf(t, err); code != output.Usage {
					t.Errorf("code %d, want %d", code, output.Usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if q.Window.Days < 1 || q.Window.Days > 401 {
				t.Errorf("window days %d", q.Window.Days)
			}
		})
	}
}
