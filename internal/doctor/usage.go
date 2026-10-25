package doctor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/stats"
)

// Thresholds read from the local telemetry (SPEC §15). Each needs a
// minimum of data before it judges: a quiet week is not a failure.
const (
	recallFailMax     = 0.05 // scope calls failed over 7 days
	recallFailMinN    = 20
	promptP95MaxMS    = 2500 // decision 0048
	promptP95MinN     = 20
	searchDropMinN    = 10 // injections in each of the two weeks
	learningQuietTime = 2 * time.Hour
)

// checkUsage judges the last 7 days of telemetry against the previous 7.
func checkUsage(dir string, now time.Time) []Check {
	week := 7 * 24 * time.Hour
	ev, err := stats.Load(dir, now.Add(-2*week), now)
	if err != nil {
		return []Check{{ID: "usage", Status: Warn, Summary: "telemetry unreadable: " + err.Error()}}
	}
	var this, last []stats.Event
	for _, e := range ev {
		if e.TS.Before(now.Add(-week)) {
			last = append(last, e)
		} else {
			this = append(this, e)
		}
	}
	if len(this) == 0 {
		return []Check{{ID: "usage", Status: Info, Summary: "no telemetry in the last 7 days: nothing to judge yet"}}
	}
	r := stats.Compute(this, now.Add(-week), now)
	p := stats.Compute(last, now.Add(-2*week), now.Add(-week))
	return []Check{
		checkRecallFailures(r),
		checkLatency(r),
		checkLearning(ev, now),
		checkSearchRate(r, p),
		checkUnknownRecords(r, p),
	}
}

func checkRecallFailures(r stats.Report) Check {
	c := Check{ID: "recall", Status: OK}
	n, failed := r.Prompts.ScopeCalls, r.Prompts.ScopeFailures
	switch {
	case n < recallFailMinN:
		c.Status, c.Summary = Info, fmt.Sprintf("%d recall scope calls in 7 days: too few to judge", n)
	case float64(failed) > recallFailMax*float64(n):
		c.Status, c.Summary = Warn, fmt.Sprintf("%d of %d recall scope calls failed in 7 days (over 5%%)", failed, n)
		c.Fix = "backsight server status; backsight stats 7 shows when"
	default:
		c.Summary = fmt.Sprintf("%d of %d recall scope calls failed in 7 days", failed, n)
	}
	return c
}

func checkLatency(r stats.Report) Check {
	c := Check{ID: "latency", Status: OK}
	switch {
	case r.Prompts.Recalls < promptP95MinN:
		c.Status, c.Summary = Info, fmt.Sprintf("%d prompt recalls in 7 days: too few for a p95", r.Prompts.Recalls)
	case r.Prompts.P95MS > promptP95MaxMS:
		c.Status, c.Summary = Warn, fmt.Sprintf("prompt hook p95 %d ms over 7 days (limit %d ms)", r.Prompts.P95MS, promptP95MaxMS)
		c.Fix = "a GPU for the reranker, or fewer rerank candidates (recall settings in config.toml)"
	default:
		c.Summary = fmt.Sprintf("prompt hook p95 %d ms over 7 days", r.Prompts.P95MS)
	}
	return c
}

// checkLearning: sessions ran in the last 24 h (and had time to settle)
// but nothing was accepted by the server: "memory not learning".
func checkLearning(ev []stats.Event, now time.Time) Check {
	c := Check{ID: "learning", Status: OK}
	ran, accepted := 0, 0
	for _, e := range ev {
		if e.TS.Before(now.Add(-24 * time.Hour)) {
			continue
		}
		switch {
		case e.Event == "prompt" && e.Reason != "machine" && e.TS.Before(now.Add(-learningQuietTime)):
			ran++
		case e.Event == "retain" && e.Status == "ok":
			accepted++
		case e.Event == "job" && notesSent(e.Details) > 0:
			accepted++
		}
	}
	switch {
	case ran == 0:
		c.Status, c.Summary = Info, "no settled session in the last 24 h"
	case accepted == 0:
		c.Status, c.Summary = Warn, fmt.Sprintf("memory not learning: %d prompt(s) in the last 24 h, nothing accepted by the server", ran)
		c.Fix = "backsight retain flush (shows why), then backsight server status"
	default:
		c.Summary = fmt.Sprintf("%d session send(s) accepted in the last 24 h", accepted)
	}
	return c
}

func notesSent(d map[string]any) int {
	if v, ok := d["notes_sent"].(float64); ok {
		return int(v)
	}
	return 0
}

// checkSearchRate: injections followed by a memory search, this week
// against last week; halved is a warning (the instructions stopped
// working, or recall turned noisy).
func checkSearchRate(r, p stats.Report) Check {
	c := Check{ID: "searches", Status: OK}
	ni, np := r.Injections.N, p.Injections.N
	if ni < searchDropMinN || np < searchDropMinN {
		c.Status, c.Summary = Info, fmt.Sprintf("%d and %d injections this week and last: too few to compare", ni, np)
		return c
	}
	now, before := float64(r.Injections.Searched)/float64(ni), float64(p.Injections.Searched)/float64(np)
	c.Summary = fmt.Sprintf("search after injection %.0f%% this week, %.0f%% last week", 100*now, 100*before)
	if now < before/2 {
		c.Status, c.Fix = Warn, "backsight stats 14 and backsight why: check what recall injects"
	}
	return c
}

// checkUnknownRecords: transcript record types Claude Code was not known
// to write, rising week over week (SPEC §14 format change).
func checkUnknownRecords(r, p stats.Report) Check {
	c := Check{ID: "transcripts", Status: OK, Summary: "no unknown transcript records this week"}
	sum := func(m map[string]int) int {
		n := 0
		for _, v := range m {
			n += v
		}
		return n
	}
	now, before := sum(r.Unknown), sum(p.Unknown)
	if now == 0 {
		return c
	}
	types := make([]string, 0, len(r.Unknown))
	for t := range r.Unknown {
		types = append(types, t)
	}
	sort.Strings(types)
	c.Summary = fmt.Sprintf("%d unknown transcript records this week (%d last week): %s", now, before, strings.Join(types, ", "))
	c.Status = Info
	if now > before {
		c.Status, c.Fix = Warn, "Claude Code's transcript format may have changed: check that these types carry no conversation, then report it"
	}
	return c
}
