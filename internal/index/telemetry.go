package index

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"
)

// Reasons of ingest_errors.
const (
	errUnparsable   = "unparsable"    // not a JSON object, a NUL byte, invalid UTF-8
	errNoTS         = "no_ts"         // ts missing or not RFC 3339
	errMissingField = "missing_field" // event, or a required field of a known kind
	errBadType      = "bad_type"      // a mapped field of the wrong JSON type
	errTooLong      = "too_long"      // a line over maxLineBytes
	errUnreadable   = "unreadable"    // the file itself (unreadableOffset)
	// The ledger reader or the local status failed (offset 0).
	errLedgerUnreadable      = "ledger_unreadable"
	errLocalStatusUnreadable = "local_status_unreadable"
)

// row is one row to insert.
type row struct {
	table string
	cols  []string
	vals  []any
}

func (r *row) add(col string, v any) {
	r.cols = append(r.cols, col)
	r.vals = append(r.vals, v)
}

// fkind is how a field is read and guarded.
type fkind int

const (
	fInt    fkind = iota // a JSON integer
	fReal                // a JSON number
	fBool                // a JSON boolean, stored 0/1
	fName                // a string, name guard
	fShape               // a string, shape guard
	fClosed              // a string, closed set
	fIDs                 // an array of ids: its length, and one tel_ids row each
	fFlags               // an array of shape strings, sorted and comma-joined
)

type fspec struct {
	key, col string
	kind     fkind
	set      []string
	req      bool
	dflt     any // fClosed: the value when absent
}

// telKind is a typed kind: its table, whether it carries n, and its
// fields after the common ones (field names of internal/telemetry).
type telKind struct {
	table  string
	n      bool
	fields []fspec
	extra  func(d *lineDec, rel string, off, ts int64) []row
}

