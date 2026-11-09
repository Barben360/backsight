package dashboard

import (
	"context"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// The verdict is always judged on the last 7 days, and the banner says so:
// its axis lines and the summary text quote the last 7 days' figures,
// whatever window the page shows. On a 30-day page, Found's line is the
// 7-day share of prompts that got memory and Flowing's the 7-day items sent,
// not the 30-day ones the tiles show.
func TestVerdictLinesQuoteTheLastSevenDays(t *testing.T) {
	h := dataHarness(t)
	get := func(p views.Params) metrics.Result {
		r, st, err := h.s.data.result(context.Background(), "overview", p, "")
		if err != nil || st != stateReady {
			t.Fatalf("overview %+v: state %d, %v", p, st, err)
		}
		return r
	}
	r7 := get(views.Params{Limit: metrics.DefaultLimit})
	r30 := get(views.Params{Limit: metrics.DefaultLimit, Days: 30, DaysSet: true})
	hit7, hit30 := r7.Metrics["recall.hit_rate"], r30.Metrics["recall.hit_rate"]
	flow7, flow30 := r7.Metrics["pipeline.flow"], r30.Metrics["pipeline.flow"]
	if hit7.Value == nil || hit30.Value == nil || flow7.Den == nil || flow30.Den == nil {
		t.Fatalf("the generated data has no hit rate or flow: %v %v %v %v", hit7.Value, hit30.Value, flow7.Den, flow30.Den)
	}
	seven, thirty := en.Percent(*hit7.Value), en.Percent(*hit30.Value)
	sentSeven, sentThirty := ofCount(en, *flow7.Num, *flow7.Den, "item", "items"), ofCount(en, *flow30.Num, *flow30.Den, "item", "items")
	if seven == thirty || sentSeven == sentThirty {
		t.Fatalf("the 7 and 30 days read alike (%s, %s): the test cannot tell them apart", seven, sentSeven)
	}

	b := bannerOf(&r30, en, r30.Window.Loc, views.Params{Days: 30, DaysSet: true})
	notes := map[string]string{}
	for _, a := range b.Axes {
		notes[a.Name] = a.Note
	}
	if st := r30.Verdict.Axes["found"].State; (st == metrics.AxisOK || st == metrics.AxisAttention) && !strings.HasPrefix(notes["Found"], seven+" ") {
		t.Errorf("Found line %q, want the 7-day %s (the window's is %s)", notes["Found"], seven, thirty)
	}
	if failed, _ := flow7.Extra["failed"].(int64); failed == 0 && notes["Flowing"] != sentSeven+" sent" {
		t.Errorf("Flowing line %q, want %q (the window's is %q)", notes["Flowing"], sentSeven+" sent", sentThirty)
	}
	b7 := bannerOf(&r7, en, r7.Window.Loc, views.Params{})
	for i, a := range b7.Axes {
		if b.Axes[i] != a {
			t.Errorf("%s on 30 days %+v, on 7 days %+v: the same verdict, the same line", a.Name, b.Axes[i], a)
		}
	}
	text := summaryText(&r30, views.Params{Days: 30, DaysSet: true}, r30.Window.Loc, "last 30 days", "test")
	if !strings.Contains(text, sentSeven+" sent") || strings.Contains(text, sentThirty) {
		t.Errorf("summary text quotes the window, not the last 7 days (%q):\n%s", sentSeven, text)
	}
}
