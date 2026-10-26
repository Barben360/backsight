package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/guide"
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

// injections are n recalls that injected a memory, of which searched were
// followed by a memory_search in their turn.
func injections(searched, n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf(`"event":"outcome","n":%d,"ids":["m"]`, i))
		if i <= searched {
			out = append(out, fmt.Sprintf(`"event":"tool_use","name":"x__memory_search","n":%d`, i))
		}
	}
	return out
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
	cs := checkUsage(t.TempDir(), usageNow, false)
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
		3 * time.Hour: append(append(repeat(18, recall), repeat(2, failing)...), `"event":"retain","status":"retry"`,
			`"event":"transcript_unknown","types":{"new-kind":5}`),
		2 * day: injections(1, 10),
		9 * day: append(injections(6, 10), `"event":"transcript_unknown","types":{"new-kind":1}`),
	})
	got := byID(checkUsage(dir, usageNow, false))
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
		24 * time.Hour * 2: injections(10, 10),
		24 * time.Hour * 9: append(injections(10, 10), `"event":"transcript_unknown","types":{"x":4}`),
		24 * time.Hour * 15: { // outside both weeks
			`"event":"transcript_unknown","types":{"y":9}`},
	})
	got := byID(checkUsage(dir, usageNow, false))
	for _, id := range []string{"recall", "latency", "learning", "searches", "transcripts"} {
		if got[id].Status != OK {
			t.Errorf("%s: %s (%s)", id, got[id].Status, got[id].Summary)
		}
	}
	// 2 failures out of 21 calls is over 5%
	dir = events(t, map[time.Duration][]string{time.Hour: append(repeat(19, recall), failing, failing)})
	if c := byID(checkUsage(dir, usageNow, false))["recall"]; c.Status != Warn {
		t.Errorf("recall: %+v", c)
	}
}

// Notes sent by a tick count as learning too.
func TestUsageLearningCountsNotes(t *testing.T) {
	dir := events(t, map[time.Duration][]string{
		5 * time.Hour: {`"event":"prompt","decision":"skip","reason":"too_short","plen":5`},
		time.Hour:     {`"event":"job","name":"tick","status":"done","details":{"notes_sent":2}`},
	})
	if c := byID(checkUsage(dir, usageNow, false))["learning"]; c.Status != OK {
		t.Errorf("%+v", c)
	}
}

// Run includes the usage checks; a usage warning counts as a warning, never
// as an error (doctor exits non-zero on errors only).
func TestRunIncludesTheUsageChecks(t *testing.T) {
	f := newFixture(t)
	f.env.Now = func() time.Time { return usageNow }
	tdir := f.env.Dirs.TelemetryDir()
	_ = os.MkdirAll(tdir, 0o700)
	_ = os.WriteFile(filepath.Join(tdir, "events-2026-10.jsonl"),
		[]byte(fmt.Sprintf(`{"ts":%q,"event":"retain","status":"retry"}`+"\n", usageNow.Add(-5*time.Hour).Format(time.RFC3339))), 0o600)
	r := f.run()
	c := check(t, r, "learning")
	if c.Status != Warn {
		t.Fatalf("learning: %+v", c)
	}
	errorsWithout := 0
	for _, c := range r.Checks {
		if c.Status == Error {
			errorsWithout++
		}
	}
	if r.Errors != errorsWithout || r.Warnings == 0 {
		t.Errorf("errors %d warnings %d", r.Errors, r.Warnings)
	}
}

// Below each minimum sample, the check only informs: a quiet week is not a
// failure.
func TestUsageBelowTheMinimumsOnlyInforms(t *testing.T) {
	failing := `"event":"prompt","decision":"recall","plen":80,"ms":9000,"scopes":{"project":{"ok":false}}`
	dir := events(t, map[time.Duration][]string{
		time.Hour:          repeat(19, failing), // 19 calls, all failed, all slow
		24 * time.Hour * 2: injections(0, 9),
		24 * time.Hour * 9: injections(10, 10),
	})
	got := byID(checkUsage(dir, usageNow, false))
	for _, id := range []string{"recall", "latency", "searches"} {
		if got[id].Status != Info {
			t.Errorf("%s: %s (%s)", id, got[id].Status, got[id].Summary)
		}
	}
}