var telKinds = map[string]telKind{
	"prompt": {table: "tel_prompt", n: true, fields: []fspec{
		{key: "decision", kind: fClosed, set: promptDecisions, req: true},
		{key: "reason", kind: fClosed, set: promptReasons},
		{key: "plen", kind: fInt},
		{key: "lang", kind: fShape},
		{key: "above", kind: fInt},
		{key: "injected", kind: fInt},
		{key: "ids", col: "ids_n", kind: fIDs},
		{key: "top", kind: fReal},
		{key: "ms", kind: fInt},
		{key: "nudged", kind: fBool},
		{key: "hinted", kind: fBool},
		{key: "detector", kind: fClosed, set: detectors},
		{key: "chars", kind: fInt},
		{key: "empty", kind: fClosed, set: promptEmpty},
	}, extra: promptScopes},
	"outcome": {table: "tel_outcome", n: true, fields: []fspec{
		{key: "ids", col: "ids_n", kind: fIDs},
		{key: "searched", kind: fBool},
		{key: "credited", kind: fClosed, set: replyKinds},
		{key: "credit_source", kind: fClosed, set: creditSources},
		{key: "contradicted", kind: fBool},
	}},
	"reply": {table: "tel_reply", n: true, fields: []fspec{
		{key: "credit", kind: fClosed, set: replyKinds},
		{key: "searched", kind: fBool},
	}},
	"briefing": {table: "tel_briefing", fields: []fspec{
		{key: "ids", col: "ids_n", kind: fIDs},
		{key: "chars", kind: fInt},
		{key: "source", kind: fClosed, set: briefingSources},
	}},
	"briefing_outcome": {table: "tel_briefing_outcome", fields: []fspec{
		{key: "used_ids", col: "used_n", kind: fIDs},
	}},
	"tool": {table: "tel_tool", fields: []fspec{
		{key: "name", kind: fName, req: true},
		{key: "ms", kind: fInt},
		{key: "hits", kind: fInt},
		{key: "ok", kind: fBool},
		{key: "live", kind: fClosed, set: liveKinds},
	}},
	// tool_use: ids is the count; memory_ids, the ids themselves.
	"tool_use": {table: "tel_tool_use", n: true, fields: []fspec{
		{key: "name", kind: fName, req: true},
		{key: "ids", col: "ids_n", kind: fInt},
		{key: "subagent", kind: fBool},
	}, extra: toolUseIDs},
	"credit": {table: "tel_credit", fields: []fspec{
		{key: "verdict", kind: fClosed, set: verdicts, req: true},
		{key: "ids_n", kind: fInt},
		{key: "ok", kind: fBool},
	}},
	"credit_check": {table: "tel_credit_check", fields: []fspec{
		{key: "outcome", kind: fClosed, set: checkOutcomes, req: true},
		{key: "shown", kind: fInt},
		{key: "flags", kind: fFlags},
		{key: "after_block", kind: fBool},
	}},
	"retire": {table: "tel_retire", fields: []fspec{
		{key: "action", kind: fClosed, set: retireActions, req: true},
		{key: "ids_n", kind: fInt},
	}},
	"feedback_settle": {table: "tel_feedback_settle", fields: []fspec{
		{key: "shows", kind: fInt},
		{key: "credits", kind: fInt},
		{key: "misses", kind: fInt},
		{key: "unknown", kind: fInt},
		{key: "late", kind: fInt},
	}},
	// feedback_error: its error field is never read.
	"feedback_error": {table: "tel_feedback_error", fields: []fspec{
		{key: "reason", kind: fClosed, set: feedbackReasons, dflt: other},
	}},
	"gate": {table: "tel_gate", n: true, fields: []fspec{
		{key: "decision", kind: fClosed, set: gateDecisions, req: true},
		{key: "reason", kind: fClosed, set: gateReasons},
	}},
	"retain": {table: "tel_retain", fields: []fspec{
		{key: "document", kind: fName, req: true},
		{key: "items", kind: fInt},
		{key: "status", kind: fClosed, set: retainStatuses, req: true},
		{key: "ms", kind: fInt},
	}},
	"hook_failure": {table: "tel_hook_failure", fields: []fspec{
		{key: "hook", kind: fShape},
		{key: "kind", kind: fClosed, set: failureKinds, req: true},
		{key: "ms", kind: fInt},
	}},
	// job: details is never read.
	"job": {table: "tel_job", fields: []fspec{
		{key: "name", kind: fShape, req: true},
		{key: "status", kind: fClosed, set: jobStatuses, req: true},
		{key: "ms", kind: fInt},
	}},
	"attribution": {table: "tel_attribution", fields: []fspec{
		{key: "turn", kind: fName},
		{key: "subagent", kind: fBool},
		{key: "base", kind: fName},
		{key: "applied", kind: fName},
		{key: "path_rule", kind: fName},
		{key: "source", kind: fClosed, set: attributionSources},
	}},
	"store": {table: "tel_store", fields: []fspec{
		{key: "result", kind: fClosed, set: storeResults, req: true},
		{key: "kind", kind: fShape},
	}},
	"dashboard": {table: "tel_dashboard", fields: []fspec{
		{key: "action", kind: fClosed, set: dashActions, req: true},
		{key: "ms", kind: fInt},
		{key: "pages", kind: fInt},
	}},
	"config": {table: "tel_config", fields: []fspec{
		{key: "sha256", kind: fName},
	}, extra: configValues},
}

// commonKeys are the writer's own fields, on every line.
var commonKeys = []string{"ts", "event", "v", "session", "project", "binary_version"}

