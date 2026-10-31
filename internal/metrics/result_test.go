package metrics

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
)

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func keyList(m map[string]any) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, " ")
}

func wantKeys(t *testing.T, what string, m map[string]any, want string) {
	t.Helper()
	if got := keyList(m); got != want {
		t.Errorf("%s keys:\n got  %s\n want %s", what, got, want)
	}
}

func sub(t *testing.T, m map[string]any, k string) map[string]any {
	t.Helper()
	v, ok := m[k].(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want an object", k, m[k])
	}
	return v
}

func TestResultJSONNames(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 44, 0, 0, time.UTC)
	w := Window{Since: now.Add(-168 * time.Hour), Until: now, Days: 7, TZ: "UTC", Loc: time.UTC}
	r := Result{
		View: "overview", ViewSchema: ViewSchema, GeneratedAt: now, Window: w,
		Previous: Span{Since: w.Since.Add(-168 * time.Hour), Until: w.Since},
		Scope:    Scope{Machines: "this", Server: "local", ServerOK: true, Project: "alpha"},
		Index:    index.Freshness{IngestedAt: now, LagSeconds: 3, Partial: true},
		Server:   &ServerState{OK: true, LastOK: &now},
		Verdict: &Verdict{WindowDays: 7, Learning: true, Axes: map[string]Axis{
			"found": {State: AxisOK, WindowDays: 7, Reasons: []Reason{{Metric: "m", Severity: SeverityInfo, Text: "t", Command: "c"}}},
		}},
		Summary: &Summary{PromptsWithMemory: 1, TurnsWithMemoryUsed: 2, ContextChars: 3, TokensApprox: 4,
			ExtractionCost: fp(0.5), Currency: "USD", PriceSet: true, ExtractionScope: "server"},
		Metrics: map[string]Metric{"recall.hit_rate": {
			Value: fp(0.36), Num: ip(12), Den: ip(33), Unit: "ratio", Aggregation: "ratio", Status: StatusOK,
			Reason: "r", Borderline: true, Tags: []string{"unreliable"}, CI: &[2]float64{0.22, 0.53}, MDC: fp(0.24),
			Previous: fp(0.3), Delta: fp(0.06), Drill: "quality",
			Series: []Point{{Day: "2026-10-24", Value: fp(1), Num: ip(1), Den: ip(1), N: ip(1), P50: fp(1), P95: fp(2), Parts: map[string]int64{"a": 1}}},
			Extra:  map[string]any{"k": 1},
		}},
		Tables:        Tables{Note: "n"},
		Attention:     []Item{{Metric: "m", Severity: SeverityBad, Axis: "right", Text: "t", Why: "w", Command: "c"}},
		AttentionMore: 2,
		Info:          []InfoItem{{Metric: "m", Count: 4, Command: "c"}},
		Text:          &MemoryText{ID: "i", Text: "x"},
	}
	m := asMap(t, r)
	wantKeys(t, "result", m, "attention attention_more generated_at index info metrics previous scope server summary tables text verdict view view_schema window")
	wantKeys(t, "window", sub(t, m, "window"), "days since tz until")
	wantKeys(t, "previous", sub(t, m, "previous"), "since until")
	wantKeys(t, "scope", sub(t, m, "scope"), "machines project server server_ok")
	wantKeys(t, "index", sub(t, m, "index"), "ingested_at lag_seconds partial")
	wantKeys(t, "server", sub(t, m, "server"), "last_ok ok")
	wantKeys(t, "verdict", sub(t, m, "verdict"), "axes learning window_days")
	axis := sub(t, sub(t, sub(t, m, "verdict"), "axes"), "found")
	wantKeys(t, "axis", axis, "reasons state window_days")
	wantKeys(t, "reason", axis["reasons"].([]any)[0].(map[string]any), "command metric severity text")
	wantKeys(t, "summary", sub(t, m, "summary"), "context_chars currency extraction_cost extraction_scope price_set prompts_with_memory tokens_approx turns_with_memory_used")
	met := sub(t, sub(t, m, "metrics"), "recall.hit_rate")
	wantKeys(t, "metric", met, "aggregation borderline ci delta den drill extra mdc num previous reason series status tags unit value")
	wantKeys(t, "point", met["series"].([]any)[0].(map[string]any), "day den n num p50 p95 parts value")
	wantKeys(t, "item", m["attention"].([]any)[0].(map[string]any), "axis command metric severity text why")
	wantKeys(t, "info item", m["info"].([]any)[0].(map[string]any), "command count metric")
	wantKeys(t, "text", sub(t, m, "text"), "id text")
	if m["view_schema"] != float64(1) {
		t.Errorf("view_schema = %v", m["view_schema"])
	}
	if got := sub(t, m, "window")["since"]; got != "2026-10-23T15:44:00Z" {
		t.Errorf("window.since = %v", got)
	}

	// Unset optionals stay out; the always-present ones stay in as null.
	m = asMap(t, Result{Metrics: map[string]Metric{"x": {}}})
	wantKeys(t, "bare result", m, "attention attention_more generated_at index info metrics previous scope tables view view_schema window")
	wantKeys(t, "bare index", sub(t, m, "index"), "lag_seconds partial")
	bare := sub(t, sub(t, m, "metrics"), "x")
	wantKeys(t, "bare metric", bare, "aggregation borderline ci delta den drill mdc num previous status tags unit value")
	for _, k := range []string{"value", "num", "den", "ci", "mdc", "previous", "delta", "tags"} {
		if bare[k] != nil {
			t.Errorf("bare metric %s = %v, want null", k, bare[k])
		}
	}
	if m["attention"] != nil || m["info"] != nil {
		t.Errorf("nil lists marshal as %v / %v (views normalise them to [])", m["attention"], m["info"])
	}
	wantKeys(t, "bare tables", sub(t, m, "tables"), "")
}

