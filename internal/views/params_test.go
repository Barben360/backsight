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
			`unknown parameter "has" for overview: one of days, since, until, tz, project, path, dev, limit, offset`},
		{"text is CLI only", "memories", "text=1", Params{}, `unknown parameter "text" for memories`},
		{"machines", "overview", "machines=all", Params{}, `unknown parameter "machines" for overview`},
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
