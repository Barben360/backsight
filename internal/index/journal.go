package index

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/feedback"
)

// kindJournal is the cursors kind of the feedback journal month files.
const kindJournal = "journal"

// errSchema: a journal line of another schema than this binary reads.
const errSchema = "schema"

// The closed sets of the journal: every literal feedback writes today.
var (
	showPaths      = []string{"prompt", "gate", "briefing", "search", "read", "failure", feedback.PathSources}
	settleOutcomes = []string{feedback.OutcomeCredit, feedback.OutcomeMiss, feedback.OutcomeShown, feedback.OutcomeUnknown, feedback.OutcomeForgotten, feedback.ImportComplete}
	retireOutcomes = []string{"invalidated", "restored", "rewritten", "gone"}
	retireVerdicts = []string{"wrong", "outdated"}
	importSources  = []string{"telemetry", "transcripts"}
)

// noteIDRE is what a correction note's id looks like; the note's text is
// never read, and an id of another shape is dropped.
var noteIDRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// keyMax bounds a show key (session|turn|ts|id: four names) in bytes.
const keyMax = 1024

// keyValue is a show key when it is valid UTF-8 of at most keyMax bytes
// without a control character, nil otherwise.
func keyValue(s string) any {
	if s == "" || len(s) > keyMax || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return nil
	}
	return s
}

// jrow starts a row of table for the line at off of rel; withPos adds the
// index of the row's item in its line.
func jrow(table, rel string, off int64, pos int, withPos bool) row {
	r := row{table: table}
	r.add("source_file", rel)
	r.add(`"offset"`, off)
	if withPos {
		r.add("pos", int64(pos))
	}
	return r
}

// intOrNil is n as int64, nil for 0 (the journal omits a zero).
func intOrNil(n int) any {
	if n == 0 {
		return nil
	}
	return int64(n)
}

