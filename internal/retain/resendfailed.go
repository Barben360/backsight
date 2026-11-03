package retain

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/attribution"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/transcript"
)

// Sessions sent while the server's extraction failed (issue 149). The
// server accepts a part before its extraction LLM reads it: with a key the
// provider rejects, the part fails there, leaving its document absent (a
// first part, given up 24 h later: replace_unconfirmed) or without that
// part (a later one, never reported). Such a session is sent again whole,
// as a re-attribution sends one (ApplyResend: its documents deleted, its
// records sent again under new operation ids): what failed and what did
// not, so no part is missing and none stored twice.

// ResendFailed is the reason of such a re-send.
const ResendFailed = "failed"

// FailedPlan is what retain resend-failed would send again.
type FailedPlan struct {
	// Named: the sessions a failed retain or a given-up part names.
	Named int `json:"named"`
	// NotHere: those whose transcript is not on this PC (another PC's,
	// or gone): nothing to send them again from.
	NotHere int `json:"not_here"`
	// Done: those sent again since their last failure.
	Done    int           `json:"done"`
	Held    []HeldSession `json:"held"`
	Resends []Resend      `json:"resends"`
	Cost    Estimate      `json:"cost"`
}

// PlanFailedResends plans a whole re-send of each session named (session
// id → its latest failure, as the server or this PC dated it) whose
// transcript is on this PC, unless it was sent again since (resent) or its
// own state holds it: quarantined, queued for backfill, a first part still
// pending, or continued on another PC, as a re-attribution holds it; or
// a directory of its records gone whose records cannot be filed as they
// were (goneFiledAsBefore: a re-attribution holds any). docs is
// the bank's listing: every conversation document of a session planned is
// deleted before its records go again. It writes nothing.
func (f *Flusher) PlanFailedResends(named map[string]time.Time, resent map[string]time.Time, docs []hindsight.DocumentItem, transcripts []string, prices Prices) FailedPlan {
	plan := FailedPlan{Named: len(named), Held: []HeldSession{}, Resends: []Resend{}}
	paths := map[string]string{}
	for _, p := range transcripts {
		paths[strings.TrimSuffix(filepath.Base(p), ".jsonl")] = p
	}
	byDoc := map[string][]hindsight.DocumentItem{}
	for _, d := range docs {
		if sid, _, ok := conversationDoc(d.ID); ok {
			byDoc[sid] = append(byDoc[sid], d)
		}
	}
	splits, splitErr := SplitSessions(f.Dirs.State)
	for _, sid := range slices.Sorted(maps.Keys(named)) {
		path, ok := paths[sid]
		switch {
		case !ok:
			plan.NotHere++
			continue
		case resent[sid].After(named[sid]):
			plan.Done++
			continue
		case splitErr != nil:
			plan.Held = append(plan.Held, HeldSession{Session: sid, Reason: "this PC's " + SplitSessionsFile(f.Dirs.State) + " cannot be read (" + splitErr.Error() + ")"})
			continue
		case len(splits[sid].Documents) > 0:
			plan.Held = append(plan.Held, HeldSession{Session: sid, Reason: HeldElsewhere})
			continue
		}
		if why := f.sessionHeld(sid); why != "" {
			plan.Held = append(plan.Held, HeldSession{Session: sid, Reason: why})
			continue
		}
		r, held := f.planFailedSession(sid, path, byDoc[sid], prices)
		switch {
		case held != "":
			plan.Held = append(plan.Held, HeldSession{Session: sid, Reason: held})
		case r != nil:
			plan.Resends = append(plan.Resends, *r)
			plan.Cost = plan.Cost.Add(r.Estimate)
		}
	}
	return plan
}

// planFailedSession is the whole re-send of one session: every record its
// transcript holds that is not kept out, under today's projects. nil when
// it holds none.
func (f *Flusher) planFailedSession(sid, path string, docs []hindsight.DocumentItem, prices Prices) (*Resend, string) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, "its transcript cannot be read: " + err.Error()
	}
	recs, _, err := transcript.Read(fh, 0)
	_ = fh.Close()
	if err != nil {
		return nil, "its transcript cannot be read: " + err.Error()
	}
	attrs := f.attribute(sid, recs, attr{})
	gone, why := f.goneFiledAsBefore(sid, recs, attrs)
	if why != "" {
		return nil, why
	}
	for _, d := range docs {
		if l := d.Metadata["last_uuid"]; l != "" && indexOf(recs, l) < 0 {
			// written from a transcript this machine does not hold: never
			// deleted from here
			return nil, HeldElsewhere
		}
	}
	r := &Resend{Session: sid, Transcript: path, Reason: ResendFailed, Documents: []string{}, Projects: []string{}, Gone: gone}
	for _, d := range docs {
		r.Documents = append(r.Documents, d.ID)
	}
	var chars int64
	for _, rec := range recs {
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
	if r.Records == 0 {
		return nil, ""
	}
	r.Estimate = EstimateSession(chars, prices)
	return r, ""
}

