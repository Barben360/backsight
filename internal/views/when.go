package views

import "time"

// WhenLayout is how a moment reads in a table or a sentence.
const WhenLayout = "2006-01-02 15:04"

// When is t in loc as WhenLayout, a nil loc being UTC; "" for a zero t, for
// each caller to give its own word for none.
func When(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format(WhenLayout)
}
