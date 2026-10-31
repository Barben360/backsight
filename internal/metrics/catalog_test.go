package metrics

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// catalogRow is a Def as the product's table states it, typed out once more
// here so a typo in either copy goes red. watch and bad are NaN when absent.
type catalogRow struct {
	id, label, unit, dir string
	minN                 int
	watch, bad           float64
	on                   string
	rel, err             bool
	ci, scope            string
	dev                  bool
	family, series       string
	drill                string
	extra                string // comma-separated
}

const none = -1.0 // no threshold value

var catalogRows = []catalogRow{
	{"recall.hit_rate", "Prompts that got memory", "ratio", "up", 20, 0.10, none, "hi", true, false, "wilson", "machine", false, "reach", "daily", "backsight stats sessions --has memory", ""},
	{"recall.empty_reasons", "Why prompts got nothing", "count", "none", 0, none, none, "", false, false, "none", "machine", false, "reach", "daily", "backsight stats quality", "empty,skips"},
	{"recall.by_lang", "Prompts that got memory, by language", "ratio", "up", 20, none, none, "", false, false, "wilson", "machine", true, "reach", "none", "backsight stats quality --dev", "by_lang"},
	{"search.rate", "Prompts where Claude searched memory", "ratio", "up", 20, none, none, "", true, false, "wilson", "machine", false, "reach", "daily", "backsight stats quality", ""},
	{"search.empty_rate", "Searches that found nothing", "ratio", "down", 20, none, none, "", true, false, "wilson", "machine", false, "reach", "daily", "backsight stats quality", "failed"},
	{"used.turn_rate", "Turns where Claude used memory", "ratio", "up", 20, none, none, "", true, false, "wilson", "machine", false, "quality", "daily", "backsight stats sessions --has used", "worst_case,pending,check_skipped,subagent_turns"},
	{"declare.coverage", "Turns where Claude said what it used", "ratio", "up", 20, 0.80, none, "hi", false, false, "wilson", "machine", false, "quality", "daily", "backsight stats sessions --has undeclared", "pending,check_skipped"},
	{"used.rubber_stamp", "Turns where every memory was marked used", "ratio", "down", 30, 0.80, none, "lo", false, false, "wilson", "machine", true, "quality", "none", "backsight stats quality --dev", ""},
	{"used.share_confident", "Confidently ranked memories that were used", "ratio", "up", 20, none, none, "", false, false, "none", "machine", true, "quality", "daily", "backsight stats quality --dev", "unknown,shown,forgotten,pending,path"},
	{"used.rank", "Position of the memories used", "count", "none", 20, none, none, "", false, false, "none", "machine", true, "quality", "none", "backsight stats quality --dev", "histogram,hit_at_1,hit_at_3"},
	{"used.by_path", "Used, by where memory was shown", "ratio", "up", 20, none, none, "", false, false, "none", "machine", true, "quality", "none", "backsight stats quality --dev", "by_path"},
	{"search.rescue_rate", "Memories Claude found itself", "ratio", "none", 20, none, none, "", false, false, "none", "machine", true, "quality", "none", "backsight stats quality --dev", ""},
	{"verdict.wrong_rate", "Memories reported wrong or out of date", "ratio", "down", 20, 0.05, 0.15, "lo", false, false, "wilson_approx", "machine", false, "quality", "daily", "backsight stats sessions --has wrong", "wrong,outdated,used"},
	{"stale.never_used", "Often shown, never used", "count", "none", 0, none, none, "", false, false, "none", "machine", false, "quality", "none", "backsight memory stale", ""},
	{"stale.hidden", "Hidden", "count", "none", 0, none, none, "", false, false, "none", "machine", false, "quality", "none", "backsight memory stale", ""},
	{"stale.reported", "Reported wrong", "count", "none", 0, none, none, "", false, false, "none", "machine", false, "quality", "none", "backsight memory stale", ""},
	{"context.per_session", "Context added per session", "chars", "down", 100, none, none, "p95", false, false, "none", "machine", false, "cost", "daily", "backsight stats sessions", "p50,p95,tokens_p50,tokens_p95,histogram"},
	{"context.per_prompt", "Context added per prompt that got memory", "chars", "down", 20, none, none, "", false, false, "none", "machine", false, "cost", "none", "backsight stats cost", "p50,p95"},
	{"recall.latency", "Time memory adds to a prompt", "ms", "down", 100, 2000, 2500, "p95", false, false, "none", "machine", false, "cost", "daily", "backsight stats days", "p50,p95"},
	{"search.latency", "Time of a memory search", "ms", "down", 100, 3000, none, "p95", false, false, "none", "machine", false, "cost", "daily", "backsight doctor", "p50,p95"},
	{"extraction.tokens", "What the server's model spent", "tokens", "none", 0, none, none, "", false, false, "none", "server", false, "cost", "daily", "backsight stats cost", "input,cached,output,by_operation,days_covered"},
	{"extraction.cost", "What the server's model spent", "money", "none", 0, none, none, "", false, false, "none", "server", false, "cost", "daily", "backsight stats cost", "price_set,currency,by_operation"},
	{"extraction.per_session", "Extraction per session sent", "tokens", "none", 0, none, none, "", false, false, "none", "server", true, "cost", "none", "backsight stats cost --dev", "sessions_sent,window_days"},
	{"bank.size", "Memories", "count", "none", 0, none, none, "", false, false, "none", "server", false, "bank", "none", "backsight stats bank", "world,experience,observation,documents,links"},
	{"bank.growth", "New memories per day", "count", "none", 0, none, none, "", false, false, "none", "server", false, "bank", "daily", "backsight stats bank", "world,experience,observation,days_covered"},
	{"bank.by_project", "Memories per project", "count", "none", 0, none, none, "", false, false, "none", "server", false, "bank", "none", "backsight stats projects", ""},
	{"bank.consolidation", "Summaries waiting", "count", "none", 0, none, none, "", false, true, "none", "server", false, "bank", "none", "backsight doctor", "pending,failed,last_consolidated_at"},
	{"bank.churn", "Retired, hidden, restored", "count", "none", 0, none, none, "", false, false, "none", "machine", false, "bank", "weekly", "backsight memory stale", "forget,retire,hide,unhide"},
	{"notes.stored", "Facts stored", "count", "none", 0, none, none, "", false, false, "none", "machine", false, "bank", "none", "backsight stats bank", "by_result,by_kind"},
	{"pipeline.flow", "Sessions sent", "ratio", "up", 0, none, none, "", false, true, "wilson", "machine", false, "pipeline", "daily", "backsight stats pipeline", "ok,failed,set_aside,waiting,backlog_oldest_ms"},
	{"retain.outcomes", "Sends by result", "count", "none", 0, none, none, "", false, false, "none", "machine", false, "pipeline", "daily", "backsight stats pipeline", "by_status,p95_ms"},
	{"local.backlog", "Waiting on this PC", "count", "none", 0, none, none, "", false, true, "none", "machine", false, "pipeline", "none", "backsight jobs tick", "notes_queued,sessions_waiting,quarantined,oldest_ms"},
	{"server.queue", "Waiting on the server", "count", "none", 0, none, none, "", false, true, "none", "server", false, "pipeline", "none", "backsight doctor", "pending,processing,failed,failed_total"},
	{"server.extraction_delay", "Time to turn a session into memories", "ms", "down", 100, 1800000, none, "p95", false, false, "none", "server", false, "pipeline", "none", "backsight doctor", "p50,p95"},
	{"hooks.failures", "Steps that crashed", "count", "none", 0, none, none, "", false, true, "none", "machine", false, "pipeline", "daily", "backsight doctor", "by_kind,last_24h"},
	{"server.reach", "Memory server answering", "count", "none", 0, none, none, "", false, true, "none", "machine", false, "pipeline", "none", "backsight doctor", "last_ok,failing_since,breaker_open_until"},
	{"doctor.check", "Health checks", "count", "none", 0, none, none, "", false, true, "none", "machine", false, "pipeline", "none", "backsight doctor", "ok,info,warn,error"},
	{"jobs.last_run", "Background jobs", "ms", "none", 0, none, none, "", false, true, "none", "machine", false, "pipeline", "none", "backsight jobs tick", "tick,maintain"},
}

