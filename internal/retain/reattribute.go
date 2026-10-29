package retain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/attribution"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/transcript"
)

// Re-attribution (decision 0072): the documents already sent are judged
// again with the rules that file new ones, so a rule change or an older
// build's labels never leave the memory diverging. A document whose whole
// content belongs to another project than its tag, under the same
// document id, is re-tagged in place (PATCH /documents/{id}: no
// extraction, though the server consolidates the moved memories again; its
// memories follow; its metadata does not, which no reader here relies
// on). A session whose documents mix projects, or hold records that now
// go under another document, is sent again: its documents are deleted and
// its records go out once more per project (extraction cost).
//
// The rules applied are the ones in force: the record rule for a
// session's turns (the path rule only runs in shadow on them) and the path
// rule for subagent reports, from the decision kept when the report was
// filed (its evidence, judged again) or from its transcript. Notes are
// never moved to another project (theirs was chosen); only a note tagged
// with the transverse keyword or a non-canonical alias is folded into the
// canonical alias, a label fix (issue 33). Only conversation and subagent
// documents whose transcript
// is on this machine are judged.

// Kinds of document a re-attribution judges.
const (
	KindSubagent     = "subagent"
	KindConversation = "conversation"
	KindNote         = "note" // a transverse label folded into the canonical alias
)

// Why a session is sent again.
const (
	ResendMixed = "mixed" // a document holds records of several projects
	ResendMoved = "moved" // its records now belong under another document
)

// Retag is a whole document moving to another project in place: no
// extraction (the server only rebuilds its observations).
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

// Why a session is not judged.
const (
	HeldQuarantined = "quarantined"     // retain retry-quarantine lifts it first
	HeldBackfill    = "backfill"        // the backfill queue sends it
	HeldPending     = "pending"         // a first segment the server has not stored yet
	HeldElsewhere   = "another machine" // a document holds records this transcript lacks, or this PC found it split
	HeldGone        = "directory gone"  // a directory of its records no longer exists: today's names would be guesses
)

// HeldSession is a session a re-attribution leaves alone, and why.
type HeldSession struct {
	Session string `json:"session"`
	Reason  string `json:"reason"`
}