// The weeks split at exactly 7 days: an event 1 s inside this week counts
// in it, one 1 s before does not.
func TestUsageWeekEdge(t *testing.T) {
	week := 7 * 24 * time.Hour
	dir := events(t, map[time.Duration][]string{
		time.Hour:          {`"event":"gate","decision":"allow"`},
		week - time.Second: {`"event":"transcript_unknown","types":{"k":2}`},
		week + time.Second: {`"event":"transcript_unknown","types":{"k":1}`},
	})
	if c := byID(checkUsage(dir, usageNow, false))["transcripts"]; !strings.Contains(c.Summary, "2 unknown transcript records this week (1 last week)") {
		t.Errorf("%+v", c)
	}
}

func TestUsageComparesWithLastWeek(t *testing.T) {
	day := 24 * time.Hour
	searched := injections
	cases := []struct {
		name       string
		this, last []string
		id         string
		want       Status
	}{
		{"unknown records not rising", []string{`"event":"transcript_unknown","types":{"k":2}`}, []string{`"event":"transcript_unknown","types":{"k":2}`}, "transcripts", Info},
		{"one more record is noise", []string{`"event":"transcript_unknown","types":{"k":4}`}, []string{`"event":"transcript_unknown","types":{"k":3}`}, "transcripts", Info},
		{"doubled but under 5", []string{`"event":"transcript_unknown","types":{"k":4}`}, []string{`"event":"transcript_unknown","types":{"k":2}`}, "transcripts", Info},
		{"doubled and at least 5", []string{`"event":"transcript_unknown","types":{"k":6}`}, []string{`"event":"transcript_unknown","types":{"k":3}`}, "transcripts", Warn},
		{"a new type", []string{`"event":"transcript_unknown","types":{"j":1}`}, []string{`"event":"transcript_unknown","types":{"k":3}`}, "transcripts", Warn},
		{"low but stable search rate", searched(2, 10), searched(2, 10), "searches", OK},
		{"exactly half", searched(3, 10), searched(6, 10), "searches", OK},
		{"under half", searched(2, 10), searched(6, 10), "searches", Warn},
	}
	for _, c := range cases {
		dir := events(t, map[time.Duration][]string{2 * day: c.this, 9 * day: c.last})
		if got := byID(checkUsage(dir, usageNow, false))[c.id]; got.Status != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got.Status, got.Summary, c.want)
		}
	}
}

func TestUsageLearningFilters(t *testing.T) {
	prompt := `"event":"prompt","decision":"skip","reason":"too_short","plen":5`
	machine := `"event":"prompt","decision":"skip","reason":"machine","plen":5`
	refused := `"event":"retain","status":"quarantined"`
	cases := []struct {
		name   string
		events map[time.Duration][]string
		want   Status
	}{
		{"prompts only in the last 2 h: not settled yet", map[time.Duration][]string{time.Hour: {prompt}}, Info},
		{"prompts older than 24 h", map[time.Duration][]string{30 * time.Hour: {prompt}, time.Hour: {`"event":"gate","decision":"allow"`}}, Info},
		{"machine messages are no session", map[time.Duration][]string{5 * time.Hour: {machine}}, Info},
		{"prompts and no send yet: sessions go out when they end", map[time.Duration][]string{5 * time.Hour: {prompt}}, Info},
		{"an accepted retain older than 24 h does not count", map[time.Duration][]string{5 * time.Hour: {refused}, 30 * time.Hour: {`"event":"retain","status":"ok"`}}, Warn},
		{"a tick that sent no note does not count", map[time.Duration][]string{5 * time.Hour: {refused}, time.Hour: {`"event":"job","name":"tick","details":{"notes_sent":0}`}}, Warn},
		{"a refused send older than 24 h does not count", map[time.Duration][]string{30 * time.Hour: {refused}, time.Hour: {prompt}}, Info},
		{"one accepted send is learning", map[time.Duration][]string{5 * time.Hour: {refused}, time.Hour: {`"event":"retain","status":"ok"`}}, OK},
	}
	for _, c := range cases {
		if got := byID(checkUsage(events(t, c.events), usageNow, false))["learning"]; got.Status != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got.Status, got.Summary, c.want)
		}
	}
}

func TestUsageLearningWithWritesOff(t *testing.T) {
	dir := events(t, map[time.Duration][]string{time.Hour: {`"event":"retain","status":"retry"`}})
	if c := byID(checkUsage(dir, usageNow, true))["learning"]; c.Status != Info || !strings.Contains(c.Summary, "components.write") {
		t.Errorf("%+v", c)
	}
}