// A typo in any value of any row goes red: each Def is read back against the
// row stated above, in order.
func TestCatalogRows(t *testing.T) {
	if len(catalogRows) != 38 || len(Catalog) != 38 {
		t.Fatalf("rows: table %d, catalog %d, want 38 each", len(catalogRows), len(Catalog))
	}
	num := func(p *float64) float64 {
		if p == nil {
			return none
		}
		return *p
	}
	for i, w := range catalogRows {
		d := Catalog[i]
		var extra string
		if len(d.Extra) > 0 {
			extra = strings.Join(d.Extra, ",")
		}
		got := catalogRow{d.ID, d.Label, d.Unit, d.Direction, d.MinN, num(d.Absolute.Watch), num(d.Absolute.Bad), d.Absolute.On,
			d.Relative, d.ErrorClass, d.CI, d.Scope, d.Dev, d.Family, d.Series, d.Drill, extra}
		if got != w {
			t.Errorf("row %d (%s):\n got  %+v\n want %+v", i+1, w.id, got, w)
		}
		if d.Aggregation != "raw" && !strings.HasPrefix(d.ID, "stale.") {
			t.Errorf("%s: aggregation %q, want raw", d.ID, d.Aggregation)
		}
		if strings.HasPrefix(d.ID, "stale.") && d.Aggregation != "recent_weighted" {
			t.Errorf("%s: aggregation %q, want recent_weighted", d.ID, d.Aggregation)
		}
	}
	if _, ok := DefOf("nope"); ok {
		t.Error("DefOf(nope) found a row")
	}
	for _, w := range catalogRows {
		if d, ok := DefOf(w.id); !ok || d.ID != w.id {
			t.Errorf("DefOf(%s) = %v, %v", w.id, d.ID, ok)
		}
	}
}

