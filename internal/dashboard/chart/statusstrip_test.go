package chart

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var d0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func statusDays(n int, f func(i int) string) []StatusDay {
	out := make([]StatusDay, n)
	for i := range out {
		out[i] = StatusDay{Day: d0.AddDate(0, 0, i), Status: f(i)}
	}
	return out
}

func baseStatus(rows ...StatusRow) StatusStrip {
	return StatusStrip{Label: "Health check results per daily run, one row per check", Size: Size{1000, 300, "vb1000"},
		Rows: rows, More: "backsight stats system --days 30"}
}

var statusMarkRE = regexp.MustCompile(`<path class="([^"]*)" d="([^"]*)"([^>]*)><title>([^<]*)</title></path>`)

func TestStatusStripShapes(t *testing.T) {
	statuses := []string{"ok", "watch", "bad", "none"}
	c := baseStatus(StatusRow{Name: "Backups", Days: statusDays(4, func(i int) string { return statuses[i] })})
	out := string(c.HTML())
	got := statusMarkRE.FindAllStringSubmatch(out, -1)
	if len(got) != 4 {
		t.Fatalf("marks = %d, want 4:\n%s", len(got), out)
	}
	for i, want := range []struct{ class, d, title string }{
		{"mkp f-st-ok", " A 4.5 4.5 ", "Oct 1 · Backups · ok"},
		{"mkp f-st-watch", "L ", "Oct 2 · Backups · watch"},
		{"mkp f-st-bad", "H ", "Oct 3 · Backups · attention"},
		{"norun", " A 4 4 ", "Oct 4 · Backups · no upkeep run"},
	} {
		m := got[i]
		if m[1] != want.class || m[4] != want.title || !strings.Contains(m[2], want.d) {
			t.Errorf("mark %d = class %q d %q title %q, want %+v", i, m[1], m[2], m[4], want)
		}
		if strings.Contains(m[2], "V") != (i == 2) {
			t.Errorf("mark %d: only the octagon has vertical sides: %s", i, m[2])
		}
	}
	// the triangle has 3 corners, the octagon 8.
	if n := strings.Count(got[1][2], "L "); n != 2 {
		t.Errorf("triangle lines = %d: %s", n, got[1][2])
	}
	if n := strings.Count(got[2][2], "L ") + strings.Count(got[2][2], "H ") + strings.Count(got[2][2], "V "); n != 7 {
		t.Errorf("octagon sides = %d: %s", n, got[2][2])
	}
	must(t, out, `<text class="ta-e lbl" `, `>Backups</text>`, `class="legend"`, `ok</span>`, `attention</span>`, `no upkeep run that day</span>`,
		`<details class="tblfb">`)
	// marks are tab stops while they are few.
	must(t, out, `tabindex="0" aria-label="Oct 2 · Backups · watch"`)
	if strings.Contains(out, "<script") || strings.Contains(out, "style=") {
		t.Error("script or style in the strip")
	}
	// the later day sits to the right.
	x := regexp.MustCompile(`^M ([0-9.]+) `)
	a, b := x.FindStringSubmatch(got[0][2]), x.FindStringSubmatch(got[1][2])
	if a == nil || b == nil {
		t.Fatalf("no x in %v %v", got[0][2], got[1][2])
	}
	xa, _ := strconv.ParseFloat(a[1], 64)
	xb, _ := strconv.ParseFloat(b[1], 64)
	if xb <= xa {
		t.Errorf("days are not left to right: %v then %v", xa, xb)
	}
}

func TestStatusStripTable(t *testing.T) {
	c := baseStatus(
		StatusRow{Name: "Backups", Days: statusDays(10, func(i int) string {
			switch {
			case i == 3:
				return "none"
			case i >= 7:
				return "watch"
			}
			return "ok"
		})},
		StatusRow{Name: "Update", Days: statusDays(10, func(int) string { return "ok" })})
	out := string(c.HTML())
	// ok 6, watch 3, bad 0, no run 1; last change Oct 8: ok → watch.
	must(t, out, `<td>Backups</td><td>watch</td><td>6</td><td>3</td><td>0</td><td>1</td><td>Oct 8: ok → watch</td>`,
		`<td>Update</td><td>ok</td><td>10</td><td>0</td><td>0</td><td>0</td><td>—</td>`)
}

func TestStatusStripCapsMarks(t *testing.T) {
	var rows []StatusRow
	for i := 0; i < 9; i++ {
		name := fmt.Sprintf("Check number %d with a long name", i)
		rows = append(rows, StatusRow{Name: name, Days: statusDays(90, func(d int) string {
			if i == 0 && d == 41 {
				return "watch"
			}
			if i == 1 && d >= 60 {
				return "none"
			}
			return "ok"
		})})
	}
	out := string(baseStatus(rows...).HTML())
	if n := strings.Count(out, "<title>"); n > MaxMarks {
		t.Fatalf("titles = %d, over %d", n, MaxMarks)
	}
	got := statusMarkRE.FindAllStringSubmatch(out, -1)
	if len(got) < 9*30 || len(got) >= 9*90 {
		t.Fatalf("marks = %d: not bucketed into a fair number", len(got))
	}
	if strings.Contains(out, `tabindex="0" aria-label`) {
		t.Error("marks are tab stops above the focus limit")
	}
	if !strings.Contains(out, `role="img"`) || !strings.Contains(out, `viewBox="0 0 1000 `) || !strings.Contains(out, `" tabindex="0">`) {
		t.Error("above the limit the svg alone is the tab stop")
	}
	// the bucket holding the watch day reads watch, with its run count.
	re := regexp.MustCompile(`<title>([A-Za-z]+ \d+–[A-Za-z]+ \d+) · Check number 0 with a long name · watch, (\d+) runs</title>`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no watch bucket for check 0:\n%s", out)
	}
	if strings.Count(out, "Check number 0 with a long name · watch") != 1 {
		t.Error("only one bucket holds the watch day")
	}
	// a bucket of ok days reads ok; days without a run are not runs.
	must(t, out, "· Check number 0 with a long name · ok, 2 runs</title>")
	if !regexp.MustCompile(`· Check number 1 with a long name · none`).MatchString(out) &&
		!regexp.MustCompile(`· Check number 1 with a long name · no upkeep run`).MatchString(out) {
		t.Error("a bucket of days without a run is not drawn as no run")
	}
}

func TestStatusStripEdges(t *testing.T) {
	if out := string(StatusStrip{Label: "x"}.HTML()); !strings.Contains(out, `class="empty"`) {
		t.Errorf("no rows: %s", out)
	}
	one := baseStatus(StatusRow{Name: "A", Days: statusDays(1, func(int) string { return "ok" })})
	if got := statusMarkRE.FindAllStringSubmatch(string(one.HTML()), -1); len(got) != 1 {
		t.Errorf("a single day: %d marks", len(got))
	}
	// an unknown status reads as no run.
	odd := baseStatus(StatusRow{Name: "A", Days: statusDays(2, func(int) string { return "weird" })})
	must(t, string(odd.HTML()), "Oct 1 · A · no upkeep run")
}

// markupStatus is a status strip whose every text is markup.
func markupStatus() string {
	c := baseStatus(StatusRow{Name: markup, Days: statusDays(2, func(int) string { return "ok" })})
	for i := 0; i < 130; i++ { // more checks than a table shows
		c.Rows = append(c.Rows, StatusRow{Name: markup, Days: statusDays(1, func(int) string { return "ok" })})
	}
	c.Label, c.More = markup, markup
	return string(c.HTML())
}
