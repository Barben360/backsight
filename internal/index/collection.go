package index

import (
	"encoding/json"
	"slices"
	"time"
)

// The collection families (internal/metricsfile's, which this package
// cannot import: it depends on internal/instructions, whose tests import
// the metrics views; the tests check the two lists agree), their schema,
// and the cursors kinds of their month files (<state>/metrics/
// <family>-YYYY-MM.jsonl).
const (
	familyServerDaily   = "server-daily"
	familyDoctorHistory = "doctor-history"
	familyMemoryDocs    = "memory-docs"
	familyScanHistory   = "scan-history"
	collectionSchema    = 1
)

const (
	kindServerDaily   = "server_daily"
	kindDoctorHistory = "doctor_history"
	kindMemoryDocs    = "memory_docs"
	kindScanHistory   = "scan_history"
)

// The closed sets of the collection lines (internal/metricsfile writes
// them; the LLM operations and statuses are the server's).
var (
	snapshotErrors = []string{"not_configured", "timeout", "unreachable", "unauthorized", "not_found", "bad_response", "server_error"}
	snapshotOps    = []string{"pending", "processing", "completed", "failed", "other"}
	llmOperations  = []string{"all", "retain", "consolidation", "refresh_mental_model"}
	llmStatuses    = []string{"success", "error"}
	checkLevels    = []string{"ok", "info", "warn", "error"}
	docOutcomes    = []string{"found"}
	docKinds       = []string{"session", "note", "import", "other", "none"}
	factTypes      = []string{"world", "experience", "observation", "other"}
	opStatuses     = []string{"pending", "processing", "completed", "failed", "other"}
)

var (
	snapshotCounts = []string{"world", "experience", "observation", "documents", "links", "observations"}
	kindCounts     = []string{"world", "experience", "observation"}
	llmCounts      = []string{"input", "output", "cached", "thoughts", "requests"}
	scanCounts     = []string{"live", "forgotten", "pages", "ms"}
)

// collectionRows maps the lines of one family to their rows and their
// time (unix ms), or names why a line is skipped. A line of another
// schema, or of a kind this binary does not read, is a metrics_other row:
// a newer binary's line is not an error. A known kind without its
// required field is missing_field. Only the fields named here are read.
func collectionRows(family string) lineFunc {
	return func(rel string, off int64, line []byte) ([]row, int64, string) {
		d, ok := parseObject(line)
		if !ok {
			return nil, 0, errUnparsable
		}
		ts, ok := d.ts()
		if !ok {
			return nil, 0, errNoTS
		}
		schema := d.integer("schema")
		kind, _ := d.str("kind")
		if d.err != "" {
			return nil, 0, d.err
		}
		c := collLine{d: d, rel: rel, off: off, ts: ts}
		var (
			rows   []row
			reason string
			known  bool
		)
		if n, _ := schema.(int64); n == collectionSchema {
			rows, reason, known = c.typed(family, kind)
		}
		if !known {
			r := c.row("metrics_other")
			r.add("family", family)
			r.add("kind", shapeValue(kind))
			rows = []row{r}
		}
		if reason == "" {
			reason = d.err
		}
		if reason != "" {
			return nil, 0, reason
		}
		return rows, ts, ""
	}
}

// collLine is one collection line being mapped.
type collLine struct {
	d   *lineDec
	rel string
	off int64
	ts  int64
}

// row starts a row of table with the line's key and time.
func (c collLine) row(table string) row {
	r := row{table: table}
	r.add("source_file", c.rel)
	r.add(`"offset"`, c.off)
	r.add("ts", c.ts)
	return r
}

// typed maps a line of a kind of family this binary reads (known false:
// not one).
func (c collLine) typed(family, kind string) (rows []row, reason string, known bool) {
	var f func() ([]row, string)
	switch family + "/" + kind {
	case familyServerDaily + "/snapshot":
		f = c.snapshot
	case familyServerDaily + "/day":
		f = c.day
	case familyDoctorHistory + "/run":
		f = c.run
	case familyMemoryDocs + "/memory":
		f = c.memory
	case familyMemoryDocs + "/listed":
		f = c.listed
	case familyMemoryDocs + "/op":
		f = c.op
	case familyScanHistory + "/scan":
		f = c.scan
	default:
		return nil, "", false
	}
	rows, reason = f()
	return rows, reason, true
}

