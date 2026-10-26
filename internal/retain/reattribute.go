package retain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/attribution"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/transcript"
)

// Re-attribution (decision 0072): the documents already sent are judged
// again with the rules that file new ones, so a rule change or an older
// build's labels never leave the memory diverging. A document whose whole
// content belongs to another project than its tag, under the same
// document id, is re-tagged in place (PATCH /documents/{id}: free, its
// memories follow; its metadata does not, which no reader here relies
// on). A session whose documents mix projects, or hold records that now
// go under another document, is sent again: its documents are deleted and
// its records go out once more per project (extraction cost).
//
// The rules applied are the ones in force: the record rule for a
// session's turns (the path rule only runs in shadow on them) and the path
// rule for subagent reports, from the decision kept when the report was
// filed (its evidence, judged again) or from its transcript. Notes are
// never moved: their project was chosen. Only documents whose transcript
// is on this machine are judged.

// Kinds of document a re-attribution judges.
const (
	KindSubagent     = "subagent"
	KindConversation = "conversation"
)

// Why a session is sent again.
const (
	ResendMixed = "mixed" // a document holds records of several projects
	ResendMoved = "moved" // its records now belong under another document
)

// Retag is a whole document moving to another project, free.
type Retag struct {
	Document string   `json:"document"`
	Session  string   `json:"session"`
	Kind     string   `json:"kind"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Tags     []string `json:"tags"` // the document's new tags
	// entry: the report's decision, kept when the move is applied.
	entry *TurnEntry
}

// Resend is a session sent again, per project.
type Resend struct {
	Session    string   `json:"session"`
	Transcript string   `json:"-"`
	Documents  []string `json:"documents"` // deleted, the records sent again
	Reason     string   `json:"reason"`
	Projects   []string `json:"projects"` // where its records go now
	Records    int      `json:"records"`
	// KeptOut: records the server holds that are now kept out of memory
	// (an excluded path, a pause): not sent again.
	KeptOut  int      `json:"kept_out,omitempty"`
	Estimate Estimate `json:"estimate"`
}

// ReattributionPlan is what a re-attribution would do.
type ReattributionPlan struct {
	Documents int      `json:"documents"` // documents read
	Judged    int      `json:"judged"`    // with their transcript on this machine
	Unchanged int      `json:"unchanged"`
	Retags    []Retag  `json:"retags"`
	Resends   []Resend `json:"resends"`
	Cost      Estimate `json:"cost"` // of the re-sends
	// decided: reports decided from their transcript, kept when applied so
	// a later run judges the same evidence.
	decided []TurnEntry
}

// conversationDoc parses conversation:<sid>[@<project>][#<n>].
func conversationDoc(id string) (sid, project string, ok bool) {
	rest, found := strings.CutPrefix(BaseDocumentID(id), "conversation:")
	if !found {
		return "", "", false
	}
	sid, project, _ = strings.Cut(rest, "@")
	return sid, project, session.ValidID(sid)
}

// subagentDoc parses subagent:<sid>:<agent>.
func subagentDoc(id string) (sid, agent string, ok bool) {
	rest, found := strings.CutPrefix(id, "subagent:")
	if !found {
		return "", "", false
	}
	sid, agent, _ = strings.Cut(rest, ":")
	return sid, agent, session.ValidID(sid) && session.ValidID(agent)
}

// tagProject is the project a document's tags name.
func tagProject(tags []string) string {
	for _, t := range tags {
		if p, ok := strings.CutPrefix(t, "project:"); ok {
			return p
		}
	}
	return ""
}

// retagged is tags with its project tag replaced by to's.
func retagged(tags []string, to string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if strings.HasPrefix(t, "project:") {
			t = "project:" + to
		}
		out = append(out, t)
	}
	return out
}

// PlanReattribution judges docs (the bank's listing) against the rules in
// force. transcripts are this machine's session transcripts. It writes
// nothing.
func (f *Flusher) PlanReattribution(docs []hindsight.DocumentItem, transcripts []string, prices Prices) ReattributionPlan {
	plan := ReattributionPlan{Documents: len(docs), Retags: []Retag{}, Resends: []Resend{}}
	paths := map[string]string{}
	for _, p := range transcripts {
		paths[strings.TrimSuffix(filepath.Base(p), ".jsonl")] = p
	}
	sessions := map[string][]hindsight.DocumentItem{}
	var order []string
	for _, d := range docs {
		if sid, _, ok := conversationDoc(d.ID); ok {
			if _, seen := sessions[sid]; !seen {
				order = append(order, sid)
			}
			sessions[sid] = append(sessions[sid], d)
		}
	}
	for _, sid := range order {
		path, ok := paths[sid]
		if !ok {
			continue
		}
		retags, resend, unchanged, judged := f.planSession(sid, path, sessions[sid], prices)
		plan.Judged += judged
		plan.Unchanged += unchanged
		plan.Retags = append(plan.Retags, retags...)
		if resend != nil {
			plan.Resends = append(plan.Resends, *resend)
			plan.Cost = plan.Cost.Add(resend.Estimate)
		}
	}
	type sessionRepos struct {
		launch string
		repos  *attribution.Repos
	}
	cache := map[string]*sessionRepos{}
	a := f.Attribution
	for _, d := range docs {
		sid, agent, ok := subagentDoc(d.ID)
		if !ok {
			continue
		}
		from := tagProject(d.Tags)
		e, known := a.loadAgent(sid, agent)
		switch {
		case known:
			e = a.Redecide(e)
		case paths[sid] == "":
			continue // not this machine's session
		default:
			agentPath := filepath.Join(strings.TrimSuffix(paths[sid], ".jsonl"), "subagents", "agent-"+agent+".jsonl")
			if _, err := os.Stat(agentPath); err != nil {
				continue
			}
			c := cache[sid]
			if c == nil {
				c = &sessionRepos{launch: launchDir(paths[sid])}
				cache[sid] = c
			}
			repos := func() (*attribution.Repos, bool) {
				if c.repos == nil {
					c.repos, _ = a.repos(c.launch, paths[sid])
				}
				return c.repos, true
			}
			at := f.Now()
			if fi, err := os.Stat(agentPath); err == nil {
				at = fi.ModTime()
			}
			// what the hook filed it under is its session directory's project
			e = a.computeSubagent(sid, agent, c.launch, repos, agentPath, from, "", at)
			plan.decided = append(plan.decided, e)
		}
		plan.Judged++
		to := a.ReportProject(e)
		if to == from || from == "" {
			plan.Unchanged++
			continue
		}
		e.Applied = to
		plan.Retags = append(plan.Retags, Retag{Document: d.ID, Session: sid, Kind: KindSubagent, From: from, To: to, Tags: retagged(d.Tags, to), entry: &e})
	}
	return plan
}

// planSession judges the conversation documents of one session: judged is
// how many it could read against the transcript.
func (f *Flusher) planSession(sid, path string, docs []hindsight.DocumentItem, prices Prices) (retags []Retag, resend *Resend, unchanged, judged int) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, 0
	}
	recs, _, err := transcript.Read(fh, 0)
	_ = fh.Close()
	if err != nil || len(recs) == 0 {
		return nil, nil, 0, 0
	}
	attrs := f.attribute(sid, recs, attr{})
	home := ""
	if cur, ok, _ := LoadCursor(f.Dirs.CursorsDir(), sid); ok && cur.Segments != nil {
		home = cur.Home
	}
	for _, d := range docs {
		if home == "" && BaseDocumentID(d.ID) == "conversation:"+sid {
			home = d.Metadata["project"]
		}
	}
	if home == "" {
		home = homeOf(recs, attrs)
	}
	why := ""
	for _, d := range docs {
		last := indexOf(recs, d.Metadata["last_uuid"])
		if last < 0 {
			continue // written from a transcript this machine does not hold
		}
		judged++
		layout, perProject := d.Metadata["project"]
		legacy := -1
		if !perProject {
			legacy = last
		} else if l := d.Metadata["legacy_last_uuid"]; l != "" {
			legacy = indexOf(recs, l)
		}
		var projects []string
		for i := 0; i <= last; i++ {
			a := attrs[recs[i].End]
			if (i <= legacy || (perProject && a.project == layout)) && !a.out && !f.headless(recs[i].Entrypoint) && !slices.Contains(projects, a.project) {
				projects = append(projects, a.project)
			}
		}
		_, idProject, _ := conversationDoc(d.ID)
		switch {
		case len(projects) > 1:
			why = ResendMixed
		case len(projects) == 0 && perProject:
			why = cmpOr(why, ResendMoved) // its project's records are named otherwise now
		case len(projects) == 0:
			unchanged++ // nothing it holds is retained any more: forget's business
		case DocumentID(sid, home, projects[0]) != DocumentID(sid, home, cmpOr(idProject, home)):
			why = cmpOr(why, ResendMoved)
		case tagProject(d.Tags) != projects[0]:
			retags = append(retags, Retag{Document: d.ID, Session: sid, Kind: KindConversation, From: tagProject(d.Tags), To: projects[0], Tags: retagged(d.Tags, projects[0])})
		default:
			unchanged++
		}
	}
	if why == "" {
		return retags, nil, unchanged, judged
	}
	// The whole session goes out again: every document of it is deleted.
	r := &Resend{Session: sid, Transcript: path, Reason: why}
	for _, d := range docs {
		r.Documents = append(r.Documents, d.ID)
	}
	// What the deleted documents hold goes out again: every record up to
	// the furthest one they reach, but those now kept out.
	end := -1
	for _, d := range docs {
		end = max(end, indexOf(recs, d.Metadata["last_uuid"]))
	}
	var chars int64
	for _, rec := range recs[:end+1] {
		a := attrs[rec.End]
		if a.out || f.headless(rec.Entrypoint) {
			r.KeptOut++
			continue
		}
		r.Records++
		if !slices.Contains(r.Projects, a.project) {
			r.Projects = append(r.Projects, a.project)
		}
		for _, t := range rec.Turns {
			chars += int64(len(t.Content))
		}
	}
	r.Estimate = EstimateSession(chars, prices)
	return nil, r, unchanged, judged
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reattributionJournal records each write of a re-attribution, with what
// it replaced: a re-tag can be undone by hand from it.
func (f *Flusher) journal(action string, fields map[string]any) {
	fields["at"] = f.Now().UTC().Format(time.RFC3339)
	fields["action"] = action
	raw, err := json.Marshal(fields)
	if err == nil {
		_ = appendFile(filepath.Join(f.Dirs.State, "reattribute.jsonl"), append(raw, '\n'))
	}
}

// ApplyRetag moves one document to its new project in place, then keeps
// a report's decision so a later run judges the same evidence. The caller
// holds the maintenance lock.
func (f *Flusher) ApplyRetag(ctx context.Context, r Retag, oldTags []string) error {
	if err := f.Client.SetDocumentTags(ctx, f.Bank, r.Document, r.Tags); err != nil {
		return errors.New(errSummary(err))
	}
	if r.entry != nil {
		if err := f.Attribution.replaceAgent(*r.entry); err != nil {
			return err
		}
	}
	f.journal("retag", map[string]any{"document": r.Document, "from": r.From, "to": r.To, "old_tags": oldTags, "tags": r.Tags})
	return nil
}

// KeepDecisions saves the reports' decisions a plan computed from their
// transcripts, moved or not: later runs judge the same evidence even once
// a transcript or a worktree is gone.
func (f *Flusher) KeepDecisions(p ReattributionPlan) {
	for _, e := range p.decided {
		_, _ = f.Attribution.keepAgent(e)
	}
}

// ApplyResend queues one session to be sent again per project: its
// records are marked lost (the restore path: their session keys dropped,
// a .dropped mark so the fork guard lets them out), its cursor removed,
// then its documents deleted. The next flush resyncs and sends them under
// today's rules. The marks come first: stopped before the deletes, the
// server still holds the records and the resync marks them sent again;
// the next run plans the same re-send. The caller holds the maintenance
// lock (no flush runs meanwhile).
func (f *Flusher) ApplyResend(ctx context.Context, r Resend) error {
	unlock, err := f.lockSession(r.Session)
	if err != nil {
		return err
	}
	defer unlock()
	fh, err := os.Open(r.Transcript)
	if err != nil {
		return err
	}
	recs, _, err := transcript.Read(fh, 0)
	_ = fh.Close()
	if err != nil {
		return err
	}
	gen := f.loadDropped(r.Session).Generation
	if cur, ok, _ := LoadCursor(f.Dirs.CursorsDir(), r.Session); ok {
		gen = max(gen, cur.Generation)
	}
	mark := dropped{Records: map[string]bool{}, Generation: gen + 1}
	var keys []string
	for _, rec := range recs {
		if k := sessionKey(f.Bank, r.Session, rec.UUID); f.Index.Has(k) {
			mark.Records[rec.UUID] = true
			keys = append(keys, k)
		}
	}
	mark.PastSince = len(recs) > 0 && !f.RetainSince.IsZero() && recs[0].Timestamp.Before(f.RetainSince)
	if err := f.addDropped(r.Session, mark); err != nil {
		return err
	}
	if err := f.Index.Remove(keys...); err != nil {
		return err
	}
	if p, err := cursorPath(f.Dirs.CursorsDir(), r.Session); err == nil {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	_ = os.Remove(pendingPath(f.Dirs.CursorsDir(), r.Session))
	for _, doc := range r.Documents {
		err := f.Client.DeleteDocument(ctx, f.Bank, doc)
		var apiErr *hindsight.APIError
		if err != nil && (!errors.As(err, &apiErr) || apiErr.Kind != hindsight.NotFound) {
			return errors.New(errSummary(err))
		}
	}
	f.journal("resend", map[string]any{"session": r.Session, "documents": r.Documents, "reason": r.Reason, "records": r.Records, "projects": r.Projects})
	return nil
}