// Every id is unique and every row carries its text and source.
func TestCatalogRowsAreComplete(t *testing.T) {
	seen := map[string]bool{}
	sources := []string{"telemetry", "journal", "ledger", "local", "server", "doctor"}
	for _, d := range Catalog {
		if seen[d.ID] {
			t.Errorf("duplicate id %s", d.ID)
		}
		seen[d.ID] = true
		for name, v := range map[string]string{"question": d.Question, "definition": d.Definition, "action": d.Action, "command": d.Command, "drill": d.Drill} {
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s: empty %s", d.ID, name)
			}
		}
		if !slices.Contains(sources, d.Source) {
			t.Errorf("%s: source %q", d.ID, d.Source)
		}
		if (d.Scope == "server") != (d.Source == "server") {
			t.Errorf("%s: scope %q but source %q", d.ID, d.Scope, d.Source)
		}
	}
}

// The words the product's own text may not carry (a copy of the cli helper's
// regexes: the design records, plan blocks and spec sections are not public).
var privateRefs = []struct {
	what string
	re   *regexp.Regexp
}{
	{"a design-record number", regexp.MustCompile(`(?i)\b(?:decisions?|plan) \d{4}\b|\b00\d{2}\b`)},
	{"a plan's block", regexp.MustCompile(`(?i)\bblocks? \d+\b`)},
	{"a spec section", regexp.MustCompile(`\bSPEC\b|§`)},
	{"an experiment, review or milestone code", regexp.MustCompile(`\b[CEM]\d{1,2}\b`)},
	{"the roadmap or a review round", regexp.MustCompile(`(?i)\bROADMAP\b|\bblock review\b|\bsecurity review [A-Z]\d`)},
	{`"tranche"`, regexp.MustCompile(`(?i)\btranches?\b`)},
	{"an issue number without its URL", regexp.MustCompile(`(?i)\bissues? #?\d+\b`)},
}

