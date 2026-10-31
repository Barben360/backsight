package index

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

// common is what the writer adds to every line.
var common = map[string]any{"session": "s1", "project": "proj", "binary_version": "1.2.3", "v": "v10"}

func withCommon(f map[string]any) map[string]any {
	m := map[string]any{}
	for k, v := range common {
		m[k] = v
	}
	for k, v := range f {
		m[k] = v
	}
	return m
}

// wantCommon is the common columns of a typed row.
func wantCommon(w map[string]any) map[string]any {
	m := map[string]any{"ts": ms0, "session": "s1", "project": "proj", "binary_version": "1.2.3", "v": "v10"}
	for k, v := range w {
		m[k] = v
	}
	return m
}

type typedCase struct {
	event  string
	fields map[string]any
	table  string
	want   map[string]any // every column but source_file and offset
}

var typedCases = []typedCase{
	{"prompt", map[string]any{
		"n": 4, "decision": "recall", "reason": "gap", "plen": 42, "lang": "fr",
		"scopes": map[string]any{"user": map[string]any{"ok": true, "ms": 12, "raw": 5}, "global": map[string]any{"ok": false, "ms": 30, "raw": 0}},
		"above":  3, "injected": 2, "ids": []string{"m1", "m2", "m3"}, "top": 0.75, "ms": 120,
		"nudged": true, "hinted": false, "detector": "rule", "chars": 900, "empty": "below_floor",
	}, "tel_prompt", map[string]any{
		"n": 4, "decision": "recall", "reason": "gap", "plen": 42, "lang": "fr", "above": 3, "injected": 2,
		"ids_n": 3, "top": 0.75, "ms": 120, "nudged": true, "hinted": false, "detector": "rule", "chars": 900, "empty": "below_floor",
	}},
	{"prompt", map[string]any{"decision": "skip", "reason": "machine", "plen": 10, "ids": []string{}, "detector": nil},
		"tel_prompt", map[string]any{
			"n": nil, "decision": "skip", "reason": "machine", "plen": 10, "lang": nil, "above": nil, "injected": nil,
			"ids_n": 0, "top": nil, "ms": nil, "nudged": nil, "hinted": nil, "detector": nil, "chars": nil, "empty": nil,
		}},
	{"outcome", map[string]any{"n": 3, "ids": []string{"m1"}, "searched": true, "credited": "exact", "credit_source": "tool", "contradicted": false},
		"tel_outcome", map[string]any{"n": 3, "ids_n": 1, "searched": true, "credited": "exact", "credit_source": "tool", "contradicted": false}},
	{"outcome", map[string]any{"n": 7, "ids": []string{}, "searched": false, "credited": nil, "contradicted": true},
		"tel_outcome", map[string]any{"n": 7, "ids_n": 0, "searched": false, "credited": nil, "credit_source": nil, "contradicted": true}},
	{"reply", map[string]any{"n": 5, "credit": "free", "searched": false},
		"tel_reply", map[string]any{"n": 5, "credit": "free", "searched": false}},
	{"reply", map[string]any{"credit": "", "searched": true}, // silent reply, no n
		"tel_reply", map[string]any{"n": nil, "credit": nil, "searched": true}},
	{"briefing", map[string]any{"ids": []string{"b1", "b2"}, "chars": 300, "source": "raw"},
		"tel_briefing", map[string]any{"ids_n": 2, "chars": 300, "source": "raw"}},
	{"briefing_outcome", map[string]any{"used_ids": []string{"b1"}},
		"tel_briefing_outcome", map[string]any{"used_n": 1}},
	{"tool", map[string]any{"name": "memory_search", "ms": 80, "hits": 4, "ok": true, "live": "stale"},
		"tel_tool", map[string]any{"name": "memory_search", "ms": 80, "hits": 4, "ok": true, "live": "stale"}},
	{"tool_use", map[string]any{"name": "mcp__x__memory_search", "ids": 2, "memory_ids": []string{"m1", "m2"}, "n": 6, "subagent": true},
		"tel_tool_use", map[string]any{"n": 6, "name": "mcp__x__memory_search", "ids_n": 2, "subagent": true}},
	{"credit", map[string]any{"verdict": "used", "ids_n": 2, "ok": true},
		"tel_credit", map[string]any{"verdict": "used", "ids_n": 2, "ok": true}},
	{"credit_check", map[string]any{"outcome": "declared", "shown": 3, "flags": []string{"zeta", "alpha"}, "after_block": true},
		"tel_credit_check", map[string]any{"outcome": "declared", "shown": 3, "flags": "alpha,zeta", "after_block": true}},
	{"retire", map[string]any{"action": "hidden", "ids_n": 1},
		"tel_retire", map[string]any{"action": "hidden", "ids_n": 1}},
	{"feedback_settle", map[string]any{"shows": 5, "credits": 2, "misses": 1, "unknown": 2, "late": 1},
		"tel_feedback_settle", map[string]any{"shows": 5, "credits": 2, "misses": 1, "unknown": 2, "late": 1}},
	{"feedback_error", map[string]any{"error": "disk full"},
		"tel_feedback_error", map[string]any{"reason": "other"}},
	{"gate", map[string]any{"n": 2, "decision": "deny", "reason": "no_search"},
		"tel_gate", map[string]any{"n": 2, "decision": "deny", "reason": "no_search"}},
	{"retain", map[string]any{"document": "conversation:s1", "items": 1, "status": "queued", "ms": 15},
		"tel_retain", map[string]any{"document": "conversation:s1", "items": 1, "status": "queued", "ms": 15}},
	{"retain", map[string]any{"document": "d2", "items": 1, "status": "retry", "ms": 9},
		"tel_retain", map[string]any{"document": "d2", "items": 1, "status": "retry", "ms": 9}},
	{"hook_failure", map[string]any{"hook": "user-prompt-submit", "kind": "timeout", "ms": 5000},
		"tel_hook_failure", map[string]any{"hook": "user-prompt-submit", "kind": "timeout", "ms": 5000}},
	{"job", map[string]any{"name": "tick", "status": "partial", "ms": 900, "details": map[string]any{"sent": 3}},
		"tel_job", map[string]any{"name": "tick", "status": "partial", "ms": 900}},
	// turn is an id: "0123" stays text, its leading zero kept.
	{"attribution", map[string]any{"turn": "0123", "subagent": false, "base": "proj", "applied": "proj", "path_rule": "other-proj", "source": "cwd"},
		"tel_attribution", map[string]any{"turn": "0123", "subagent": false, "base": "proj", "applied": "proj", "path_rule": "other-proj", "source": "cwd"}},
	{"attribution", map[string]any{"turn": "agent:a1", "subagent": true, "base": "proj", "applied": "other-proj", "path_rule": "other-proj", "source": "evidence+mention"},
		"tel_attribution", map[string]any{"turn": "agent:a1", "subagent": true, "base": "proj", "applied": "other-proj", "path_rule": "other-proj", "source": "evidence+mention"}},
	// base and applied are project names: "007" stays text.
	{"attribution", map[string]any{"turn": "t2", "base": "007", "applied": "0042", "source": "none"},
		"tel_attribution", map[string]any{"turn": "t2", "subagent": nil, "base": "007", "applied": "0042", "path_rule": nil, "source": "none"}},
	{"store", map[string]any{"result": "saved", "kind": "decision"},
		"tel_store", map[string]any{"result": "saved", "kind": "decision"}},
	{"dashboard", map[string]any{"action": "start", "ms": 10, "pages": 3},
		"tel_dashboard", map[string]any{"action": "start", "ms": 10, "pages": 3}},
	// sha256 keeps a lower-case hex SHA-256 only: prose, a short or an
	// upper-case value is NULL.
	{"config", map[string]any{"sha256": "CANARY prose in the hash field"},
		"tel_config", map[string]any{"sha256": nil}},
	{"config", map[string]any{"sha256": "ab12"},
		"tel_config", map[string]any{"sha256": nil}},
	{"config", map[string]any{"sha256": strings.ToUpper(sha256Hex)},
		"tel_config", map[string]any{"sha256": nil}},
	// last: the config values test reads its offset
	{"config", map[string]any{"sha256": sha256Hex, "recall.min_score": 0.4, "model": "x"},
		"tel_config", map[string]any{"sha256": sha256Hex}},
}