func realOrNil(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

func msOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// journalRows maps one journal line to its rows and its time (unix ms),
// or names why it is skipped. The line decodes into feedback.Event, so a
// field that Event does not name is never read.
func journalRows(rel string, off int64, line []byte) ([]row, int64, string) {
	d, ok := parseObject(line)
	if !ok {
		return nil, 0, errUnparsable
	}
	schema := d.integer("schema")
	if d.err != "" {
		return nil, 0, d.err
	}
	if n, _ := schema.(int64); n != feedback.Schema {
		return nil, 0, errSchema
	}
	ts, ok := d.ts()
	if !ok {
		return nil, 0, errNoTS
	}
	var e feedback.Event
	if json.Unmarshal(line, &e) != nil {
		return nil, 0, errBadType
	}
	if e.Kind == "" {
		return nil, 0, errMissingField
	}
	session, turn, agent, project := nameValue(e.Session), nameValue(e.Turn), nameValue(e.Agent), nameValue(e.Project)

	var out []row
	switch e.Kind {
	case "turn":
		r := jrow("turns", rel, off, 0, false)
		r.add("ts", ts)
		r.add("session", session)
		r.add("turn", turn)
		r.add("agent", agent)
		r.add("project", project)
		r.add("n", intOrNil(e.N))
		var machine any
		if e.Machine {
			machine = "true"
		}
		r.add("machine", machine)
		r.add("binary_version", nameValue(e.Binary))
		out = append(out, r)
	case "shows":
		for i, s := range feedback.ShowsOf(e) {
			it := e.Items[i]
			r := jrow("shows", rel, off, i, true)
			r.add("ts", ts)
			r.add("session", session)
			r.add("key", keyValue(s.Key))
			r.add("turn", turn)
			r.add("agent", agent)
			r.add("path", closedValue(e.Path, showPaths))
			r.add("memory_id", nameValue(it.ID))
			r.add("rank", intOrNil(it.Rank))
			r.add("reranker", realOrNil(it.Reranker))
			r.add("boost", realOrNil(it.Boost))
			r.add("rank_without_boost", intOrNil(it.RankWithoutBoost))
			var swb any
			if it.ScoreWithoutBoost != nil {
				swb = *it.ScoreWithoutBoost
			}
			r.add("score_without_boost", swb)
			r.add("project", nameValue(s.Project))
			out = append(out, r)
		}
	case "credit":
		ids := e.IDs
		if len(ids) == 0 {
			ids = []string{""}
		}
		for i, id := range ids {
			r := jrow("credits", rel, off, i, true)
			r.add("ts", ts)
			r.add("session", session)
			r.add("turn", turn)
			r.add("agent", agent)
			r.add("project", project)
			r.add("memory_id", nameValue(id))
			r.add("verdict", closedValue(e.Verdict, verdicts))
			out = append(out, r)
		}
	case "stop":
		r := jrow("stops", rel, off, 0, false)
		r.add("ts", ts)
		r.add("session", session)
		r.add("turn", turn)
		r.add("agent", agent)
		r.add("outcome", closedValue(e.Outcome, checkOutcomes))
		r.add("stop_check", shapeValue(e.StopCheck))
		r.add("flags", flagsValue(e.Flags))
		out = append(out, r)
	case "settle":
		for i, s := range e.Settled {
			r := jrow("settles", rel, off, i, true)
			r.add("key", keyValue(s.Key))
			r.add("memory_id", nameValue(s.ID))
			r.add("show_at", msOrNil(s.At))
			r.add("outcome", closedValue(s.Outcome, settleOutcomes))
			r.add("settle_ts", ts)
			out = append(out, r)
		}
	case feedback.KindRetire, feedback.KindHide, feedback.KindUnhide, "forget":
		ids := e.IDs
		if len(ids) == 0 {
			ids = []string{""}
		}
		var note any
		if noteIDRE.MatchString(e.Note) {
			note = nameValue(e.Note)
		}
		for i, id := range ids {
			r := jrow("retirements", rel, off, i, true)
			r.add("ts", ts)
			r.add("session", session)
			r.add("kind", e.Kind)
			r.add("memory_id", nameValue(id))
			r.add("verdict", closedValue(e.Verdict, retireVerdicts))
			r.add("outcome", closedValue(e.Outcome, retireOutcomes))
			r.add("note_id", note)
			out = append(out, r)
		}
	case feedback.KindRecall:
		for i, it := range e.Items {
			r := jrow("recalls", rel, off, i, true)
			r.add("ts", ts)
			r.add("session", session)
			r.add("turn", turn)
			r.add("agent", agent)
			r.add("path", closedValue(e.Path, showPaths))
			r.add("memory_id", nameValue(it.ID))
			r.add("rank", intOrNil(it.Rank))
			r.add("rank_without_boost", intOrNil(it.RankWithoutBoost))
			r.add("reranker", realOrNil(it.Reranker))
			r.add("boost", realOrNil(it.Boost))
			var swb any
			if it.ScoreWithoutBoost != nil {
				swb = *it.ScoreWithoutBoost
			}
			r.add("score_without_boost", swb)
			r.add("flags", flagsValue(e.Flags))
			out = append(out, r)
		}
	case feedback.KindImport:
		source := closedValue(e.Source, importSources)
		for i, s := range e.Settled {
			r := jrow("imports", rel, off, i, true)
			r.add("ts", ts)
			r.add("source", source)
			r.add("key", keyValue(s.Key))
			r.add("memory_id", nameValue(s.ID))
			r.add("at", msOrNil(s.At))
			r.add("outcome", closedValue(s.Outcome, settleOutcomes))
			r.add("rank", intOrNil(s.Rank))
			out = append(out, r)
		}
		if e.Outcome != "" || len(e.Settled) == 0 { // the complete mark, after the items
			r := jrow("imports", rel, off, len(e.Settled), true)
			r.add("ts", ts)
			r.add("source", source)
			r.add("key", nil)
			r.add("memory_id", nil)
			r.add("at", nil)
			r.add("outcome", closedValue(e.Outcome, settleOutcomes))
			r.add("rank", nil)
			out = append(out, r)
		}
	default: // stale, and kinds a later binary writes
		r := jrow("journal_other", rel, off, 0, false)
		r.add("ts", ts)
		r.add("session", session)
		r.add("kind", shapeValue(e.Kind))
		out = append(out, r)
	}
	return out, ts, ""
}
