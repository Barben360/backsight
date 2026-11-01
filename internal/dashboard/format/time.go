package format

import (
	"strconv"
	"time"
)

// Time is a moment written for a page: Text for the eye, Datetime for the
// time element, Title for its hover.
type Time struct{ Text, Datetime, Title string }

func zone(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// dayNumber counts calendar days of t's own zone, so a DST change does not
// move a day boundary.
func dayNumber(t time.Time) int {
	y, m, d := t.Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// Rel writes at relative to now by the calendar of loc (nil: UTC).
func Rel(now, at time.Time, loc *time.Location) Time {
	loc = zone(loc)
	now, at = now.In(loc), at.In(loc)
	t := Time{Datetime: at.Format(time.RFC3339), Title: at.Format("2006-01-02 15:04 MST")}
	ago := now.Sub(at)
	days := dayNumber(now) - dayNumber(at)
	switch {
	case ago < time.Minute:
		t.Text = "just now"
	case ago < time.Hour:
		t.Text = strconv.Itoa(int(ago/time.Minute)) + " min ago"
	case days <= 0:
		t.Text = strconv.Itoa(int(ago/time.Hour)) + " h ago"
	case days == 1:
		t.Text = "yesterday"
	case days < 7:
		t.Text = strconv.Itoa(days) + " d ago"
	case at.Year() == now.Year():
		t.Text = at.Format("Jan 2")
	default:
		t.Text = at.Format("2006-01-02")
	}
	return t
}

// Clock writes at as "09:12" on now's calendar day in loc, else "Oct 29 09:12".
func Clock(now, at time.Time, loc *time.Location) string {
	loc = zone(loc)
	now, at = now.In(loc), at.In(loc)
	if dayNumber(now) == dayNumber(at) {
		return at.Format("15:04")
	}
	return at.Format("Jan 2 15:04")
}