// telemetryRows maps one telemetry line to its rows and its time (unix
// ms), or names why it is skipped.
func telemetryRows(rel string, off int64, line []byte) ([]row, int64, string) {
	d, ok := parseObject(line)
	if !ok {
		return nil, 0, errUnparsable
	}
	ts, ok := d.ts()
	if !ok {
		return nil, 0, errNoTS
	}
	event, ok := d.str("event")
	if d.err != "" {
		return nil, 0, d.err
	}
	if !ok || event == "" {
		return nil, 0, errMissingField
	}
	session, project, binary := d.name("session"), d.name("project"), d.name("binary_version")
	k, typed := telKinds[event]
	if !typed {
		r := row{table: "events"}
		r.add("source_file", rel)
		r.add(`"offset"`, off)
		r.add("ts", ts)
		r.add("kind", shapeValue(event))
		r.add("session", session)
		r.add("project", project)
		r.add("binary_version", binary)
		r.add("fields", d.payload(payloadAllow[event]))
		if d.err != "" {
			return nil, 0, d.err
		}
		return []row{r}, ts, ""
	}
	r := row{table: k.table}
	r.add("source_file", rel)
	r.add(`"offset"`, off)
	r.add("ts", ts)
	r.add("session", session)
	r.add("project", project)
	r.add("binary_version", binary)
	r.add("v", d.name("v"))
	if k.n {
		r.add("n", d.integer("n"))
	}
	if d.err != "" {
		return nil, 0, d.err
	}
	rows := []row{{}} // the main row goes first
	for _, f := range k.fields {
		col := f.col
		if col == "" {
			col = f.key
		}
		var v any
		switch f.kind {
		case fInt:
			v = d.integer(f.key)
		case fReal:
			v = d.real(f.key)
		case fBool:
			v = d.boolean(f.key)
		case fName, fShape, fClosed:
			s, _ := d.str(f.key)
			if f.req && s == "" && d.err == "" {
				return nil, 0, errMissingField
			}
			switch f.kind {
			case fName:
				v = nameValue(s)
			case fShape:
				v = shapeValue(s)
			default:
				v = closedValue(s, f.set)
				if v == nil && f.dflt != nil {
					v = f.dflt
				}
			}
		case fIDs:
			ids, ok := d.strs(f.key)
			if ok {
				v = int64(len(ids))
				rows = append(rows, idRows(rel, off, ts, event, ids)...)
			}
		case fFlags:
			flags, _ := d.strs(f.key)
			v = flagsValue(flags)
		}
		if d.err != "" {
			return nil, 0, d.err
		}
		r.add(col, v)
	}
	if k.extra != nil {
		rows = append(rows, k.extra(d, rel, off, ts)...)
		if d.err != "" {
			return nil, 0, d.err
		}
	}
	rows[0] = r
	return rows, ts, ""
}

// idRows are the tel_ids rows of a line's ids, pos being the index in
// its array; an id that is not a valid name is left out.
func idRows(rel string, off, ts int64, kind string, ids []string) []row {
	var out []row
	for i, id := range ids {
		if !nameOK(id) {
			continue
		}
		r := row{table: "tel_ids"}
		r.add("source_file", rel)
		r.add(`"offset"`, off)
		r.add("ts", ts)
		r.add("kind", kind)
		r.add("pos", int64(i))
		r.add("memory_id", id)
		out = append(out, r)
	}
	return out
}

func toolUseIDs(d *lineDec, rel string, off, ts int64) []row {
	ids, _ := d.strs("memory_ids")
	return idRows(rel, off, ts, "tool_use", ids)
}

// promptScopes: one tel_prompt_scopes row per scope of a prompt.
func promptScopes(d *lineDec, rel string, off, ts int64) []row {
	raw, ok := d.raw("scopes")
	if !ok {
		return nil
	}
	var scopes map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &scopes) != nil {
		d.badType()
		return nil
	}
	names := make([]string, 0, len(scopes))
	for name := range scopes {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []row
	for _, name := range names {
		sd, ok := parseObject(scopes[name])
		if !ok {
			d.badType()
			return nil
		}
		okv, ms, rawN := sd.boolean("ok"), sd.integer("ms"), sd.integer("raw")
		if sd.err != "" {
			d.badType()
			return nil
		}
		if !nameOK(name) {
			continue
		}
		r := row{table: "tel_prompt_scopes"}
		r.add("source_file", rel)
		r.add(`"offset"`, off)
		r.add("ts", ts)
		r.add("scope", name)
		r.add("ok", okv)
		r.add("ms", ms)
		r.add("raw", rawN)
		out = append(out, r)
	}
	return out
}