// sha256Hex is a SHA-256 in lower-case hex.
const sha256Hex = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

// TestTelemetryTypedRows: each typed kind lands in its table, column by
// column; ids, scopes and config values in their side tables.
func TestTelemetryTypedRows(t *testing.T) {
	e := newEnv(t)
	var content strings.Builder
	offsets := make([]int64, len(typedCases))
	kinds := map[string]bool{}
	for i, c := range typedCases {
		offsets[i] = int64(content.Len())
		content.WriteString(tline(c.event, ts0, withCommon(c.fields)))
		kinds[c.event] = true
	}
	if len(kinds) != 20 {
		t.Fatalf("the cases cover %d kinds, want the 20 typed ones", len(kinds))
	}
	e.write("2026-10", content.String())
	st := e.ingest()
	if st.Lines != int64(len(typedCases)) || st.Errors != 0 {
		t.Fatalf("stats %+v", st)
	}
	for i, c := range typedCases {
		rs := e.rowsOf(`SELECT * FROM `+c.table+` WHERE "offset" = ?`, offsets[i])
		if len(rs) != 1 {
			t.Errorf("%s #%d: %d rows in %s", c.event, i, len(rs), c.table)
			continue
		}
		got := rs[0]
		if got["source_file"] != "telemetry/events-2026-10.jsonl" {
			t.Errorf("%s: source_file %v", c.event, got["source_file"])
		}
		want := wantCommon(c.want)
		var cols []string
		for k := range got {
			if k != "source_file" && k != "offset" {
				cols = append(cols, k)
			}
		}
		sort.Strings(cols)
		for _, k := range cols {
			w, ok := want[k]
			if !ok {
				t.Errorf("%s #%d: column %s not in the case", c.event, i, k)
				continue
			}
			if !sameValue(got[k], w) {
				t.Errorf("%s #%d: %s = %#v, want %#v", c.event, i, k, got[k], w)
			}
		}
		if len(want) != len(cols) {
			t.Errorf("%s #%d: the case lists %d columns, the table has %d", c.event, i, len(want), len(cols))
		}
	}
	// Nothing went to events: every kind here is typed.
	if n := e.count("events"); n != 0 {
		t.Errorf("events has %d rows", n)
	}

	ids := e.rowsOf(`SELECT "offset", kind, pos, memory_id, ts FROM tel_ids ORDER BY "offset", pos`)
	var gotIDs []string
	for _, r := range ids {
		if r["ts"] != ms0 {
			t.Errorf("tel_ids ts %v", r["ts"])
		}
		gotIDs = append(gotIDs, fmt.Sprintf("%v/%v/%v", r["kind"], r["pos"], r["memory_id"]))
	}
	wantIDs := []string{
		"prompt/0/m1", "prompt/1/m2", "prompt/2/m3",
		"outcome/0/m1",
		"briefing/0/b1", "briefing/1/b2",
		"briefing_outcome/0/b1",
		"tool_use/0/m1", "tool_use/1/m2",
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Errorf("tel_ids\n got %v\nwant %v", gotIDs, wantIDs)
	}
	if ids[0]["offset"] != offsets[0] {
		t.Errorf("prompt ids at %v, want %d", ids[0]["offset"], offsets[0])
	}

	scopes := e.rowsOf(`SELECT "offset", ts, scope, ok, ms, raw FROM tel_prompt_scopes ORDER BY scope`)
	if len(scopes) != 2 {
		t.Fatalf("scopes %v", scopes)
	}
	for i, w := range []map[string]any{
		{"offset": offsets[0], "ts": ms0, "scope": "global", "ok": false, "ms": 30, "raw": 0},
		{"offset": offsets[0], "ts": ms0, "scope": "user", "ok": true, "ms": 12, "raw": 5},
	} {
		for k, v := range w {
			if !sameValue(scopes[i][k], v) {
				t.Errorf("scope %d: %s = %#v, want %#v", i, k, scopes[i][k], v)
			}
		}
	}

	cv := e.rowsOf(`SELECT "offset", ts, key, value FROM tel_config_values`)
	if len(cv) != 1 || cv[0]["key"] != "recall.min_score" || cv[0]["value"] != 0.4 || cv[0]["ts"] != ms0 || cv[0]["offset"] != offsets[len(offsets)-1] {
		t.Errorf("config values %v", cv)
	}
	if hits := textAnywhere(t, e.ix, "x"); slices.ContainsFunc(hits, func(h string) bool { return strings.HasSuffix(h, "=x") }) {
		t.Errorf("the config's model string was stored: %v", hits)
	}
	if hits := textAnywhere(t, e.ix, "disk full"); len(hits) > 0 {
		t.Errorf("an error text was stored: %v", hits)
	}
}