// goneFiledAsBefore checks the records of recs whose directory no longer
// exists (a removed worktree or clone), which a re-send deletes from the
// server and sends again under today's names (attrs, what the next flush
// gives them). Today's disk names such a directory by nothing better than
// its basename, so its records usually keep the previous record's project:
// right for a worktree of the repository the session was in, wrong for
// another's. So each directory's retained records must be filed today
// under the project they were filed under when they ran, as this PC
// recorded the session's turns (the attribution ledger):
//   - every turn holding one was recorded, so none of it was kept out of
//     memory then (a worktree of an excluded repository is not known to be
//     one any more);
//   - the turns where the directory holds most of the records agree on one
//     project, the directory's then (a directory named by its repository
//     files all its records under one project; a minority of a turn's
//     records says nothing of theirs);
//   - each of its records goes under that project today.
//
// It returns how many retained records lie in a removed directory, or,
// when one directory fails, why the session is held, naming each that
// does: deleting its documents first, the session cannot be sent without
// those records nor with them under a guess.
func (f *Flusher) goneFiledAsBefore(sid string, recs []transcript.Record, attrs map[int64]attr) (int, string) {
	type dir struct {
		recs   []int
		inTurn map[int]int // its records per turn (the turn's prompt index)
	}
	dirs := map[string]*dir{}
	var order []string
	seen := map[string]bool{}
	for _, r := range recs {
		if r.CWD == "" || seen[r.CWD] {
			continue
		}
		seen[r.CWD] = true
		if fi, err := os.Stat(r.CWD); err != nil || !fi.IsDir() {
			dirs[r.CWD] = &dir{inTurn: map[int]int{}}
			order = append(order, r.CWD)
		}
	}
	if len(order) == 0 {
		return 0, ""
	}
	filed := f.Attribution.filedTurns(sid)
	turn := make([]int, len(recs)) // the index of each record's prompt, -1 before the first
	size := map[int]int{}
	start := -1
	for i, r := range recs {
		if attribution.IsPrompt(r) {
			start = i
		}
		turn[i] = start
		size[start]++
		if d := dirs[r.CWD]; d != nil {
			d.recs = append(d.recs, i)
			d.inTurn[start]++
		}
	}
	filedAs := func(t int) (string, bool) {
		if t < 0 {
			return "", false
		}
		p, ok := filed[recs[t].UUID]
		return p, ok
	}
	var why []string
	n := 0
	for _, cwd := range order {
		d := dirs[cwd]
		var kept []int
		recorded := true
		for _, i := range d.recs {
			if a := attrs[recs[i].End]; a.out || f.headless(recs[i].Entrypoint) {
				continue // not sent again
			}
			kept = append(kept, i)
			if _, ok := filedAs(turn[i]); !ok {
				recorded = false
			}
		}
		if len(kept) == 0 {
			continue
		}
		was, several := "", false
		for t, c := range d.inTurn {
			if p, ok := filedAs(t); ok && 2*c > size[t] {
				several = several || (was != "" && was != p)
				was = cmpOr(was, p)
			}
		}
		switch {
		case !recorded || was == "":
			why = append(why, cwd+" (no record of the project its turns were filed under)")
			continue
		case several:
			why = append(why, cwd+" (its turns were filed under several projects)")
			continue
		}
		moved := ""
		for _, i := range kept {
			if p := attrs[recs[i].End].project; p != was {
				moved = p
				break
			}
		}
		if moved != "" {
			why = append(why, fmt.Sprintf("%s (its turns were filed under %s; today's rules would file them under %s)", cwd, was, moved))
			continue
		}
		n += len(kept)
	}
	if len(why) > 0 {
		return 0, HeldGone + ": " + strings.Join(why, "; ")
	}
	return n, ""
}

// SessionOfDocument is the session a conversation document belongs to.
func SessionOfDocument(id string) (string, bool) {
	sid, _, ok := conversationDoc(id)
	return sid, ok
}

// failedResendsFile records, per session, when retain resend-failed last
// sent it again: a failure older than that is repaired.
func failedResendsFile(stateDir string) string { return filepath.Join(stateDir, "resent-failed.json") }

// FailedResends reads that record; absent: none.
func FailedResends(stateDir string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	raw, err := os.ReadFile(failedResendsFile(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(raw, &out)
}

// RecordFailedResend records that session sid was sent again at at.
// Entries older than keep go.
func RecordFailedResend(stateDir, sid string, at time.Time, keep time.Duration) error {
	m, err := FailedResends(stateDir)
	if err != nil {
		m = map[string]time.Time{} // unreadable: rebuilt; a session may be planned again
	}
	for k, t := range m {
		if at.Sub(t) > keep {
			delete(m, k)
		}
	}
	m[sid] = at.UTC()
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(failedResendsFile(stateDir), raw, 0o600)
}
