package dashboard

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

var twoViews = pageSpec{Name: "overview", File: "overview.html", Views: []string{"overview", "days"}}

func TestReadViewsKeepsTheDeclaredOrder(t *testing.T) {
	// The first view answers last: the answers must still be in the views'
	// order, not in the order they finish.
	release := make(chan struct{})
	read := func(ctx context.Context, view string, p views.Params, id string) (metrics.Result, indexState, error) {
		if view == "overview" {
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
		} else {
			close(release)
		}
		return metrics.Result{View: view}, stateReady, nil
	}
	outs := readViewsWith(context.Background(), twoViews, views.Params{}, "", read)
	if len(outs) != 2 {
		t.Fatalf("%d answers, want 2", len(outs))
	}
	if outs[0].res.View != "overview" || outs[1].res.View != "days" {
		t.Errorf("answers in order %q, %q; want overview, days", outs[0].res.View, outs[1].res.View)
	}
}

func TestFirstProblemIsTheFirstViewsInDeclaredOrder(t *testing.T) {
	boom := errors.New("boom")
	other := errors.New("other")
	for _, c := range []struct {
		name    string
		outs    []viewOut
		ok      bool
		st      indexState
		err     error
		errIsIx bool
	}{
		{name: "both ready", outs: []viewOut{{}, {}}},
		{name: "the first fails, the second is fine", outs: []viewOut{{err: boom}, {}}, ok: true, err: boom},
		{name: "the second fails, the first is fine", outs: []viewOut{{}, {err: boom}}, ok: true, err: boom},
		{name: "both fail: the first's error", outs: []viewOut{{err: boom}, {err: other}}, ok: true, err: boom},
		{name: "the first fails, the second is building: the error", outs: []viewOut{{err: boom}, {st: stateBuilding}}, ok: true, err: boom},
		{name: "the first is building, the second fails: the state", outs: []viewOut{{st: stateBuilding}, {err: boom}}, ok: true, st: stateBuilding},
		{name: "the first is updated, the second building: updated", outs: []viewOut{{st: stateUpdated}, {st: stateBuilding}}, ok: true, st: stateUpdated},
		{name: "an update in progress is the updating state", outs: []viewOut{{err: index.ErrUpdateInProgress}, {}}, ok: true, st: stateUpdating},
		{name: "a broken index with no error is told as no index", outs: []viewOut{{}, {st: stateBroken}}, ok: true, st: stateBroken, errIsIx: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			bad, ok := firstProblem(c.outs)
			if ok != c.ok {
				t.Fatalf("problem %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if bad.st != c.st {
				t.Errorf("state %d, want %d", bad.st, c.st)
			}
			switch {
			case c.errIsIx:
				if !errors.Is(bad.err, errNoIndex) {
					t.Errorf("error %v, want the missing index", bad.err)
				}
			case !errors.Is(bad.err, c.err) || (c.err == nil) != (bad.err == nil):
				t.Errorf("error %v, want %v", bad.err, c.err)
			}
		})
	}
}

func TestAViewThatPanicsFailsTheRequestNotTheProcess(t *testing.T) {
	read := func(_ context.Context, view string, _ views.Params, _ string) (metrics.Result, indexState, error) {
		if view == "days" {
			panic("secret path /home/someone/x")
		}
		return metrics.Result{View: view}, stateReady, nil
	}
	outs := readViewsWith(context.Background(), twoViews, views.Params{}, "", read)
	if len(outs) != 2 || outs[0].err != nil || outs[0].res.View != "overview" {
		t.Fatalf("the other view's answer was lost: %+v", outs)
	}
	if !errors.Is(outs[1].err, errViewFailed) {
		t.Fatalf("the panicking view's slot: %v, want errViewFailed", outs[1].err)
	}
	bad, ok := firstProblem(outs)
	if !ok || !errors.Is(bad.err, errViewFailed) {
		t.Errorf("the page does not fail on it: %+v, %v", bad, ok)
	}
	if strings.Contains(bad.err.Error(), "secret") {
		t.Errorf("the panic's value reached the error: %q", bad.err)
	}
}

// On all PCs the Overview and Cost say "This PC's N" with this PC's own count, not the merged one.
// The merged answer carries this PC's (no second read, and the result cache keeps it): the fixture has other
// PCs, so the two counts differ.
func TestAllPCsPagesShowThisPCsOwnCount(t *testing.T) {
	old := dataSpec
	t.Cleanup(func() { dataSpec = old })
	dataSpec.OtherPCs = 3
	h := dataHarness(t)
	ck := cookieHdr(h.login())
	countRE := regexp.MustCompile(`This PC’s ([0-9,]+) (?:prompt|turn|session|recall)s? →`)
	for _, c := range []struct{ path, view, metric string }{
		{"/?machines=all", "overview", "recall.hit_rate"},
		{"/cost?machines=all", "cost", "recall.latency"},
	} {
		ctx := context.Background()
		all, _, err := h.s.data.result(ctx, c.view, views.Params{Machines: "all", Limit: 50}, "")
		if err != nil {
			t.Fatal(err)
		}
		this, _, err := h.s.data.result(ctx, c.view, views.Params{Machines: "this", Limit: 50}, "")
		if err != nil {
			t.Fatal(err)
		}
		own := all.ThisPC()
		if own == nil || own.Metrics[c.metric].Den == nil || *own.Metrics[c.metric].Den != *this.Metrics[c.metric].Den {
			t.Fatalf("%s: the cached merged result does not carry this PC's answer: %+v", c.path, own)
		}
		if *all.Metrics[c.metric].Den == *this.Metrics[c.metric].Den {
			t.Fatalf("%s: fixture: merged and own counts are both %d", c.path, *this.Metrics[c.metric].Den)
		}
		resp := h.do("GET", c.path, ck...)
		text := visibleText(body(t, resp))
		ms := countRE.FindAllStringSubmatch(text, -1)
		if resp.StatusCode != 200 || len(ms) == 0 {
			t.Fatalf("GET %s: %d, no \"This PC's N\" link in %.300s", c.path, resp.StatusCode, text)
		}
		for _, m := range ms {
			n := strings.ReplaceAll(m[1], ",", "")
			if n == strconv.FormatInt(*all.Metrics[c.metric].Den, 10) {
				t.Errorf("GET %s: %q shows the merged count", c.path, m[0])
			}
		}
		if want := "This PC’s " + strconv.FormatInt(*this.Metrics[c.metric].Den, 10) + " prompts →"; !strings.Contains(text, want) {
			t.Errorf("GET %s: no %q in %.300s", c.path, want, text)
		}
	}
}