// TestFreeTextNeverStored: reasons, kinds and statuses outside their
// closed sets become other, names that could carry text become NULL, and
// a payload the ingest does not map never reaches the file.
func TestFreeTextNeverStored(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("p", 257)
	lines := []string{
		tline("prompt", ts0, withCommon(map[string]any{"n": 1, "decision": "skip", "reason": "budget exceeded by user", "lang": "Français, mon ami"})),
		tline("gate", ts0, withCommon(map[string]any{"n": 1, "decision": "allow", "reason": "x"})),
		tline("hook_failure", ts0, withCommon(map[string]any{"hook": "stop", "kind": "CANARY-segfault-kind", "ms": 1})),
		tline("job", ts0, withCommon(map[string]any{"name": "maintain", "status": "partial", "ms": 1, "details": map[string]any{"shared_error": "CANARY-boom-error"}})),
		tline("tool", ts0, map[string]any{"session": "canary-a\ncanary-b", "project": long, "binary_version": "1.2.3", "v": "v10", "name": "memory_read"}),
		tline("credit_check", ts0, withCommon(map[string]any{"outcome": "declared", "shown": 1, "flags": []string{"ok_flag", "Not A Flag"}})),
		tline("prompt", ts0, withCommon(map[string]any{"n": 2, "decision": "maybe later"})),
		tline("prompt", ts0, withCommon(map[string]any{"n": 3, "decision": "recall",
			"ids":    []string{"ok1", "bad\nid", "ok2"},
			"scopes": map[string]any{"user": map[string]any{"ok": true}, "bad\nscope": map[string]any{"ok": true}}})),
		tline("config", ts0, withCommon(map[string]any{"sha256": "CANARY-sha256-prose words", "Model Name": 3, "recall.gap": 2, "path": "/home/someone/x"})),
		tline("attribution", ts0, withCommon(map[string]any{"turn": "t1", "base": "proj", "applied": "x\ty", "path_rule": "line one\nline two", "source": "my own words"})),
	}
	e.write("2026-10", strings.Join(lines, ""))
	if st := e.ingest(); st.Errors != 0 {
		t.Fatalf("stats %+v", st)
	}
	one := func(q string) map[string]any {
		t.Helper()
		rs := e.rowsOf(q)
		if len(rs) != 1 {
			t.Fatalf("%s: %d rows", q, len(rs))
		}
		return rs[0]
	}
	if r := one(`SELECT reason, lang FROM tel_prompt WHERE n = 1`); r["reason"] != "other" || r["lang"] != "other" {
		t.Errorf("prompt %v", r)
	}
	if r := one(`SELECT decision FROM tel_prompt WHERE n = 2`); r["decision"] != "other" {
		t.Errorf("prompt decision %v", r)
	}
	if r := one(`SELECT reason FROM tel_gate`); r["reason"] != "other" {
		t.Errorf("gate %v", r)
	}
	if r := one(`SELECT kind FROM tel_hook_failure`); r["kind"] != "other" {
		t.Errorf("hook_failure %v", r)
	}
	if r := one(`SELECT name, status FROM tel_job`); r["name"] != "maintain" || r["status"] != "partial" {
		t.Errorf("job %v", r)
	}
	if r := one(`SELECT session, project, name FROM tel_tool`); r["session"] != nil || r["project"] != nil || r["name"] != "memory_read" {
		t.Errorf("tool %v", r)
	}
	if r := one(`SELECT flags FROM tel_credit_check`); r["flags"] != "ok_flag,other" {
		t.Errorf("flags %v", r)
	}
	if r := one(`SELECT ids_n FROM tel_prompt WHERE n = 3`); r["ids_n"] != int64(3) {
		t.Errorf("ids_n %v", r)
	}
	var ids []string
	for _, r := range e.rowsOf(`SELECT pos, memory_id FROM tel_ids ORDER BY pos`) {
		ids = append(ids, fmt.Sprintf("%v:%v", r["pos"], r["memory_id"]))
	}
	if !slices.Equal(ids, []string{"0:ok1", "2:ok2"}) {
		t.Errorf("tel_ids %v", ids)
	}
	if r := one(`SELECT scope FROM tel_prompt_scopes`); r["scope"] != "user" {
		t.Errorf("scopes %v", r)
	}
	if r := one(`SELECT key, value FROM tel_config_values`); r["key"] != "recall.gap" || r["value"] != 2.0 {
		t.Errorf("config values %v", r)
	}
	if r := one(`SELECT base, applied, path_rule, source FROM tel_attribution`); r["base"] != "proj" || r["applied"] != nil || r["path_rule"] != nil || r["source"] != "other" {
		t.Errorf("attribution %v", r)
	}
	for _, s := range []string{"line one", "my own words", "CANARY-boom-error", "budget exceeded", "CANARY-segfault-kind", "Français", "Not A Flag", "maybe later", long, "bad\n", "Model Name", "/home/someone"} {
		if hits := textAnywhere(t, e.ix, s); len(hits) > 0 {
			t.Errorf("%q stored: %v", s, hits)
		}
	}
	noBytes(t, e.ix, "CANARY-boom-error")
	noBytes(t, e.ix, "CANARY-segfault-kind")
	noBytes(t, e.ix, "canary-a\ncanary-b")
	noBytes(t, e.ix, "CANARY-sha256-prose")
}