func privateRefsIn(text string) string {
	var found []string
	for _, r := range privateRefs {
		for _, m := range r.re.FindAllString(text, -1) {
			found = append(found, fmt.Sprintf("%s %q", r.what, m))
		}
	}
	return strings.Join(found, "; ")
}

func TestPrivateRefsHelperBites(t *testing.T) {
	for _, s := range []string{"see decision 0079", "block 2", "SPEC", "§4", "E7", "issue #12"} {
		if privateRefsIn(s) == "" {
			t.Errorf("%q not caught", s)
		}
	}
	if got := privateRefsIn("Prompts that got memory, 2,000 ms"); got != "" {
		t.Errorf("plain text flagged: %s", got)
	}
}

func TestCatalogText(t *testing.T) {
	internal := []string{"credited", "confident miss", "consolidation", "quarantine", "backfill", "boost", "retain", "settle", "p50", "p95", "hook failure"}
	for _, d := range Catalog {
		for name, v := range map[string]string{"label": d.Label, "question": d.Question, "definition": d.Definition, "action": d.Action} {
			if r := privateRefsIn(v); r != "" {
				t.Errorf("%s %s cites %s", d.ID, name, r)
			}
		}
		if !strings.HasSuffix(d.Question, "?") {
			t.Errorf("%s: question %q does not end with ?", d.ID, d.Question)
		}
		for _, w := range internal {
			if strings.Contains(strings.ToLower(d.Question), w) {
				t.Errorf("%s: question names the internal term %q", d.ID, w)
			}
		}
		if !strings.HasPrefix(d.Command, "backsight ") || !strings.HasPrefix(d.Drill, "backsight ") {
			t.Errorf("%s: command %q or drill %q does not start with backsight", d.ID, d.Command, d.Drill)
		}
	}
	// A row the table gives no drill for drills to its command.
	for _, id := range []string{"search.rate", "bank.size", "jobs.last_run"} {
		d, _ := DefOf(id)
		if d.Drill != d.Command {
			t.Errorf("%s: drill %q, want its command %q", id, d.Drill, d.Command)
		}
	}
	// And the plain-words additions the table asks for.
	for id, want := range map[string]string{
		"pipeline.flow":      "documents",
		"server.queue":       "last 7 days",
		"bank.consolidation": "current total",
		"context.per_prompt": "p50",
	} {
		d, _ := DefOf(id)
		if !strings.Contains(d.Definition, want) {
			t.Errorf("%s: definition lacks %q", id, want)
		}
	}
}

func TestOnlyTheListedMetricsAreRelative(t *testing.T) {
	var rel, errc []string
	for _, d := range Catalog {
		if d.Relative {
			rel = append(rel, d.ID)
		}
		if d.ErrorClass {
			errc = append(errc, d.ID)
		}
	}
	slices.Sort(rel)
	slices.Sort(errc)
	wantRel := []string{"recall.hit_rate", "search.empty_rate", "search.rate", "used.turn_rate"}
	wantErr := []string{"bank.consolidation", "doctor.check", "hooks.failures", "jobs.last_run", "local.backlog", "pipeline.flow", "server.queue", "server.reach"}
	if !slices.Equal(rel, wantRel) {
		t.Errorf("relative = %v, want %v", rel, wantRel)
	}
	if !slices.Equal(errc, wantErr) {
		t.Errorf("error class = %v, want %v", errc, wantErr)
	}
}

// A definitions row reads as the catalog entry with n and mdc beside it.
func TestDefRowJSON(t *testing.T) {
	d, _ := DefOf("recall.hit_rate")
	m := 0.2
	b, err := json.Marshal(DefRow{Def: d, N: 33, MDC: &m})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"id": "recall.hit_rate", "n": 33.0, "mdc": 0.2, "min_n": 20.0, "relative": true, "ci": "wilson"} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if a, _ := got["absolute"].(map[string]any); a["on"] != "hi" || a["watch"] != 0.10 {
		t.Errorf("absolute = %v", got["absolute"])
	}
}