func (c collLine) snapshot() ([]row, string) {
	d := c.d
	day := dayValue(d, "day")
	if day == nil {
		return nil, missingOr(d)
	}
	r := c.row("server_snapshots")
	r.add("day", day)
	r.add("ok", d.boolean("ok"))
	r.add("error", closedStr(d, "error", snapshotErrors))
	for _, k := range snapshotCounts {
		r.add(k, d.integer(k))
	}
	ops := intFields(d, "ops") // a key outside the set is not read
	for _, k := range snapshotOps {
		r.add("ops_"+k, ops[k])
	}
	r.add("consolidation_pending", d.integer("consolidation_pending"))
	r.add("consolidation_failed", d.integer("consolidation_failed"))
	r.add("last_consolidated_at", timeValue(d, "last_consolidated_at"))
	out := []row{r}
	projects := intFields(d, "projects")
	for _, name := range sortedKeys(projects) {
		if !nameOK(name) {
			continue // the project is the row's key
		}
		p := c.row("server_snapshot_projects")
		p.add("day", day)
		p.add("project", name)
		p.add("memories", projects[name])
		out = append(out, p)
	}
	return out, ""
}

// opDay is one operation's LLM use, operations outside the set summed
// into other.
type opDay struct {
	n        map[string]any
	statuses map[string]any
}

func (c collLine) day() ([]row, string) {
	d := c.d
	bucket := timeValue(d, "bucket")
	if bucket == nil {
		return nil, missingOr(d)
	}
	r := c.row("server_days")
	r.add("bucket", bucket)
	news := intFields(d, "new")
	for _, k := range kindCounts {
		r.add(k, news[k])
	}
	out := []row{r}
	llm := objects(d, "llm")
	byOp := map[string]*opDay{}
	for _, name := range sortedKeys(llm) {
		op, _ := closedValue(name, llmOperations).(string)
		od, ok := parseObject(llm[name])
		if !ok {
			return nil, errBadType
		}
		if op == "" {
			continue // an empty name: no row
		}
		acc := byOp[op]
		if acc == nil {
			acc = &opDay{n: map[string]any{}, statuses: map[string]any{}}
			byOp[op] = acc
		}
		for _, k := range llmCounts {
			acc.n[k] = addCounts(acc.n[k], od.integer(k))
		}
		statuses := intFields(od, "statuses")
		if od.err != "" {
			return nil, od.err
		}
		for _, name := range sortedKeys(statuses) {
			if s, ok := closedValue(name, llmStatuses).(string); ok {
				acc.statuses[s] = addCounts(acc.statuses[s], statuses[name])
			}
		}
	}
	for _, op := range sortedKeys(byOp) {
		acc := byOp[op]
		l := c.row("server_day_llm")
		l.add("bucket", bucket)
		l.add("operation", op)
		for _, k := range llmCounts {
			l.add(k, acc.n[k])
		}
		out = append(out, l)
		for _, status := range sortedKeys(acc.statuses) {
			s := c.row("server_day_llm_status")
			s.add("bucket", bucket)
			s.add("operation", op)
			s.add("status", status)
			s.add("n", acc.statuses[status])
			out = append(out, s)
		}
	}
	return out, ""
}

func (c collLine) run() ([]row, string) {
	d := c.d
	if _, ok := d.raw("checks"); !ok {
		return nil, errMissingField
	}
	checks := objects(d, "checks")
	inner := &lineDec{o: checks}
	var out []row
	for _, id := range sortedKeys(checks) {
		level, _ := inner.str(id)
		if inner.err != "" {
			return nil, inner.err
		}
		if idValue(id) == nil {
			continue // the check is the row's key
		}
		r := c.row("doctor_runs")
		r.add("check_id", id)
		r.add("status", closedValue(level, checkLevels))
		out = append(out, r)
	}
	return out, ""
}

