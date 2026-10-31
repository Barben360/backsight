package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The goldens of this file were recorded from b4acd3d, before the metrics
// index (dashboard block 1): the existing `stats` output stays byte for
// byte what it was.
var goldenNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// telLineAt is one telemetry line of e at ts.
func telLineAt(t *testing.T, ts time.Time, e telemetry.Event) string {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["ts"], m["event"], m["session"], m["project"] = ts.UTC().Format("2006-01-02T15:04:05.000Z"), e.EventName(), "s1", "p1"
	out, _ := json.Marshal(m)
	return string(out) + "\n"
}

// fixedStatsFixture writes a fixed telemetry and journal fixture and
// freezes the clock.
func fixedStatsFixture(t *testing.T) {
	t.Helper()
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	llmInput.Store(2_000_000)
	statsDown.Store(true)
	old := now
	now = func() time.Time { return goldenNow }
	t.Cleanup(func() { now = old })
	at := func(h int) time.Time { return goldenNow.Add(-time.Duration(h) * time.Hour) }
	var b strings.Builder
	for i, e := range []telemetry.Event{
		telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, MS: 1200, Injected: 1, Lang: "fr", Scopes: map[string]telemetry.ScopeResult{"project": {OK: true}}},
		telemetry.Prompt{N: 2, Decision: "recall", Plen: 300, MS: 800, Injected: 0, Lang: "en"},
		telemetry.Prompt{N: 3, Decision: "skip", Reason: "short", Plen: 5},
		telemetry.Raw{Name: "tool_use", Fields: map[string]any{"name": "mcp__plugin_backsight_memory__memory_search", "n": 1}},
		telemetry.Outcome{N: 1, IDs: []string{"m1"}, Searched: true},
		telemetry.Credit{Verdict: "used", IDs: 1, OK: true},
		telemetry.CreditCheck{Outcome: "declared"},
		telemetry.FeedbackSettle{Shows: 4, Credits: 1, Misses: 1},
		telemetry.Retain{Document: "d1", Items: 2, Status: "ok", MS: 300},
	} {
		b.WriteString(telLineAt(t, at(30-i), e))
	}
	dir := pathsOf(home).TelemetryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events-2026-10.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	journalIn(t, home,
		feedback.Event{Kind: "shows", Path: "prompt", TS: at(20), Items: []feedback.Item{{ID: "m1", Rank: 1}}, Flags: []string{feedback.FlagSetChanged}},
		feedback.Event{Kind: feedback.KindRecall, Path: "search", TS: at(10), Items: []feedback.Item{{ID: "m1", Rank: 1}}},
	)
}

var goldenPort = regexp.MustCompile(`127\.0\.0\.1:\d+`)

// TestStatsOutputIsUnchanged pins every `stats` output the metrics index
// must not move.
func TestStatsOutputIsUnchanged(t *testing.T) {
	fixedStatsFixture(t)
	cases := []struct {
		name string
		args []string
	}{
		{"stats-default.golden", []string{"stats"}},
		{"stats-14.golden", []string{"stats", "14"}},
		{"stats-14-json.golden", []string{"stats", "14", "--json"}},
		{"stats-window-parity.golden", []string{"stats", "--since", "2026-10-01", "--until", "2026-10-08", "--parity"}},
		{"stats-window-parity-json.golden", []string{"stats", "--since", "2026-10-01", "--until", "2026-10-08", "--parity", "--json"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := run(t, c.args...)
			if code != output.OK {
				t.Fatalf("exit %d: %s %s", code, out, errOut)
			}
			cliGolden(t, c.name, []byte(goldenPort.ReplaceAllString(out, "127.0.0.1:PORT")))
		})
	}
}
