package chart

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func rankHist() Hist {
	ns := []int64{9, 6, 4, 2, 1, 1}
	h := Hist{Label: "Position when shown", Size: Half, XName: "Rank when shown", Unit: "times shown", Fmt: plain, Class: "f-used", More: "backsight stats"}
	for i, n := range ns {
		l := "Rank " + strconv.Itoa(i+1)
		if i == 5 {
			l = "Rank 6+"
		}
		h.Bins = append(h.Bins, Bin{l, n})
	}
	return h
}

var rectRe = regexp.MustCompile(`<rect class="([^"]*)" x="([^"]*)" y="([^"]*)" width="([^"]*)" height="([^"]*)" rx="2"[^>]*><title>([^<]*)</title></rect>`)

func TestHistBins(t *testing.T) {
	h := rankHist()
	h.Markers = []Marker{{"typical", 2.5, "thr-w"}, {"high", 4.5, "thr"}}
	out := string(h.HTML())
	rects := rectRe.FindAllStringSubmatch(out, -1)
	if len(rects) != 6 {
		t.Fatalf("want 6 bars, got %d:\n%s", len(rects), out)
	}
	if rects[0][6] != "Rank 1: 9 times shown" || rects[0][1] != "f-used" {
		t.Errorf("first bar: %v", rects[0])
	}
	hs := make([]float64, 6)
	for i, r := range rects {
		hs[i], _ = strconv.ParseFloat(r[5], 64)
		if i > 0 && hs[i] >= hs[i-1] && h.Bins[i].N != h.Bins[i-1].N {
			t.Errorf("bar %d is not shorter than bar %d: %v", i, i-1, hs)
		}
	}
	if hs[0] <= hs[1] {
		t.Errorf("the first bar must be the tallest: %v", hs)
	}
	lines := regexp.MustCompile(`<line class="(thr-w|thr)" x1="([^"]*)" x2="[^"]*" y1="14.0" y2="200.0"></line><text class="lbl ta-m" x="[^"]*" y="12">([a-z]+)</text>`).FindAllStringSubmatch(out, -1)
	if len(lines) != 2 || lines[0][3] != "typical" || lines[1][3] != "high" || lines[0][1] == lines[1][1] {
		t.Fatalf("markers: %v", lines)
	}
	// 2.5 bins in: left + 2.5 slots; slot = (552 - 44) / 6.
	want := f(44 + 2.5*(552.0-44)/6)
	if lines[0][2] != want {
		t.Errorf("marker x %s, want %s", lines[0][2], want)
	}
	must(t, out, `<th>Rank when shown</th><th class="r">times shown</th>`, `<td>Rank 1</td><td class="r">9</td>`)
}

func TestHistMergesBinsPastTheCap(t *testing.T) {
	h := rankHist()
	h.Bins = nil
	var total int64
	for i := 0; i < 1200; i++ {
		h.Bins = append(h.Bins, Bin{fmt.Sprintf("b%d", i), int64(i%5 + 1)})
		total += int64(i%5 + 1)
	}
	out := string(h.HTML())
	rects := rectRe.FindAllStringSubmatch(out, -1)
	if len(rects) > MaxMarks || len(rects) != 400 {
		t.Fatalf("want 400 merged bars (3 per bar), got %d", len(rects))
	}
	if rects[0][6] != "b0–b2: 6 times shown" { // 1+2+3
		t.Errorf("merged title %q", rects[0][6])
	}
	var sum int64
	for _, r := range rects {
		n, _ := strconv.ParseInt(strings.Fields(strings.SplitN(r[6], ": ", 2)[1])[0], 10, 64)
		sum += n
	}
	if sum != total {
		t.Errorf("merging lost counts: %d of %d", sum, total)
	}
}

func TestHistFocusRule(t *testing.T) {
	h := rankHist()
	out := string(h.HTML())
	if strings.Count(out, `<rect class="f-used"`) != 6 || strings.Count(out, `tabindex="0" aria-label="Rank `) != 6 {
		t.Errorf("each of 6 bars is a focus stop: %s", out)
	}
	if strings.Contains(out, `role="img" aria-label="Position when shown" tabindex`) {
		t.Error("the svg is also a stop")
	}
	h.Bins = nil
	for i := 0; i < FocusMax+1; i++ {
		h.Bins = append(h.Bins, Bin{strconv.Itoa(i), 1})
	}
	out = string(h.HTML())
	if strings.Count(out, `<rect class="f-used"`) != FocusMax+1 || strings.Count(out, "tabindex") != 1 ||
		!strings.Contains(out, `aria-label="Position when shown" tabindex="0">`) {
		t.Errorf("past %d bars the svg alone is a stop", FocusMax)
	}
}