func (c collLine) memory() ([]row, string) {
	d := c.d
	id := strID(d, "memory")
	if id == nil {
		return nil, missingOr(d)
	}
	r := c.row("memory_docs")
	r.add("memory_id", id)
	r.add("outcome", closedStr(d, "outcome", docOutcomes))
	kind := closedStr(d, "doc_kind", docKinds)
	r.add("doc_kind", kind)
	// A document is kept only when it is a session's: another may name a
	// file.
	var document, session any
	if kind == "session" {
		document, session = strID(d, "document"), strID(d, "session")
	}
	r.add("document", document)
	r.add("session", session)
	r.add("created_day", dayValue(d, "created_day"))
	r.add("fact_type", closedStr(d, "fact_type", factTypes))
	return []row{r}, ""
}

func (c collLine) listed() ([]row, string) {
	d := c.d
	day := dayValue(d, "day")
	if day == nil {
		return nil, missingOr(d)
	}
	r := c.row("memory_days")
	r.add("day", day)
	r.add("n", d.integer("n"))
	complete := d.boolean("complete")
	if complete == nil {
		complete = int64(0) // written only when true
	}
	r.add("complete", complete)
	return []row{r}, ""
}

func (c collLine) op() ([]row, string) {
	d := c.d
	id := strID(d, "operation")
	if id == nil {
		return nil, missingOr(d)
	}
	r := c.row("doc_ops")
	r.add("operation_id", id)
	r.add("document", strID(d, "document"))
	r.add("session", strID(d, "session"))
	r.add("status", closedStr(d, "status", opStatuses))
	r.add("created_at", timeValue(d, "created_at"))
	r.add("updated_at", timeValue(d, "updated_at"))
	r.add("retries", d.integer("retries"))
	r.add("units", d.integer("units"))
	noUnits := d.boolean("no_units")
	if noUnits == nil {
		noUnits = int64(0) // written only when true
	}
	r.add("no_units", noUnits)
	return []row{r}, ""
}

func (c collLine) scan() ([]row, string) {
	d := c.d
	count := d.integer("count")
	if count == nil {
		return nil, missingOr(d)
	}
	r := c.row("scans")
	r.add("count", count)
	for _, k := range scanCounts {
		r.add(k, d.integer(k))
	}
	return []row{r}, ""
}

// missingOr is why a required field gave no value: its type error, else
// missing_field.
func missingOr(d *lineDec) string {
	if d.err != "" {
		return d.err
	}
	return errMissingField
}

// idValue is s when it is an id (a name without whitespace), nil
// otherwise.
func idValue(s string) any {
	if !keyPartOK(s) {
		return nil
	}
	return s
}

func strID(d *lineDec, key string) any {
	s, _ := d.str(key)
	return idValue(s)
}

func closedStr(d *lineDec, key string, set []string) any {
	s, _ := d.str(key)
	return closedValue(s, set)
}

// dayValue is the field when it is a calendar date YYYY-MM-DD, nil
// otherwise.
func dayValue(d *lineDec, key string) any {
	s, _ := d.str(key)
	if t, err := time.Parse(time.DateOnly, s); err != nil || t.Format(time.DateOnly) != s {
		return nil
	}
	return s
}

// timeValue is the field, an RFC 3339 time, in unix ms; nil when absent,
// not such a time, or the zero time (one the server did not give).
func timeValue(d *lineDec, key string) any {
	s, _ := d.str(key)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// objects is the field's JSON object, member by member (nil when absent;
// another type is bad_type).
func objects(d *lineDec, key string) map[string]json.RawMessage {
	raw, ok := d.raw(key)
	if !ok {
		return nil
	}
	var m map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &m) != nil {
		d.badType()
		return nil
	}
	return m
}

// intFields is the field's object of integers; a null member is none.
func intFields(d *lineDec, key string) map[string]any {
	m := objects(d, key)
	inner := &lineDec{o: m}
	out := make(map[string]any, len(m))
	for k := range m {
		if v := inner.integer(k); v != nil {
			out[k] = v
		}
	}
	if inner.err != "" {
		d.badType()
		return nil
	}
	return out
}

// addCounts adds two integer values, nil being none.
func addCounts(a, b any) any {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return a.(int64) + b.(int64)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
