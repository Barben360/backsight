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
// "2026-10-24T15:00+01:00") of a window, clipped to the window. Partial:
// the clip cut it (the window starts or ends inside it), so its counts
// cover less than a whole day or hour.
type Bucket struct {
	Key          string
	Since, Until time.Time
	Partial      bool
}

const day = 24 * time.Hour

// hourlyMax is the longest window drawn by the hour.
const hourlyMax = 48 * time.Hour

// MaxWindowDays is the longest window a view takes, in calendar days. Every
// window the product offers fits: --days up to 365, the dashboard's picker
// (from the first day of the month 12 months back to today, at most 397
// days) and the 13-month calendar; the daily document shared between PCs
// has the same 400-day cap. A longer window would only cost memory (a
// bucket per day, for the window, its previous window and the baselines):
// its previous window already reaches past the 25 months the index keeps.
const MaxWindowDays = 400

// FirstYear and LastYear bound the days a window may start and end in,
// read in the window's zone: from FirstYear-01-01 00:00 to the end of
// LastYear. Outside them a date is a typo, and the window's arithmetic (its
// length and its previous window) could overflow.
const (
	FirstYear = 2000
	LastYear  = 2099
)

// InRange reports whether t, read in loc, lies within the years a window
// may cover: FirstYear-01-01 00:00 to (LastYear+1)-01-01 00:00 included.
func InRange(t time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.UTC
	}
	return !t.In(loc).Before(time.Date(FirstYear, 1, 1, 0, 0, 0, 0, loc)) &&
		!t.In(loc).After(time.Date(LastYear+1, 1, 1, 0, 0, 0, 0, loc))
}

// Errors of NewWindow a caller can tell apart.
var (
	ErrWindowEmpty      = errors.New("the end of the window is not after its start")
	ErrWindowTooLong    = errors.New("the window is longer than 400 days")
	ErrWindowOutOfRange = errors.New("the window starts before 2000 or ends after 2099")
)

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
	case days > MaxWindowDays:
		return Window{}, ErrWindowTooLong
	case days > 0:
		until = now
		since = now.Add(-time.Duration(days) * day)
	default:
		if until.IsZero() {
			until = now
		}
	}
	// The range first: it keeps the length and the calendar arithmetic
	// below far from time.Duration's 292 years.
	if !InRange(since, loc) || !InRange(until, loc) {
		return Window{}, ErrWindowOutOfRange
	}
	if !until.After(since) {
		return Window{}, ErrWindowEmpty
	}
	// Calendar days in loc, so that a window across a DST change is not
	// refused for its extra hour.
	if until.After(since.In(loc).AddDate(0, 0, MaxWindowDays)) {
		return Window{}, ErrWindowTooLong
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
// clipped to the window, so the buckets tile it exactly, and marked
// Partial when the clip cut them. Hours are counted from the local hour of
// Since: a 48 h window starting at 10:30 has 49 buckets, the first and the
// last half an hour each.
func (w Window) Buckets() []Bucket {
	loc := w.Loc
	if loc == nil {
		loc = time.UTC
	}
	if w.Length() > hourlyMax {
		return w.DayBuckets()
	}
	var out []Bucket
	s := w.Since.In(loc)
	h := s.Add(-time.Duration(s.Minute())*time.Minute - time.Duration(s.Second())*time.Second - time.Duration(s.Nanosecond()))
	for ; h.Before(w.Until); h = h.Add(time.Hour) {
		out = append(out, w.clipped(h.Format("2006-01-02T15:04-07:00"), h, h.Add(time.Hour)))
	}
	return out
}

// DayBuckets splits the window into local days whatever its length (the
// days view lists days even for a window drawn by the hour); the first and
// last are clipped and marked Partial as in Buckets.
func (w Window) DayBuckets() []Bucket {
	loc := w.Loc
	if loc == nil {
		loc = time.UTC
	}
	var out []Bucket
	y, m, d := w.Since.In(loc).Date()
	for t := time.Date(y, m, d, 0, 0, 0, 0, loc); t.Before(w.Until); {
		y, m, d = t.Date()
		next := time.Date(y, m, d+1, 0, 0, 0, 0, loc)
		out = append(out, w.clipped(t.Format("2006-01-02"), t, next))
		t = next
	}
	return out
}

// clipped is the bucket [from, to) cut to the window.
func (w Window) clipped(key string, from, to time.Time) Bucket {
	return Bucket{Key: key, Since: max2(from, w.Since), Until: min2(to, w.Until),
		Partial: from.Before(w.Since) || to.After(w.Until)}
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