// ReattributionPlan is what a re-attribution would do.
type ReattributionPlan struct {
	Documents int           `json:"documents"` // documents read
	Judged    int           `json:"judged"`    // with their transcript on this machine
	Unchanged int           `json:"unchanged"`
	Held      []HeldSession `json:"held"`
	Retags    []Retag       `json:"retags"`
	Resends   []Resend      `json:"resends"`
	Cost      Estimate      `json:"cost"` // of the re-sends
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
	plan := ReattributionPlan{Documents: len(docs), Held: []HeldSession{}, Retags: []Retag{}, Resends: []Resend{}}
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
	// A document this PC found split holds another PC's segments, under
	// this PC's last one: a re-send deletes it, and that PC takes its
	// records as held (they are in its sent index). Unreadable: every
	// session is held, as one may be split, under a reason naming the file.
	splits, splitErr := SplitSessions(f.Dirs.State)
	for _, sid := range order {
		path, ok := paths[sid]
		if !ok {
			continue
		}
		if splitErr != nil {
			plan.Held = append(plan.Held, HeldSession{Session: sid, Reason: "this PC's " + SplitSessionsFile(f.Dirs.State) + " cannot be read (" + splitErr.Error() + ")"})
			continue
		}
		if slices.ContainsFunc(sessions[sid], func(d hindsight.DocumentItem) bool { return slices.Contains(splits[sid].Documents, d.ID) }) {
			plan.Held = append(plan.Held, HeldSession{Session: sid, Reason: HeldElsewhere})
			continue
		}
		if why := f.sessionHeld(sid); why != "" {
			plan.Held = append(plan.Held, HeldSession{Session: sid, Reason: why})
			continue
		}
		retags, resend, unchanged, judged, held := f.planSession(sid, path, sessions[sid], prices)
		if held != "" {
			plan.Held = append(plan.Held, HeldSession{Session: sid, Reason: held})
			continue
		}
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
	// Notes: the keyword or an alias folds into the canonical alias; a
	// real project is never touched, and a note is not "judged".
	for _, d := range docs {
		from := tagProject(d.Tags)
		if !strings.HasPrefix(d.ID, "note:") || from == "" {
			continue
		}
		if to := CanonicalProject(a.Transverse, from); to != from {
			plan.Retags = append(plan.Retags, Retag{Document: d.ID, Kind: KindNote, From: from, To: to, Tags: retagged(d.Tags, to)})
		}
	}
	for _, d := range docs {
		sid, agent, ok := subagentDoc(d.ID)
		if !ok {
			continue
		}
		from := tagProject(d.Tags)
		e, known := a.loadAgent(sid, agent)
		switch {
		case known && !e.Provisional:
			e = a.Redecide(e)
		case d.Metadata["attribution"] != "":
			// decided when it was filed (the decision travels in its
			// metadata): never decided again from today's disk
			plan.Judged++
			plan.Unchanged++
			continue
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
func (f *Flusher) planSession(sid, path string, docs []hindsight.DocumentItem, prices Prices) (retags []Retag, resend *Resend, unchanged, judged int, held string) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, 0, ""
	}
	recs, _, err := transcript.Read(fh, 0)
	_ = fh.Close()
	if err != nil || len(recs) == 0 {
		return nil, nil, 0, 0, ""
	}
	// Today's names come from today's disk: a directory removed since (a
	// worktree, a clone) would name its records by a basename, worse than
	// the label they were sent with. Such a session is not judged.
	seen := map[string]bool{}
	for _, r := range recs {
		if r.CWD == "" || seen[r.CWD] {
			continue
		}
		seen[r.CWD] = true
		if fi, err := os.Stat(r.CWD); err != nil || !fi.IsDir() {
			return nil, nil, 0, 0, HeldGone
		}
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
			// Written from a transcript this machine does not hold (the
			// session went on elsewhere): never deleted from here, so no
			// re-send of this session either.
			return nil, nil, 0, 0, HeldElsewhere
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
		return retags, nil, unchanged, judged, ""
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
	return nil, r, unchanged, judged, ""
}

// sessionHeld says why a session's own state forbids judging it now:
// quarantined (refused by the server or an untrusted position: the user
// lifts it), queued for backfill, or a first segment still pending.
func (f *Flusher) sessionHeld(sid string) string {
	cur, ok, _ := LoadCursor(f.Dirs.CursorsDir(), sid)
	switch {
	case ok && cur.Quarantined:
		return HeldQuarantined
	case ok && cur.Backfill:
		return HeldBackfill
	case f.pendingMark(sid) != "":
		return HeldPending
	}
	return ""
}

// ReattributionGuard refuses to judge or write while the server may hold
// less than it will: a restore or an update not finished, a restore epoch
// not reconciled (Hold), or retains still queued (a document read now
// could gain a segment after a delete, which Hindsight would recreate
// holding that segment alone), or a connect since the run began.
func (f *Flusher) ReattributionGuard(ctx context.Context) error {
	if err := f.restoreHold(); err != nil {
		return err
	}
	// connect moved this PC to another server since the run began (its
	// backup takes minutes): nothing goes to the old one, and the cursors
	// are the new one's.
	if err := f.StartedOver(); err != nil {
		return err
	}
	if f.Hold != "" {
		return errors.New(f.Hold)
	}
	n, err := f.Client.QueuedOperations(ctx, f.Bank)
	if err != nil {
		return errors.New(errSummary(err))
	}
	if n > 0 {
		return fmt.Errorf("%d operation(s) still queued on the server: retried once they ran", n)
	}
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reattributionJournal records each write of a re-attribution, with what
// it replaced: a re-tag can be undone by hand from it.
// An "intent" line comes before the write, a "done" one after it: a run
// stopped in between leaves what it was about to replace.
func (f *Flusher) journal(action, phase string, fields map[string]any) error {
	fields["at"] = f.Now().UTC().Format(time.RFC3339)
	fields["action"], fields["phase"] = action, phase
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return appendFile(filepath.Join(f.Dirs.State, "reattribute.jsonl"), append(raw, '\n'))
}

// ApplyRetag moves one document to its new project in place, then keeps
// a report's decision so a later run judges the same evidence. The caller
// holds the maintenance lock.
// A conversation document is moved under its session's lock (a flush that
// started before the maintenance lock may still append to it). The move is
// read back: the document and every memory extracted from it must carry
// the new project, else it is a failure (the next run tries again).
func (f *Flusher) ApplyRetag(ctx context.Context, r Retag, oldTags []string) error {
	if err := f.StartedOver(); err != nil {
		return err
	}
	if r.Kind == KindConversation {
		unlock, err := f.lockSession(r.Session)
		if err != nil {
			return err
		}
		defer unlock()
	}
	fields := map[string]any{"document": r.Document, "from": r.From, "to": r.To, "old_tags": oldTags, "tags": r.Tags}
	if err := f.journal("retag", "intent", fields); err != nil {
		return fmt.Errorf("the journal cannot be written, nothing was moved: %w", err)
	}
	if err := f.Client.SetDocumentTags(ctx, f.Bank, r.Document, r.Tags); err != nil {
		return errors.New(errSummary(err))
	}
	// The server took the tags: the report's decision follows them, even
	// when reading the memories back then fails.
	if r.entry != nil {
		if err := f.Attribution.replaceAgent(*r.entry); err != nil {
			return err
		}
	}
	if err := f.checkTags(ctx, r.Document, "project:"+r.To); err != nil {
		return err
	}
	_ = f.journal("retag", "done", fields)
	return nil
}

// checkTags reads a moved document back: it, and each of its memories,
// must carry tag.
func (f *Flusher) checkTags(ctx context.Context, doc, tag string) error {
	d, err := f.Client.GetDocument(ctx, f.Bank, doc)
	if err != nil {
		return errors.New("moved, but reading it back failed: " + errSummary(err))
	}
	if !slices.Contains(d.Tags, tag) {
		return fmt.Errorf("the server did not apply %s to the document", tag)
	}
	missing := 0
	err = f.Client.ListMemories(ctx, f.Bank, hindsight.MemoryQuery{DocumentID: doc}, func(m hindsight.Memory) error {
		if !slices.Contains(m.Tags, tag) {
			missing++
		}
		return nil
	})
	if err != nil {
		return errors.New("moved, but reading its memories back failed: " + errSummary(err))
	}
	if missing > 0 {
		return fmt.Errorf("the server did not apply %s to %d of the document's memories", tag, missing)
	}
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
	// Nothing may land on a document after its delete: Hindsight would
	// recreate it holding that segment alone, which the resync trusts. So
	// no re-send while anything is queued (a re-tag's consolidation, a
	// note), and nothing changes here before it is known.
	if err := f.StartedOver(); err != nil {
		return err
	}
	if n, err := f.Client.QueuedOperations(ctx, f.Bank); err != nil {
		return errors.New(errSummary(err))
	} else if n > 0 {
		return fmt.Errorf("%w (%d)", ErrQueued, n)
	}
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
	// The generation salts the re-send's operation ids; it is kept for
	// good, before anything changes, so no later re-send reuses one.
	gen := max(f.loadDropped(r.Session).Generation, f.generation(r.Session))
	if cur, ok, _ := LoadCursor(f.Dirs.CursorsDir(), r.Session); ok {
		gen = max(gen, cur.Generation)
	}
	gen++
	if err := f.saveGeneration(r.Session, gen); err != nil {
		return err
	}
	mark := dropped{Records: map[string]bool{}, Generation: gen}
	var keys []string
	for _, rec := range recs {
		if k := ownKey(f.Index, f.Bank, r.Session, rec.UUID); k != "" {
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
	if err := f.journal("resend", "intent", map[string]any{"session": r.Session, "documents": r.Documents}); err != nil {
		return err
	}
	for _, doc := range r.Documents {
		err := f.Client.DeleteDocument(ctx, f.Bank, doc)
		var apiErr *hindsight.APIError
		if err != nil && (!errors.As(err, &apiErr) || apiErr.Kind != hindsight.NotFound) {
			return errors.New(errSummary(err))
		}
	}
	_ = f.journal("resend", "done", map[string]any{"session": r.Session, "documents": r.Documents, "reason": r.Reason, "records": r.Records, "projects": r.Projects})
	return nil
}

// ErrQueued: a re-send waits for the server's queue to empty; nothing was
// changed.
var ErrQueued = errors.New("operations are queued on the server: the re-send waits for them")

// generationPath keeps a session's last re-send generation for good: a
// later resync rebuilds the cursor, and the .dropped mark is pruned.
func generationPath(cursorDir, sid string) string { return filepath.Join(cursorDir, sid+".generation") }

func (f *Flusher) generation(sid string) int {
	raw, err := os.ReadFile(generationPath(f.Dirs.CursorsDir(), sid))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return n
}

func (f *Flusher) saveGeneration(sid string, n int) error {
	return fsutil.WriteAtomic(generationPath(f.Dirs.CursorsDir(), sid), []byte(strconv.Itoa(n)), 0o600)
}
