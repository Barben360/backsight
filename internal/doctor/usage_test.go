package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var usageNow = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// events writes telemetry lines, each at now minus its age.
func events(t *testing.T, lines map[time.Duration][]string) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	for age, ls := range lines {
		for _, l := range ls {
			fmt.Fprintf(&b, `{"ts":%q,"session":"s1",%s}`+"\n", usageNow.Add(-age).Format("2006-01-02T15:04:05.000Z"), l)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "events-2026-10.jsonl"), []byte(b.String()), 0o600)
	return dir
}

func byID(cs []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range cs {
		m[c.ID] = c
	}
	return m
}

func repeat(n int, s string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestUsageWithNoTelemetryJudgesNothing(t *testing.T) {
	cs := checkUsage(t.TempDir(), usageNow)
	if len(cs) != 1 || cs[0].Status != Info {
		t.Errorf("%+v", cs)
	}
}

func TestUsageWarnsOnEachThreshold(t *testing.T) {
	day := 24 * time.Hour
	// this week: 20 recalls, 3 s each, 2 of 2 scopes failing on 2 of them
	// (2/40 = 5%: not over); 10 injections, 1 searched; prompts 3 h ago and
	// no accepted retain; unknown records rising.
	recall := `"event":"prompt","decision":"recall","plen":80,"ms":3000,"scopes":{"project":{"ok":true},"global":{"ok":true}}`
	failing := `"event":"prompt","decision":"recall","plen":80,"ms":3000,"scopes":{"project":{"ok":false},"global":{"ok":true}}`
	dir := events(t, map[time.Duration][]string{
		3 * time.Hour: append(append(repeat(18, recall), repeat(2, failing)...), `"event":"retain","status":"queued"`,
			`"event":"transcript_unknown","types":{"new-kind":5}`),
		2 * day: append(repeat(9, `"event":"outcome","searched":false`), `"event":"outcome","searched":true`),
		9 * day: append(append(repeat(6, `"event":"outcome","searched":true`), repeat(4, `"event":"outcome","searched":false`)...),
			`"event":"transcript_unknown","types":{"new-kind":1}`),
	})
	got := byID(checkUsage(dir, usageNow))
	for id, want := range map[string]Status{"recall": OK, "latency": Warn, "learning": Warn, "searches": Warn, "transcripts": Warn} {
		if got[id].Status != want {
			t.Errorf("%s: %s (%s), want %s", id, got[id].Status, got[id].Summary, want)
		}
	}
	if !strings.Contains(got["latency"].Summary, "3000 ms") || !strings.Contains(got["transcripts"].Summary, "new-kind") || got["learning"].Fix == "" {
		t.Errorf("%+v", got)
	}
}

func TestUsageHealthyWeek(t *testing.T) {
	recall := `"event":"prompt","decision":"recall","plen":80,"ms":900,"scopes":{"project":{"ok":true}}`
	failing := `"event":"prompt","decision":"recall","plen":80,"ms":900,"scopes":{"project":{"ok":false}}`
	dir := events(t, map[time.Duration][]string{
		3 * time.Hour:      append(append(repeat(19, recall), failing), `"event":"retain","status":"ok"`),
		24 * time.Hour * 2: repeat(10, `"event":"outcome","searched":true`),
		24 * time.Hour * 9: append(repeat(10, `"event":"outcome","searched":true`), `"event":"transcript_unknown","types":{"x":4}`),
		24 * time.Hour * 15: { // outside both weeks
			`"event":"transcript_unknown","types":{"y":9}`},
	})
	got := byID(checkUsage(dir, usageNow))
	for _, id := range []string{"recall", "latency", "learning", "searches", "transcripts"} {
		if got[id].Status != OK {
			t.Errorf("%s: %s (%s)", id, got[id].Status, got[id].Summary)
		}
	}
	// 2 failures out of 21 calls is over 5%
	dir = events(t, map[time.Duration][]string{time.Hour: append(repeat(19, recall), failing, failing)})
	if c := byID(checkUsage(dir, usageNow))["recall"]; c.Status != Warn {
		t.Errorf("recall: %+v", c)
	}
}

// Notes sent by a tick count as learning too.
func TestUsageLearningCountsNotes(t *testing.T) {
	dir := events(t, map[time.Duration][]string{
		5 * time.Hour: {`"event":"prompt","decision":"skip","reason":"too_short","plen":5`},
		time.Hour:     {`"event":"job","name":"tick","status":"done","details":{"notes_sent":2}`},
	})
	if c := byID(checkUsage(dir, usageNow))["learning"]; c.Status != OK {
		t.Errorf("%+v", c)
	}
}
