package retain

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/transcript"
)

// titleKinds maps the leading word of a saved note's title to its kind
// (French and English, decision 0051).
var titleKinds = []struct{ prefix, kind string }{
	{"décision", "decision"}, {"decision", "decision"},
	{"correction", "correction"},
	{"état", "state"}, {"etat", "state"}, {"state", "state"},
	{"convention", "convention"},
	{"piège", "pitfall"}, {"piege", "pitfall"}, {"pitfall", "pitfall"},
	{"leçon", "lesson"}, {"lecon", "lesson"}, {"lesson", "lesson"},
}

// TransverseKeyword names the transverse project in tool calls, whatever
// the configured aliases are.
const TransverseKeyword = "transverse"

// IsPool: p names the transverse pool, the keyword or one of its aliases.
func IsPool(aliases []string, p string) bool {
	return len(aliases) > 0 && (p == TransverseKeyword || slices.Contains(aliases, p))
}

// CanonicalProject maps the keyword and every transverse alias to the first
// alias (one pool, SPEC §4); any other name is kept. The live note path
// (backsight store, memory_store) and the replay of saved notes both use it.
func CanonicalProject(aliases []string, p string) string {
	if IsPool(aliases, p) {
		return aliases[0]
	}
	return p
}

func kindFromTitle(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	for _, k := range titleKinds {
		if strings.HasPrefix(t, k.prefix) {
			return k.kind
		}
	}
	return "lesson"
}

func str(in map[string]json.RawMessage, key string) string {
	var v string
	_ = json.Unmarshal(in[key], &v)
	return strings.TrimSpace(v)
}

// NotesFromRecords turns explicit saves found in a session (memory_store
// and hindsight_ingest_document calls) into notes, dated by their record
// and identified by session and tool call so a rerun collapses (0051).
// projectOf names the project of a record; ok false skips the record
// (kept out). transverse are the configured aliases: a save naming the
// pool, or made from one of its directories, goes to the canonical alias,
// as the live path files it (a note without a project keeps the project of
// its record, as it was accepted then: a replay stays faithful).
func NotesFromRecords(bank, sid string, recs []transcript.Record, transverse []string, projectOf func(r transcript.Record) (project string, ok bool)) []Note {
	var notes []Note
	for _, r := range recs {
		sessionProject, ok := projectOf(r)
		if !ok {
			continue
		}
		for _, tu := range r.Tools {
			var kind, project, content string
			switch {
			case strings.HasSuffix(tu.Name, "memory_store"):
				content = str(tu.Input, "text")
				if content == "" {
					content = str(tu.Input, "content")
				}
				kind, project = str(tu.Input, "kind"), str(tu.Input, "project")
			case strings.HasSuffix(tu.Name, "hindsight_ingest_document"):
				title, body := str(tu.Input, "title"), str(tu.Input, "content")
				content = strings.TrimSpace(title + "\n" + body)
				kind = kindFromTitle(title)
				var tags []string
				_ = json.Unmarshal(tu.Input["tags"], &tags)
				for _, t := range tags {
					if p, ok := strings.CutPrefix(t, "project:"); ok && p != "" {
						project = p
						break
					}
				}
			default:
				continue
			}
			if content == "" {
				continue
			}
			if !Kinds[kind] {
				kind = "lesson"
			}
			if project == "" {
				project = sessionProject
			}
			project = CanonicalProject(transverse, project)
			content, _ = redact.Text(content)
			n := NewNote(bank, kind, project, content, r.Timestamp)
			n.DocumentID = "note:" + uuid5(nsBacksight, bank+"\n"+sid+"\n"+tu.ID)
			n.OperationID = uuid5(nsBacksight, bank+"\n"+n.DocumentID+"\n"+content)
			notes = append(notes, n)
		}
	}
	return notes
}