func TestHistSurvivesDegenerateBins(t *testing.T) {
	h := rankHist()
	h.Empty = "No sessions yet."
	for name, bins := range map[string][]Bin{"none": nil, "zeros": {{"a", 0}, {"b", 0}}, "negative": {{"a", -4}}} {
		hh := h
		hh.Bins = bins
		if out := string(hh.HTML()); out != `<p class="empty">No sessions yet.</p>` {
			t.Errorf("%s: %s", name, out)
		}
	}
	for name, hh := range map[string]Hist{
		"one":                      {Size: Half, Bins: []Bin{{"a", 3}}, Fmt: plain},
		"negative beside positive": {Size: Half, Bins: []Bin{{"a", -4}, {"b", 2}}, Fmt: plain},
		"huge":                     {Size: Half, Bins: []Bin{{"a", math.MaxInt64 / 2}, {"b", 1}}, Fmt: plain},
		"nan marker":               {Size: Half, Bins: []Bin{{"a", 3}}, Markers: []Marker{{"x", math.NaN(), ""}, {"y", math.Inf(1), ""}, {"z", 99, "evil"}}, Fmt: plain},
		"nil fmt":                  {Size: Half, Bins: []Bin{{"a", 3}}},
	} {
		out := string(hh.HTML())
		if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") || len(rectRe.FindAllString(out, -1)) != len(hh.Bins) {
			t.Errorf("%s: %s", name, out)
		}
		if strings.Contains(out, "evil") || strings.Contains(out, ">x</text>") || strings.Contains(out, ">y</text>") {
			t.Errorf("%s: a bad marker is drawn: %s", name, out)
		}
	}
	// a negative count draws a zero-height bar, never a negative one.
	out := string(Hist{Size: Half, Bins: []Bin{{"a", -4}, {"b", 2}}, Fmt: plain}.HTML())
	if r := rectRe.FindAllStringSubmatch(out, -1); r[0][5] != "0.0" || r[0][6] != "a: 0" {
		t.Errorf("negative bin: %v", r[0])
	}
	// a marker past the last bin stays on the plot.
	if out := string(Hist{Size: Half, Bins: []Bin{{"a", 3}}, Markers: []Marker{{"z", 99, ""}}, Fmt: plain}.HTML()); !strings.Contains(out, `x1="552.0" x2="552.0"`) {
		t.Errorf("marker not clamped: %s", out)
	}
}

func TestHistLabelsAreAtMostTwelve(t *testing.T) {
	h := rankHist()
	h.Size = Full
	h.Bins = nil
	for i := 0; i < 40; i++ {
		h.Bins = append(h.Bins, Bin{"Rank " + strconv.Itoa(i*10), 1})
	}
	out := string(h.HTML())
	n := strings.Count(out, `<text class="ta-m"`)
	if n < 7 || n > 12 {
		t.Errorf("want between 7 and 12 x labels on 40 bins, got %d", n)
	}
	h.Size = Half
	if n2 := strings.Count(string(h.HTML()), `<text class="ta-m"`); n2 >= n {
		t.Errorf("a narrower plot keeps fewer labels, not overlapping ones: %d vs %d", n2, n)
	}
}

func TestHistEscapesText(t *testing.T) {
	h := Hist{Label: markup, Size: Half, XName: markup, Unit: markup, Fmt: func(float64) string { return markup },
		Bins: []Bin{{markup, 2}, {markup + "2", 3}}, Markers: []Marker{{markup, 1, "thr-w"}}, More: markup, Empty: markup}
	out := string(h.HTML())
	for _, want := range []string{`aria-label="` + escaped + `"`, `<th>` + escaped, `>` + escaped + `</text>`, `<title>` + escaped} {
		if !strings.Contains(out, want) {
			t.Errorf("histogram lacks %q", want)
		}
	}
	if strings.Contains(out, "<b>") || strings.Contains(out, `x"'`) {
		t.Errorf("raw markup reaches the output: %s", out)
	}
	if e := string(Hist{Empty: markup}.HTML()); !strings.Contains(e, escaped) || strings.Contains(e, "<b>") {
		t.Errorf("empty sentence: %s", e)
	}
	h.More = markup
	h.Bins = nil
	for i := 0; i < 130; i++ {
		h.Bins = append(h.Bins, Bin{markup, 1})
	}
	if !strings.Contains(string(h.HTML()), `<code>`+escaped+`</code>`) {
		t.Error("the more-command is not escaped")
	}
}
