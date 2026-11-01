package format

import (
	"math"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // Europe/Paris whatever the host has

	"github.com/Barben360/backsight/internal/metrics"
)

var (
	fr   = FromAcceptLanguage("fr")
	de   = FromAcceptLanguage("de")
	deCH = FromAcceptLanguage("de-CH")
)

func eq(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func fp(v float64) *float64 { return &v }

func TestNumbers(t *testing.T) {
	eq(t, English.Int(1234567), "1,234,567")
	eq(t, English.Int(999), "999")
	eq(t, English.Int(-1234), "-1,234")
	eq(t, fr.Int(1234567), "1 234 567")
	eq(t, de.Int(1234567), "1.234.567")
	eq(t, de.Fixed(1.6, 1), "1,6")
	eq(t, deCH.Int(1234567), "1’234’567")
	eq(t, English.Fixed(2.25, 1), "2.3")
	eq(t, English.Fixed(-2.25, 1), "-2.3")
	eq(t, English.Fixed(0.04, 0), "0")
	eq(t, English.Fixed(-0.04, 0), "0")
	eq(t, Locale{}.Int(1234), "1,234")
	eq(t, Locale{}.Fixed(1234.5, 1), "1,234.5")
}

func TestPercentAndInterval(t *testing.T) {
	eq(t, English.Percent(0.2754), "28 %")
	eq(t, English.Percent(0.004), "0.4 %")
	eq(t, fr.Percent(0.004), "0,4 %")
	eq(t, English.Percent(1), "100 %")
	eq(t, English.Percent(0), "0 %")
	eq(t, English.Interval([2]float64{0.21, 0.35}), "(21–35 %)")
	// tiny positive percent
	eq(t, English.Percent(0.00001), "< 0.1 %")
	eq(t, English.Percent(0.00049), "< 0.1 %")
	eq(t, English.Percent(0.0005), "0.1 %")
	eq(t, English.Percent(0.0099), "1.0 %")
}

func TestSizesAndDurations(t *testing.T) {
	eq(t, English.Size(3600, "chars"), "3,600 chars")
	eq(t, English.Size(17000, "tokens"), "17 k tokens")
	eq(t, English.Size(17000, ""), "17 k")
	eq(t, English.Size(12400, "chars"), "12.4 k chars")
	eq(t, fr.Size(12400, "chars"), "12,4 k chars")
	eq(t, English.Size(186400, "tokens"), "186 k tokens")
	eq(t, English.Size(9999, "chars"), "9,999 chars")
	eq(t, English.Size(99999, ""), "100 k")
	eq(t, English.Duration(840), "840 ms")
	eq(t, English.Duration(1600), "1.6 s")
	eq(t, English.Duration(999.6), "1.0 s")
	eq(t, English.Duration(910), "910 ms")
	// size boundary
	eq(t, English.Size(10000, ""), "10 k")
	eq(t, English.Size(9999.4, ""), "9,999")
	eq(t, English.Size(9999.5, ""), "10 k")
}

func TestMoney(t *testing.T) {
	eq(t, English.Money(43.62, "USD"), "$43.62")
	eq(t, English.Money(1234.5, "USD"), "$1,234.50")
	eq(t, English.Money(43.62, "GBP"), "£43.62")
	eq(t, English.Money(43.62, "EUR"), "43.62 €")
	eq(t, fr.Money(43.62, "EUR"), "43,62 €")
	eq(t, English.Money(43.62, "CHF"), "43.62 CHF")
	eq(t, English.Money(43.62, ""), "43.62")
}

func def(t *testing.T, id string) metrics.Def {
	t.Helper()
	d, ok := metrics.DefOf(id)
	if !ok {
		t.Fatalf("no metric %s", id)
	}
	return d
}

func TestValueParts(t *testing.T) {
	hit := metrics.Metric{Value: fp(0.2754), CI: &[2]float64{0.21, 0.35}}
	eq(t, English.Value(def(t, "recall.hit_rate"), hit), "28 % (21–35 %)")
	if got, want := English.ValueParts(def(t, "recall.hit_rate"), hit), (Parts{"28", "%", "(21–35 %)"}); got != want {
		t.Errorf("hit parts %+v, want %+v", got, want)
	}
	cases := []struct {
		id   string
		m    metrics.Metric
		want Parts
	}{
		{"recall.latency", metrics.Metric{Value: fp(1600)}, Parts{"1.6", "s", ""}},
		{"recall.latency", metrics.Metric{Value: fp(910)}, Parts{"910", "ms", ""}},
		{"context.per_session", metrics.Metric{Value: fp(3600)}, Parts{"3,600", "chars", ""}},
		{"bank.size", metrics.Metric{Value: fp(1284)}, Parts{"1,284", "", ""}},
		{"bank.size", metrics.Metric{}, Parts{"—", "", ""}},
	}
	for _, c := range cases {
		if got := English.ValueParts(def(t, c.id), c.m); got != c.want {
			t.Errorf("%s: parts %+v, want %+v", c.id, got, c.want)
		}
	}
	cost := def(t, "extraction.cost")
	eq(t, English.Value(cost, metrics.Metric{}), "price not set")
	eq(t, English.Value(cost, metrics.Metric{Value: fp(43.62), Extra: map[string]any{"currency": "USD"}}), "$43.62")
}

func TestDeltas(t *testing.T) {
	cases := []struct {
		name string
		id   string
		m    metrics.Metric
		want Delta
	}{
		{"up better", "recall.hit_rate", metrics.Metric{Value: fp(0.3), Delta: fp(0.02)}, Delta{"▲", "+2 pts, better", "better"}},
		{"down worse", "recall.hit_rate", metrics.Metric{Value: fp(0.3), Delta: fp(-0.01)}, Delta{"▼", "−1 pt, worse", "worse"}},
		{"down metric rising", "verdict.wrong_rate", metrics.Metric{Value: fp(0.1), Delta: fp(0.03)}, Delta{"▲", "+3 pts, worse", "worse"}},
		{"zero", "recall.hit_rate", metrics.Metric{Value: fp(0.3), Delta: fp(0)}, Delta{"=", "no change", ""}},
		{"sub point", "recall.hit_rate", metrics.Metric{Value: fp(0.3), Delta: fp(0.004)}, Delta{"▲", "+0.4 pts, better", "better"}},
		{"slow seconds", "recall.latency", metrics.Metric{Value: fp(1600), Delta: fp(100)}, Delta{"▲", "+0.1 s, worse", "worse"}},
		{"fast ms", "recall.latency", metrics.Metric{Value: fp(910), Delta: fp(-700)}, Delta{"▼", "−700 ms, better", "better"}},
		{"chars", "context.per_session", metrics.Metric{Value: fp(3000), Delta: fp(-200)}, Delta{"▼", "−200 chars, better", "better"}},
		{"count no direction", "bank.size", metrics.Metric{Value: fp(1284), Delta: fp(46)}, Delta{"▲", "+46", ""}},
	}
	for _, c := range cases {
		c.m.Status = metrics.StatusOK
		got, ok := English.Delta(def(t, c.id), c.m)
		if !ok || got != c.want {
			t.Errorf("%s: %+v %v, want %+v", c.name, got, ok, c.want)
		}
	}
	hit := def(t, "recall.hit_rate")
	for name, m := range map[string]metrics.Metric{
		"too few":  {Value: fp(0.3), Delta: fp(0.02), Status: metrics.StatusTooFew},
		"no data":  {Delta: fp(0.02), Status: metrics.StatusNoData},
		"snapshot": {Value: fp(0.3), Delta: fp(0.02), Status: metrics.StatusOK, Tags: []string{"snapshot"}},
		"no delta": {Value: fp(0.3), Status: metrics.StatusOK},
	} {
		if d, ok := English.Delta(hit, m); ok {
			t.Errorf("%s: got %+v, want none", name, d)
		}
	}
	// ratio delta between half a point and a point
	dl, _ := English.Delta(hit, metrics.Metric{Value: fp(0.3), Delta: fp(0.006), Status: metrics.StatusOK})
	eq(t, dl.Text, "+1 pt, better")
	dl, _ = English.Delta(hit, metrics.Metric{Value: fp(0.3), Delta: fp(0.0049), Status: metrics.StatusOK})
	eq(t, dl.Text, "+0.5 pts, better")
	// ms delta under 50 ms at a value of a second or more stays in ms
	dl, _ = English.Delta(def(t, "recall.latency"), metrics.Metric{Value: fp(1600), Delta: fp(40), Status: metrics.StatusOK})
	eq(t, dl.Text, "+40 ms, worse")
}

func TestLocaleFromHostileHeaders(t *testing.T) {
	cases := []struct{ h, tag string }{
		{"", "en"}, {"*", "en"}, {"zz-ZZ", "en"}, {"en;q=0, xx", "en"},
		{"fr-FR,fr;q=0.9,en;q=0.8", "fr"}, {"en;q=0, fr", "fr"}, {"fr;q=abc, de", "de"},
		{"fr;q=2, de", "de"}, {"xx, de;q=0.5", "de"}, {"de;q=0.5, fr;q=0.8", "fr"},
		{"de;q=0.5, fr;q=0.5", "de"}, {"de-CH", "de-CH"}, {"it-ch", "it-CH"}, {"de-AT", "de"},
		{strings.Repeat("xx,", 200) + "fr", "en"},
		{strings.Repeat("xx,", 31) + "fr", "fr"},
		{strings.Repeat("a", 10<<10), "en"},
		{strings.Repeat("fr;q=0.1,", 2000), "fr"},
	}
	for _, c := range cases {
		l := FromAcceptLanguage(c.h)
		if l.Tag != c.tag {
			t.Errorf("%.30q: tag %q, want %q", c.h, l.Tag, c.tag)
		}
		if l.Group == "" || l.Decimal == "" {
			t.Errorf("%.30q: separators unset: %+v", c.h, l)
		}
	}
	// the cap reads 32 entries: the 33rd is never seen
	eq(t, FromAcceptLanguage(strings.Repeat("xx,", 32)+"fr").Tag, "en")
	// q=0 refuses the tag
	for h, tag := range map[string]string{
		"fr;q=0": "en", "fr;q=0, de": "de", "fr;q=0.0,de;q=0.1": "de", "de;q=0, fr;q=0.1": "fr",
		"fr;q=0.000, fr-CA;q=0": "en", "de-CH;q=0, de": "de",
	} {
		eq(t, FromAcceptLanguage(h).Tag, tag)
	}

	// hostile headers with a concrete outcome
	eq(t, FromAcceptLanguage(strings.Repeat("x", 10<<10)+",fr").Tag, "fr")
	eq(t, FromAcceptLanguage(strings.Repeat("xx,", 40)+"de;q=1,fr").Tag, "en")
	eq(t, FromAcceptLanguage(strings.Repeat("xx,", 31)+"de,fr;q=0.9").Tag, "de")
	eq(t, FromAcceptLanguage(strings.Repeat("en;q=0.1,", 31)+"fr;q=0.2,de;q=0.9").Tag, "fr")
}

func TestRelativeTimeEdges(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	p := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02T15:04", s, paris)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	now := p("2026-10-30T15:44")
	cases := []struct {
		now, at time.Time
		want    string
	}{
		{now, now.Add(-25 * time.Minute), "25 min ago"},
		{now, now.Add(-30 * time.Second), "just now"},
		{now, now.Add(5 * time.Minute), "just now"},
		{now, now.Add(-2 * time.Hour), "2 h ago"},
		{now, p("2026-10-29T20:00"), "yesterday"},
		{now, now.Add(-24 * time.Hour), "yesterday"},
		{now, p("2026-10-28T09:00"), "2 d ago"},
		{now, p("2026-10-12T10:00"), "Oct 12"},
		{now, p("2025-12-01T10:00"), "2025-12-01"},
		{p("2026-10-30T00:30"), p("2026-10-30T00:30").Add(-40 * time.Minute), "40 min ago"},
		{p("2026-10-30T00:30"), p("2026-10-30T00:30").Add(-3 * time.Hour), "yesterday"},
	}
	for _, c := range cases {
		eq(t, Rel(c.now, c.at, paris).Text, c.want)
	}
	r := Rel(now, now.Add(-25*time.Minute), paris)
	eq(t, r.Datetime, "2026-10-30T15:19:00+01:00")
	eq(t, r.Title, "2026-10-30 15:19 CET")

	// across the October DST change: 25 hours apart, one calendar day
	dstNow := time.Date(2026, 10, 26, 1, 0, 0, 0, paris)
	dstAt := time.Date(2026, 10, 25, 1, 30, 0, 0, paris)
	d := Rel(dstNow, dstAt, paris)
	eq(t, d.Text, "yesterday")
	if !strings.HasSuffix(d.Datetime, "+02:00") {
		t.Errorf("datetime %q does not end +02:00", d.Datetime)
	}
	// the window's zone decides the day, not the instant's own
	late := p("2026-10-30T00:30")
	eq(t, Rel(late, late.Add(-90*time.Minute), paris).Text, "yesterday")
	eq(t, Rel(late, late.Add(-90*time.Minute), time.UTC).Text, "1 h ago")
	eq(t, Rel(now, now.Add(-time.Hour), nil).Title[len("2026-10-30 14:44 "):], "UTC")
	// calendar days across the DST changes (at before 01:00 on the long day)
	at := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, paris) }
	// fall-back day, 25 h long: at is before 01:00 CEST
	eq(t, Rel(at(2026, 10, 26, 0, 30), at(2026, 10, 25, 0, 10), paris).Text, "yesterday")
	eq(t, Rel(at(2026, 10, 25, 0, 30), at(2026, 10, 24, 23, 50), paris).Text, "40 min ago")
	// spring-forward day, 23 h long
	eq(t, Rel(at(2026, 3, 30, 0, 30), at(2026, 3, 29, 0, 10), paris).Text, "yesterday")
	eq(t, Rel(at(2026, 3, 31, 0, 30), at(2026, 3, 29, 0, 10), paris).Text, "2 d ago")
	eq(t, Rel(at(2026, 3, 30, 12, 0), at(2026, 3, 29, 23, 30), paris).Text, "yesterday")
	// Rel 6 d / 7 d
	wk := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	eq(t, Rel(wk, wk.AddDate(0, 0, -6), nil).Text, "6 d ago")
	eq(t, Rel(wk, wk.AddDate(0, 0, -7), nil).Text, "Oct 23")
}