func TestServerStateNullLastOK(t *testing.T) {
	b, err := json.Marshal(ServerState{OK: false})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"ok":false,"last_ok":null}` {
		t.Errorf("got %s", b)
	}
}

func TestTablesRowNames(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	tb := Tables{
		Page: &Page{}, Activity: &Activity{}, Lately: []SessionRow{{}}, OutcomeMix: []OutcomeDay{{ByPath: map[string]OutcomeDay{"a": {}}}},
		Reported: []ReportedRow{{}}, Extraction: []ExtractionRow{{}}, ByProject: []ProjectCount{{}},
		Review:           &Review{NeverUsed: []ReviewRow{{}}, Hidden: []ReviewRow{}, Reported: []ReviewRow{}},
		FailedOperations: []OperationRow{{}}, Jobs: []JobRow{{LastAt: now}}, Doctor: []DoctorRow{{}},
		System:   &SystemInfo{},
		Projects: []ProjectRow{{}}, Pooled: &PooledRow{}, Project: &ProjectPage{},
		Sessions: []SessionRow{{}}, Session: &SessionPage{Turns: []TurnRow{{Shows: []ShowChip{{}}}}},
		Memories: []MemoryRow{{}}, Memory: &MemoryPage{Shows: []ShowMark{{}}, Retirements: []RetireMark{{}}},
		Days: []DayRow{{}}, Day: &DayPage{}, Releases: []ReleaseRow{{}},
		Settings: map[string]float64{"a": 1}, Note: "n",
	}
	m := asMap(t, tb)
	wantKeys(t, "tables", m, "activity by_project day days doctor extraction failed_operations jobs lately memories memory note outcome_mix page pooled project projects releases reported review session sessions settings system")
	first := func(k string) map[string]any { return m[k].([]any)[0].(map[string]any) }
	wantKeys(t, "page", sub(t, m, "page"), "limit offset total")
	wantKeys(t, "activity", sub(t, m, "activity"), "credits machine prompts recalls retains with_hits")
	wantKeys(t, "session row", first("lately"), "context_chars id last project prompts prompts_with_memory sent shown start used wrong")
	wantKeys(t, "outcome day", first("outcome_mix"), "by_path day ignored not_declared pending shown_only used")
	wantKeys(t, "reported", first("reported"), "at memory_id project session turn verdict")
	wantKeys(t, "extraction", first("extraction"), "cached cost day input operation output")
	wantKeys(t, "by project", first("by_project"), "memories project")
	wantKeys(t, "review", sub(t, m, "review"), "hidden never_used reported")
	wantKeys(t, "review row", sub(t, m, "review")["never_used"].([]any)[0].(map[string]any), "command id last_shown project shown")
	wantKeys(t, "operation", first("failed_operations"), "age_ms retries type")
	wantKeys(t, "job", first("jobs"), "age_ms last_at ms name status")
	wantKeys(t, "doctor", first("doctor"), "fix id status")
	wantKeys(t, "system", sub(t, m, "system"), "backup index mode reach runtime server_version update")
	sys := sub(t, m, "system")
	wantKeys(t, "reach", sub(t, sys, "reach"), "breaker_failures")
	wantKeys(t, "backup", sub(t, sys, "backup"), "ok")
	wantKeys(t, "update", sub(t, sys, "update"), "check_failed newer")
	wantKeys(t, "index info", sub(t, sys, "index"), "lag_seconds partial schema")
	wantKeys(t, "project row", first("projects"), "last_activity memories_on_server project prompts prompts_with_memory reported_wrong series sessions used_turns")
	wantKeys(t, "pooled", sub(t, m, "pooled"), "label max min totals")
	wantKeys(t, "project page", sub(t, m, "project"), "attribution_disagreements command never_used no_memories project sessions top_used")
	wantKeys(t, "session page", sub(t, m, "session"), "totals turns")
	turn := sub(t, m, "session")["turns"].([]any)[0].(map[string]any)
	wantKeys(t, "turn", turn, "at check decision declared empty n reason searches shows turn")
	wantKeys(t, "chip", turn["shows"].([]any)[0].(map[string]any), "memory_id outcome path rank")
	wantKeys(t, "memory row", first("memories"), "boost id ignored last_shown last_used project rate shown state used")
	mem := sub(t, m, "memory")
	wantKeys(t, "memory page", mem, "commands created kind rank_histogram retirements row server_state shows")
	wantKeys(t, "show mark", mem["shows"].([]any)[0].(map[string]any), "at outcome path prompt rank rank_without_boost score_without_boost session turn")
	wantKeys(t, "retire mark", mem["retirements"].([]any)[0].(map[string]any), "at kind")
	wantKeys(t, "day row", first("days"), "context_chars day prompts prompts_with_memory recalls sent_failed sent_ok sessions used_turns wrong")
	wantKeys(t, "day page", sub(t, m, "day"), "day sessions")
	wantKeys(t, "release row", first("releases"), "bank_size first flagship last n project_shares prompts_per_day version")
}
