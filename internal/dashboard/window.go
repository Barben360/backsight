package dashboard

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// Custom is the custom window picker: a GET form at the end of the window
// links that sends from and to (both days included). The server turns them
// into since and until (until exclusive) and redirects; a refusal renders
// the page with its previous window and the picker open.
type Custom struct {
	Action      string      // the page's path, the form's action
	Keep        [][2]string // query pairs carried as hidden inputs
	From, To    string      // the fields' values
	Min, Max    string      // the first day of the month 12 months back; today
	Err         string      // the refusal's sentence; "" when none
	FromBad     bool        // the From field is the one refused
	ToBad       bool        // the To field is the one refused
	Label, Aria string      // the summary's text and its accessible name
	Current     bool        // a custom window is shown
	ErrID       string      // the error's id: werr, or werr-m in the Menu
	Pop         string      // the popover's classes
}

// telemetryMonths is how many months the telemetry rotation keeps: the
// current one and the 12 before.
const telemetryMonths = 13

// pickerDrop are the keys the picker's form does not carry: it sets the
// window itself and starts the list over.
var pickerDrop = []string{"days", "since", "until", "from", "to", "offset", "code"}

// inMenu is the picker's copy inside the Menu: the same form, its error
// id its own.
func (c Custom) inMenu() *Custom {
	c.ErrID, c.Pop = "werr-m", "menu-pop"
	return &c
}

// pickerBounds are the earliest and latest days the picker takes, in loc
// at now.
func pickerBounds(now time.Time, loc *time.Location) (lo, hi time.Time) {
	t := now.In(loc)
	hi = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	lo = time.Date(t.Year(), t.Month()-(telemetryMonths-1), 1, 0, 0, 0, 0, loc)
	return lo, hi
}

// pickerZone is the zone the picker reads its days in: the query's tz, or
// this machine's.
func pickerZone(q url.Values) *time.Location {
	if tz := q.Get("tz"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// keptPairs are q's pairs the picker carries, by key.
func keptPairs(q url.Values) [][2]string {
	keys := make([]string, 0, len(q))
	for k := range q {
		if !slices.Contains(pickerDrop, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var out [][2]string
	for _, k := range keys {
		for _, v := range q[k] {
			out = append(out, [2]string{k, v})
		}
	}
	return out
}

// newCustom is the picker of a page at path with query q, the window w
// shown, at now.
func newCustom(path string, q url.Values, p views.Params, w metrics.Window, now time.Time) Custom {
	base, _, _ := strings.Cut(path, "?")
	loc := zoneOr(w.Loc)
	if w.Loc == nil {
		loc = pickerZone(q)
	}
	lo, hi := pickerBounds(now, loc)
	c := Custom{Action: base, Keep: keptPairs(q), Min: lo.Format(time.DateOnly), Max: hi.Format(time.DateOnly),
		Label: "Custom", Aria: "Custom window: pick the days", ErrID: "werr", Pop: "menu-pop right wide"}
	if p.Since != "" && p.Until != "" && !w.Since.IsZero() && !w.Until.IsZero() {
		s, u := w.Since.In(loc), w.Until.In(loc)
		if midnight(s) && midnight(u) && u.After(s) {
			last := u.AddDate(0, 0, -1)
			c.Label = s.Format("Jan 2") + " – " + last.Format("Jan 2")
			c.Aria = c.Label + ", custom window, change it"
			c.Current = true
			c.From, c.To = s.Format(time.DateOnly), last.Format(time.DateOnly)
		}
	}
	return c
}

func midnight(t time.Time) bool {
	return t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0
}

// hasPicker says whether the page offers window links, and so the picker:
// not the text page, nor the pages about one day, session or memory.
func hasPicker(spec pageSpec, r *http.Request) bool {
	return kindOf[r.Pattern] != "text" && spec.Name != "day" && spec.Name != "session" && spec.Name != "memory"
}

// pickWindow reads the picker's from and to in q; both "" and nil when q
// has neither. Otherwise either redirect is the page's path with the kept
// keys, since and until, or refused holds the sentence, the fields refused
// and the values to keep. A key given twice is a usage error.
func pickWindow(path string, q url.Values, now time.Time) (redirect string, refused *Custom, err error) {
	_, hasFrom := q["from"]
	_, hasTo := q["to"]
	if !hasFrom && !hasTo {
		return "", nil, nil
	}
	for _, k := range []string{"from", "to"} {
		if len(q[k]) > 1 {
			return "", nil, views.Usage("give %s once", k)
		}
	}
	fromS, toS := q.Get("from"), q.Get("to")
	loc := pickerZone(q)
	lo, hi := pickerBounds(now, loc)
	refuse := func(msg string, fromBad, toBad bool) (string, *Custom, error) {
		return "", &Custom{From: fromS, To: toS, Err: msg, FromBad: fromBad, ToBad: toBad}, nil
	}
	from, errF := time.ParseInLocation(time.DateOnly, fromS, loc)
	to, errT := time.ParseInLocation(time.DateOnly, toS, loc)
	if errF != nil || errT != nil {
		return refuse("Pick both days.", errF != nil, errT != nil)
	}
	const f = "Jan 2"
	switch {
	case to.Before(from):
		return refuse("“To” ("+to.Format(f)+") is before “From” ("+from.Format(f)+"). Pick a “To” on or after "+from.Format(f)+".", false, true)
	case from.Before(lo):
		return refuse("This PC keeps 13 months: pick "+lo.Format("Jan 2, 2006")+" or later.", true, to.Before(lo))
	case to.After(hi):
		return refuse(to.Format(f)+" is in the future: pick today or earlier.", from.After(hi), true)
	}
	base, _, _ := strings.Cut(path, "?")
	v := url.Values{}
	for _, kv := range keptPairs(q) {
		v.Add(kv[0], kv[1])
	}
	qs := v.Encode()
	if qs != "" {
		qs += "&"
	}
	qs += "since=" + from.Format(time.DateOnly) + "&until=" + to.AddDate(0, 0, 1).Format(time.DateOnly)
	return base + "?" + qs, nil, nil
}