// A note or report put in the outbox ("queued") is the normal path, not a
// refused send.
func TestUsageQueuedOutboxNotesAreNotRefusals(t *testing.T) {
	dir := events(t, map[time.Duration][]string{5 * time.Hour: {`"event":"prompt","decision":"skip","reason":"too_short","plen":5`, `"event":"retain","status":"queued"`}})
	if c := byID(checkUsage(dir, usageNow, false))["learning"]; c.Status == Warn {
		t.Errorf("%+v", c)
	}
}

// A slow recall's fix points at the docs section that explains what helps
// (0067 block 4): a GPU alone changes nothing in this version, so the fix
// must not offer one.
func TestLatencyFixPointsAtTheGPUSection(t *testing.T) {
	recall := `"event":"prompt","decision":"recall","plen":80,"ms":3000,"scopes":{"project":{"ok":true}}`
	c := byID(checkUsage(events(t, map[time.Duration][]string{time.Hour: repeat(20, recall)}), usageNow, false))["latency"]
	const ref = "troubleshooting#slow-recall-and-gpu-acceleration"
	if c.Status != Warn || !strings.Contains(c.Fix, "backsight guide "+ref) || strings.Contains(c.Fix, "a GPU for the reranker") {
		t.Fatalf("%+v", c)
	}
	corpus, err := guide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range corpus.Topics {
		for _, s := range tp.Sections {
			if s.Ref() == ref {
				return
			}
		}
	}
	t.Errorf("the docs have no section %s", ref)
}

// The attribution check reads the shadow answers of main turns and the
// moves of subagent reports from the week's telemetry.
func TestUsageShowsThePathRulesDisagreement(t *testing.T) {
	h := time.Hour
	dir := events(t, map[time.Duration][]string{h: {
		`"event":"attribution","turn":"u1","subagent":false,"base":"billing","applied":"billing","path_rule":"shop","source":"evidence"`,
		`"event":"attribution","turn":"u2","subagent":false,"base":"billing","applied":"billing","path_rule":"billing","source":"cwd"`,
		`"event":"attribution","turn":"u3","subagent":false,"base":"billing","applied":"billing","path_rule":"billing","source":"cwd"`,
		`"event":"attribution","turn":"u4","subagent":false,"base":".claude","applied":".claude","path_rule":"shop","source":"evidence"`,
		`"event":"attribution","turn":"agent:a1","subagent":true,"base":"billing","applied":"shop","path_rule":"shop","source":"evidence"`,
		`"event":"attribution","turn":"agent:a2","subagent":true,"base":"billing","applied":"billing","path_rule":"billing","source":"cwd"`,
	}, 9 * 24 * h: {
		`"event":"attribution","turn":"old","subagent":false,"base":"billing","applied":"billing","path_rule":"shop","source":"evidence"`,
	}})
	var c Check
	for _, x := range checkUsage(dir, usageNow, false) {
		if x.ID == "attribution" {
			c = x
		}
	}
	if c.Status != Info || !strings.Contains(c.Summary, "2 of 4 main turns elsewhere (50%") || !strings.Contains(c.Summary, "1 of 2 subagent reports") {
		t.Errorf("%+v", c)
	}
	if strings.Contains(c.Summary, "shop") || strings.Contains(c.Summary, "billing") {
		t.Errorf("a project name in the check: %q", c.Summary)
	}
}

// doctor lists the owners learnt, without the denied ones, and says how
// to deny one.
func TestOwnersCheckListsWhatWasLearnt(t *testing.T) {
	f := newFixture(t)
	if c := checkOwners(f.env); c.Status != Info || !strings.Contains(c.Summary, "no repository owner learnt") {
		t.Errorf("%+v", c)
	}
	_ = os.MkdirAll(f.env.Dirs.AttributionDir(), 0o700)
	_ = os.WriteFile(filepath.Join(f.env.Dirs.AttributionDir(), "owners.json"), []byte(`["github.com/me","github.com/ohmyzsh"]`), 0o600)
	put(t, f.env.Dirs.ConfigFile(), "[projects]\ndeny_owners = [\"github.com/OhMyZsh\"]\n", 0o600)
	c := checkOwners(f.env)
	if c.Status != Info || !strings.HasSuffix(c.Summary, ": github.com/me") || !strings.Contains(c.Fix, "projects.deny_owners") || len(c.Projects) != 1 {
		t.Errorf("%+v", c)
	}
}
