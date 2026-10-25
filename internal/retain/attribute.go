package retain

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/transcript"
)

// attr is what the project rule says of one record: its project, whether
// it is kept out of retain (and why), and namedOut, why the record that
// named its project was kept out ("" when it was retained).
type attr struct {
	project  string
	out      bool
	why      string
	namedOut string
}

// attribute names the project of each record of recs, keyed by its End
// offset (unique in a file), and says which records are kept out; prev is
// the record before recs[0] (the zero attr at the file's start). This is
// the one place the project rule lives (decision 0071, C2a):
//   - a record whose directory has a strong name (Resolver.Named: transverse
//     path, .backsight.toml project, git repository) takes it; so does a
//     session's first directory (no previous project) by its basename, and
//     a directory whose basename is the current project already;
//   - any other keeps the previous record's project, so a cd into
//     ~/.npm-global/... or /tmp does not start a project; a record with no
//     directory keeps it too (it is kept out anyway);
//   - a record is kept out when its pause or its directory says so (pauses,
//     OptedOut), and also when it keeps a project named by a record kept
//     out: privacy fails closed, so a plain directory after an excluded
//     repository (or one entered while paused) is neither sent nor labelled
//     with that repository's name, until a record names a project again.
//
// It reads the records, their directories' git repositories and
// .backsight.toml files and the session's pause state; never the cursor or
// the server, so a resync names the records as the incremental flushes
// did, as long as none of those changed. Headless entrypoints are judged by
// sendRuns: they keep a session out, not a project.
func (f *Flusher) attribute(sid string, recs []transcript.Record, prev attr) map[int64]attr {
	type dir struct {
		name   string
		strong bool
		out    bool
		why    string
	}
	pauses := f.pauses(sid)
	dirs := map[string]dir{}
	out := make(map[int64]attr, len(recs))
	for _, r := range recs {
		d, ok := dirs[r.CWD]
		if !ok {
			d.out, d.why = f.Resolver.OptedOut(r.CWD, f.Exclude)
			if strings.TrimSpace(r.CWD) != "" {
				d.name, d.strong = f.Resolver.Named(r.CWD)
			}
			dirs[r.CWD] = d
		}
		a := attr{project: prev.project, out: d.out, why: d.why, namedOut: prev.namedOut}
		if paused, why := pauses(r); paused {
			a.out, a.why = true, why
		}
		if d.name != "" && (d.strong || prev.project == "" || d.name == prev.project) {
			a.project, a.namedOut = d.name, ""
			if a.out {
				a.namedOut = a.why
			}
		} else if a.namedOut != "" && !a.out {
			a.out, a.why = true, a.namedOut
		}
		out[r.End] = a
		prev = a
	}
	return out
}

// homeOf is the session's home project: its first named record's, kept out
// or not (a home document named conversation:<sid> leaks no name).
func homeOf(recs []transcript.Record, attrs map[int64]attr) string {
	for _, r := range recs {
		if p := attrs[r.End].project; p != "" {
			return p
		}
	}
	return ""
}

// projectsOf lists home, then every other project of recs' retained records
// once, in order: the documents a resync reads (a kept-out project's name
// never reaches the server, not even in a read).
func projectsOf(home string, recs []transcript.Record, attrs map[int64]attr) []string {
	out := []string{home}
	seen := map[string]bool{home: true}
	for _, r := range recs {
		if a := attrs[r.End]; a.project != "" && !a.out && !seen[a.project] {
			seen[a.project] = true
			out = append(out, a.project)
		}
	}
	return out
}

// serverDoc is what the server holds of one of a session's documents.
type serverDoc struct {
	project, id string
	found       bool
	lastUUID    string
	segment     string
	// legacy: the record up to which this document holds every project's
	// records (a build from before per-project documents wrote it).
	legacy string
}

// readDocuments reads the session's document of each project; a missing
// one is not an error, any other failure is.
func (f *Flusher) readDocuments(ctx context.Context, sid, home string, projects []string) ([]serverDoc, error) {
	out := make([]serverDoc, 0, len(projects))
	for _, p := range projects {
		d := serverDoc{project: p, id: DocumentID(sid, home, p)}
		doc, err := f.Client.GetDocument(ctx, f.Bank, d.id)
		var apiErr *hindsight.APIError
		switch {
		case err == nil:
			m := doc.Metadata
			d.found, d.lastUUID, d.segment, d.legacy = true, m["last_uuid"], m["segment"], m["legacy_last_uuid"]
			if _, layout := m["project"]; !layout {
				d.legacy = d.lastUUID // written before per-project documents
			}
		case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
		default:
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// position is what the server holds of a session, per document.
type position struct {
	at       map[string]int // project → index in recs of its document's last_uuid
	segments map[string]int // project → its document's last segment
	// legacy: the index up to which conversation:<sid> holds every
	// project's records, -1 when it does not; legacyUUID that record.
	legacy     int
	legacyUUID string
	found      bool // the server holds at least one of the documents
}

// holds says whether the server holds record i of project p: p's document
// reaches it, or the legacy part of conversation:<sid> does.
func (p position) holds(i int, project string) bool {
	at, ok := p.at[project]
	return (ok && i <= at) || i <= p.legacy
}

// serverPosition reads docs against recs. bad, when not empty, is why the
// position cannot be trusted (a record recs lack, an invalid segment): the
// session is quarantined, since sending would duplicate or replace.
func serverPosition(recs []transcript.Record, docs []serverDoc, homeDoc string) (position, string) {
	pos := position{at: map[string]int{}, segments: map[string]int{}, legacy: -1}
	for _, d := range docs {
		if !d.found {
			continue
		}
		i := indexOf(recs, d.lastUUID)
		if i < 0 {
			return pos, "the server holds this session up to record " + d.lastUUID + ", which this transcript does not contain"
		}
		seg, err := strconv.Atoi(d.segment)
		if err != nil || seg < 1 {
			return pos, "the server's document " + d.id + " has no valid segment number; sending would replace it"
		}
		pos.at[d.project], pos.segments[d.project], pos.found = i, seg, true
		if d.id == homeDoc && d.legacy != "" {
			if pos.legacy = indexOf(recs, d.legacy); pos.legacy < 0 {
				return pos, "the server holds this session up to record " + d.legacy + ", which this transcript does not contain"
			}
			pos.legacyUUID = d.legacy
		}
	}
	return pos, ""
}