// configValues: one tel_config_values row per numeric field of a config
// event whose key has the shape of a config key; strings are never read.
func configValues(d *lineDec, rel string, off, ts int64) []row {
	keys := make([]string, 0, len(d.o))
	for k := range d.o {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []row
	for _, k := range keys {
		if slices.Contains(commonKeys, k) || !configKeyRE.MatchString(k) {
			continue
		}
		raw, ok := d.raw(k)
		if !ok || !isNumber(raw) {
			continue
		}
		f, err := strconv.ParseFloat(string(raw), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			continue
		}
		r := row{table: "tel_config_values"}
		r.add("source_file", rel)
		r.add(`"offset"`, off)
		r.add("ts", ts)
		r.add("key", k)
		r.add("value", f)
		out = append(out, r)
	}
	return out
}

// lineDec reads a line's fields; a type error sticks in err.
type lineDec struct {
	o   map[string]json.RawMessage
	err string
}

// parseObject decodes a JSON object in valid UTF-8 (the decoder would
// replace invalid bytes; it refuses a NUL byte itself).
func parseObject(b []byte) (*lineDec, bool) {
	if !utf8.Valid(b) {
		return nil, false
	}
	var o map[string]json.RawMessage
	if err := json.Unmarshal(b, &o); err != nil || o == nil {
		return nil, false
	}
	return &lineDec{o: o}, true
}

// badType records a field of the wrong JSON type.
func (d *lineDec) badType() {
	if d.err == "" {
		d.err = errBadType
	}
}

// raw is the field's JSON, absent and null alike reported missing.
func (d *lineDec) raw(key string) (json.RawMessage, bool) {
	r, ok := d.o[key]
	if !ok {
		return nil, false
	}
	r = bytes.TrimSpace(r)
	if len(r) == 0 || string(r) == "null" {
		return nil, false
	}
	return r, true
}

func isNumber(r json.RawMessage) bool { return r[0] == '-' || (r[0] >= '0' && r[0] <= '9') }

func (d *lineDec) str(key string) (string, bool) {
	r, ok := d.raw(key)
	if !ok {
		return "", false
	}
	var s string
	if r[0] != '"' || json.Unmarshal(r, &s) != nil {
		d.badType()
		return "", false
	}
	return s, true
}

func (d *lineDec) name(key string) any {
	s, _ := d.str(key)
	return nameValue(s)
}

// integer is the field as int64, nil when absent; a number with a
// fraction is a bad type.
func (d *lineDec) integer(key string) any {
	r, ok := d.raw(key)
	if !ok {
		return nil
	}
	if !isNumber(r) {
		d.badType()
		return nil
	}
	if n, err := strconv.ParseInt(string(r), 10, 64); err == nil {
		return n
	}
	f, err := strconv.ParseFloat(string(r), 64)
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		d.badType()
		return nil
	}
	return int64(f)
}

func (d *lineDec) real(key string) any {
	r, ok := d.raw(key)
	if !ok {
		return nil
	}
	if !isNumber(r) {
		d.badType()
		return nil
	}
	f, err := strconv.ParseFloat(string(r), 64)
	if err != nil || math.IsInf(f, 0) {
		d.badType()
		return nil
	}
	return f
}

func (d *lineDec) boolean(key string) any {
	r, ok := d.raw(key)
	if !ok {
		return nil
	}
	switch string(r) {
	case "true":
		return int64(1)
	case "false":
		return int64(0)
	}
	d.badType()
	return nil
}

// strs is an array of strings.
func (d *lineDec) strs(key string) ([]string, bool) {
	r, ok := d.raw(key)
	if !ok {
		return nil, false
	}
	var out []string
	if r[0] != '[' || json.Unmarshal(r, &out) != nil {
		d.badType()
		return nil, false
	}
	return out, true
}

// ts is the line's time in unix ms: a string in RFC 3339.
func (d *lineDec) ts() (int64, bool) {
	r, ok := d.raw("ts")
	if !ok || r[0] != '"' {
		return 0, false
	}
	var s string
	if json.Unmarshal(r, &s) != nil {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// payload is the JSON of the allowed fields: numbers and booleans as
// they are, strings of the enum shape, anything else other; nil when
// none is present.
func (d *lineDec) payload(allow []string) any {
	m := map[string]any{}
	for _, k := range allow {
		r, ok := d.raw(k)
		if !ok {
			continue
		}
		switch {
		case isNumber(r):
			m[k] = json.Number(r)
		case string(r) == "true" || string(r) == "false":
			m[k] = string(r) == "true"
		case r[0] == '"':
			var s string
			if json.Unmarshal(r, &s) == nil && shapeOK(s) {
				m[k] = s
			} else {
				m[k] = other
			}
		default:
			m[k] = other
		}
	}
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(b)
}