// TestUnknownKindsKeepNoPayload: other kinds keep their common columns
// and only the fields of their allowlist.
func TestUnknownKindsKeepNoPayload(t *testing.T) {
	e := newEnv(t)
	const canary = "CANARY-unknown-kind-7f3a91"
	lines := []string{
		tline("session_state_error", ts0, map[string]any{"session": "s1", "project": "proj", "binary_version": "1.2.3", "error": "E1"}),
		tline("spawn_failed", ts0, withCommon(map[string]any{"error": canary})),
		tline("failure_recall", ts0, withCommon(map[string]any{"tool": "Bash", "decision": "skip", "reason": "budget", "ms": 3})),
		tline("failure_recall", ts0, withCommon(map[string]any{"tool": "Bash", "decision": "Skip Now", "ms": []int{1}})),
		tline("Weird Kind!", ts0, withCommon(nil)),
	}
	e.write("2026-10", strings.Join(lines, ""))
	if st := e.ingest(); st.Errors != 0 || st.Lines != 5 {
		t.Fatalf("stats %+v", st)
	}
	rs := e.rowsOf(`SELECT kind, session, project, binary_version, fields, ts FROM events ORDER BY "offset"`)
	if len(rs) != 5 {
		t.Fatalf("events %v", rs)
	}
	want := []map[string]any{
		{"kind": "session_state_error", "session": "s1", "project": "proj", "binary_version": "1.2.3", "fields": nil, "ts": ms0},
		{"kind": "spawn_failed", "fields": nil},
		{"kind": "failure_recall", "fields": `{"decision":"skip","ms":3}`},
		{"kind": "failure_recall", "fields": `{"decision":"other","ms":"other"}`},
		{"kind": "other", "fields": nil},
	}
	for i, w := range want {
		for k, v := range w {
			if !sameValue(rs[i][k], v) {
				t.Errorf("event %d: %s = %#v, want %#v", i, k, rs[i][k], v)
			}
		}
	}
	if hits := textAnywhere(t, e.ix, "E1"); len(hits) > 0 {
		t.Errorf("E1 stored: %v", hits)
	}
	for _, s := range []string{canary, "Bash", "budget", "Weird"} {
		if hits := textAnywhere(t, e.ix, s); len(hits) > 0 {
			t.Errorf("%q stored: %v", s, hits)
		}
	}
	noBytes(t, e.ix, canary)
}
