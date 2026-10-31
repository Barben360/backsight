package metrics

import (
	"errors"
	"time"
)

// Window is the half-open interval [Since, Until) a view is computed over,
// read in Loc.
type Window struct {
	Since time.Time      `json:"since"`
	Until time.Time      `json:"until"`
	Days  int            `json:"days"` // ceil((until-since)/24h)
	TZ    string         `json:"tz"`
	Loc   *time.Location `json:"-"`
}

// Span is a bare interval (Result.Previous).
type Span struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// Bucket is one day (key "2026-10-24") or one hour (key
// "2026-10-24T15:00+01:00") of a window, clipped to the window.
type Bucket struct {
	Key          string
	Since, Until time.Time
}

const day = 24 * time.Hour

// hourlyMax is the longest window drawn by the hour.
const hourlyMax = 48 * time.Hour

// NewWindow builds a window ending at until (now when zero) that either
// spans the last days*24 h (days > 0) or starts at since; exactly one of
// the two must be given. Elapsed time, not calendar days: across a DST
// change a 7-day window is still 168 h. A nil loc is UTC.
func NewWindow(now time.Time, days int, since, until time.Time, loc *time.Location, tz string) (Window, error) {
	if loc == nil {
		loc = time.UTC
	}
	if tz == "" {
		tz = loc.String()
	}
	switch {
	case days > 0 && !since.IsZero():
		return Window{}, errors.New("give either a number of days or a start, not both")
	case days <= 0 && since.IsZero():
		return Window{}, errors.New("a window needs a number of days or a start")
	case days > 0:
		until = now
		since = now.Add(-time.Duration(days) * day)
	default:
		if until.IsZero() {
			until = now
		}
		if !until.After(since) {
			return Window{}, errors.New("the end of the window is not after its start")
		}
	}
	w := Window{Since: since.In(loc), Until: until.In(loc), TZ: tz, Loc: loc}
	w.Days = ceilDays(w.Length())
	return w, nil
}

func ceilDays(d time.Duration) int {
	return int((d + day - 1) / day)
}

// Length is Until - Since.
func (w Window) Length() time.Duration { return w.Until.Sub(w.Since) }

func (w Window) span(since, until time.Time) Window {
	o := Window{Since: since, Until: until, TZ: w.TZ, Loc: w.Loc}
	o.Days = ceilDays(o.Length())
	return o
}

// Previous is the window of the same length ending at Since.
func (w Window) Previous() Window {
	return w.span(w.Since.Add(-w.Length()), w.Since)
}

// Baselines are the four 7-day windows before Since, newest first:
// [Since - 7k·24h, Since - 7(k-1)·24h) for k = 1..4, whatever the window's
// own length.
func (w Window) Baselines() []Window {
	out := make([]Window, 0, 4)
	for k := 1; k <= 4; k++ {
		out = append(out, w.span(
			w.Since.Add(-time.Duration(7*k)*day),
			w.Since.Add(-time.Duration(7*(k-1))*day)))
	}
	return out
}

// Last is the final d of the window: [Until - d, Until).
func (w Window) Last(d time.Duration) Window {
	return w.span(w.Until.Add(-d), w.Until)
}

// Buckets splits the window into local days (a DST day lasts 23 or 25 h),
// or into hours when it is at most 48 h long. The first and last bucket are
// clipped to the window, so the buckets tile it exactly. Hours are counted
// from the local hour of Since.
func (w Window) Buckets() []Bucket {
	loc := w.Loc
	if loc == nil {
		loc = time.UTC
	}
	var out []Bucket
	add := func(key string, from, to time.Time) {
		out = append(out, Bucket{Key: key, Since: max2(from, w.Since), Until: min2(to, w.Until)})
	}
	if w.Length() <= hourlyMax {
		s := w.Since.In(loc)
		h := s.Add(-time.Duration(s.Minute())*time.Minute - time.Duration(s.Second())*time.Second - time.Duration(s.Nanosecond()))
		for ; h.Before(w.Until); h = h.Add(time.Hour) {
			add(h.Format("2006-01-02T15:04-07:00"), h, h.Add(time.Hour))
		}
		return out
	}
	y, m, d := w.Since.In(loc).Date()
	for t := time.Date(y, m, d, 0, 0, 0, 0, loc); t.Before(w.Until); {
		y, m, d = t.Date()
		next := time.Date(y, m, d+1, 0, 0, 0, 0, loc)
		add(t.Format("2006-01-02"), t, next)
		t = next
	}
	return out
}

func max2(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func min2(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