func TestClockAndCount(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 44, 0, 0, time.UTC)
	eq(t, Clock(now, time.Date(2026, 10, 30, 9, 12, 0, 0, time.UTC), nil), "09:12")
	eq(t, Clock(now, time.Date(2026, 10, 29, 9, 12, 0, 0, time.UTC), nil), "Oct 29 09:12")
	eq(t, Count(English, 1, "turn", "turns"), "1 turn")
	eq(t, Count(English, 61, "turn", "turns"), "61 turns")
	eq(t, Count(English, 1234, "turn", "turns"), "1,234 turns")
	// Clock across midnight within 24 h
	eq(t, Clock(time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 29, 23, 0, 0, 0, time.UTC), nil), "Oct 29 23:00")
	eq(t, Count(English, 0, "turn", "turns"), "0 turns")
}

func TestNonFiniteRendersABareDash(t *testing.T) {
	// non-finite: a bare dash, no unit
	nan := math.NaN()
	for _, got := range []string{English.Percent(nan), English.Size(math.Inf(1), "chars"), English.Duration(nan), English.Money(nan, "USD"), English.Interval([2]float64{nan, 1})} {
		eq(t, got, "—")
	}
	eq(t, English.Value(def(t, "recall.hit_rate"), metrics.Metric{Value: fp(nan)}), "—")
	eq(t, English.Value(def(t, "recall.latency"), metrics.Metric{Value: fp(math.Inf(1))}), "—")
}
