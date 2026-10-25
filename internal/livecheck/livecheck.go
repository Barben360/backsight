// Package livecheck is `verify --live` (0068, 0067 block 4): read-only
// against the configured server, it checks that this machine's latest
// settled session was sent (its document exists, with the segment this
// machine recorded) and that consolidation formed an observation from it.
package livecheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/retain"
)

// Server is what the check reads; *hindsight.Client is one.
type Server interface {
	GetDocument(ctx context.Context, bank, id string) (hindsight.Document, error)
	ListMemories(ctx context.Context, bank string, q hindsight.MemoryQuery, fn func(hindsight.Memory) error) error
	QueuedOperations(ctx context.Context, bank string) (int, error)
}

// Input is this machine's side: its transcripts and cursors.
type Input struct {
	Server      Server
	Bank        string
	Transcripts []string
	CursorsDir  string
	RetainSince time.Time // sessions last written before it are backfill's
	Now         time.Time
}

// Statuses.
const (
	Pass    = "pass"
	Pending = "pending" // not settled, not sent or not consolidated yet: rerun after WaitS
	Fail    = "fail"
)

const (
	// TickEvery is jobs tick's cadence (scheduler: every 15 minutes).
	TickEvery = 15 * time.Minute
	// sendGrace: a settled session not sent after two ticks is a failure.
	sendGrace = 2 * TickEvery
	// workWait: how long an extraction or a consolidation is given once
	// the server has stopped working on its queue.
	workWait = 30 * time.Minute
	// pollWait is the rerun delay suggested while the server works.
	pollWait = 5 * time.Minute
	// lookback: earlier sessions searched for an observation when the
	// latest one's facts formed none.
	lookback = 5
)

// Result is the verdict and what it rests on.
type Result struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
	Fix     string `json:"fix,omitempty"`
	// WaitS: when pending, the seconds after which a rerun can tell.
	WaitS         int       `json:"wait_s,omitempty"`
	Session       string    `json:"session,omitempty"`
	Transcript    string    `json:"transcript,omitempty"`
	Document      string    `json:"document,omitempty"`
	Segment       int       `json:"segment,omitempty"`        // this machine's cursor
	ServerSegment int       `json:"server_segment,omitempty"` // the document's metadata
	SettledAt     time.Time `json:"settled_at,omitzero"`
	SentAt        time.Time `json:"sent_at,omitzero"`
	Facts         int       `json:"facts"`
	Observations  int       `json:"observations"`
	// ObservedIn: the earlier session whose facts gave the observation,
	// when the latest one's formed none.
	ObservedIn string `json:"observed_in,omitempty"`
}

type session struct {
	sid, path string
	written   time.Time
	size      int64
	cur       retain.Cursor
}

func (s session) settledAt() time.Time { return s.written.Add(retain.SettleAfter) }

// Check runs the live check. It only reads.
func Check(ctx context.Context, in Input) Result {
	var settled []session
	var live *session
	for _, p := range in.Transcripts {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		s := session{sid: strings.TrimSuffix(filepath.Base(p), ".jsonl"), path: p, written: fi.ModTime(), size: fi.Size()}
		if !in.RetainSince.IsZero() && s.written.Before(in.RetainSince) {
			continue // backfill's
		}
		if in.Now.Sub(s.written) < retain.SettleAfter {
			if live == nil || s.written.After(live.written) {
				live = &s
			}
			continue
		}
		settled = append(settled, s)
	}
	sort.Slice(settled, func(i, j int) bool { return settled[i].written.After(settled[j].written) })

	var target *session
	var earlier []session
	for _, s := range settled {
		cur, ok, err := retain.LoadCursor(in.CursorsDir, s.sid)
		if err != nil {
			if target == nil {
				return Result{Status: Fail, Session: s.sid, Transcript: s.path, Summary: "cannot read the cursor of session " + s.sid, Detail: err.Error(), Fix: "backsight doctor"}
			}
			continue
		}
		s.cur = cur
		switch {
		case ok && cur.Backfill:
			continue // started before live retain: backfill sends it
		case ok && cur.Quarantined && target == nil:
			// Even with earlier segments out: the rest is held.
			return Result{Status: Fail, Session: s.sid, Transcript: s.path, SettledAt: s.settledAt(),
				Summary: "the latest settled session " + s.sid + " is quarantined: the server refused it",
				Fix:     "backsight retain retry-quarantine (after fixing the reason shown by backsight doctor)"}
		case ok && !cur.Quarantined && cur.Bank == in.Bank && len(cur.SentDocuments(s.sid)) > 0:
			if target == nil {
				target = &s
			} else {
				earlier = append(earlier, s)
			}
		case target != nil:
			continue // only sent sessions help an earlier proof
		case ok && cur.Bank == in.Bank && cur.Offset >= s.size:
			continue // handled without sending: opted out or paused
		default:
			return notSent(s, in.Now)
		}
		if len(earlier) >= lookback {
			break
		}
	}
	if target == nil {
		if live != nil {
			wait := live.settledAt().Add(TickEvery).Sub(in.Now)
			return Result{Status: Pending, Session: live.sid, Transcript: live.path, SettledAt: live.settledAt(), WaitS: seconds(wait),
				Summary: "the latest session " + live.sid + " is not settled yet: it is sent 30 min after its last turn, at the next jobs tick"}
		}
		return Result{Status: Pending, WaitS: seconds(retain.SettleAfter + TickEvery),
			Summary: "no session from this machine to check yet",
			Fix:     "use Claude Code in a project, then rerun backsight verify --live 45 min after the last turn"}
	}
	return checkSent(ctx, in, *target, earlier)
}

// notSent: a settled session with nothing sent. The next tick sends it;
// after two ticks without it, sending is broken.
func notSent(s session, now time.Time) Result {
	r := Result{Session: s.sid, Transcript: s.path, SettledAt: s.settledAt()}
	if now.Before(s.settledAt().Add(sendGrace)) {
		r.Status, r.WaitS = Pending, seconds(s.settledAt().Add(TickEvery).Sub(now))
		r.Summary = "the latest settled session " + s.sid + " is not sent yet: the next jobs tick sends it"
		return r
	}
	r.Status = Fail
	r.Summary = fmt.Sprintf("the latest settled session %s was not sent (settled %s UTC)", s.sid, s.settledAt().UTC().Format("2006-01-02 15:04"))
	r.Detail = "jobs tick sends settled sessions every 15 minutes"
	r.Fix = "backsight doctor (jobs, writes, server); backsight jobs tick sends it now"
	return r
}

func checkSent(ctx context.Context, in Input, s session, earlier []session) Result {
	// A session has one document per project it sent records of (retain
	// DocumentID): each must hold what this machine sent to it. The result
	// names the home document, or the one that does not match.
	docs := s.cur.SentDocuments(s.sid)
	r := Result{Session: s.sid, Transcript: s.path, Document: docs[0].Document, Segment: docs[0].Segment, SettledAt: s.settledAt(), SentAt: s.cur.SentAt}
	fail := func(summary, detail, fix string) Result {
		r.Status, r.Summary, r.Detail, r.Fix = Fail, summary, detail, fix
		return r
	}
	// While the server works on it: a rerun can tell in a few minutes.
	pending := func(summary string) Result {
		r.Status, r.Summary, r.WaitS = Pending, summary, seconds(pollWait)
		return r
	}
	unreadable := func(err error) Result {
		return fail("cannot read the server", err.Error(), "backsight server status")
	}
	queued := -1
	busy := func() bool {
		if queued < 0 {
			n, err := in.Server.QueuedOperations(ctx, in.Bank)
			if err != nil {
				n = 0
			}
			queued = n
		}
		return queued > 0
	}
	recent := in.Now.Sub(s.cur.SentAt) < workWait

	homeSegment := 0
	for i, sd := range docs {
		r.Document, r.Segment = sd.Document, sd.Segment
		doc, err := in.Server.GetDocument(ctx, in.Bank, sd.Document)
		var apiErr *hindsight.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
			if busy() || recent {
				return pending(fmt.Sprintf("session %s was sent but %s is not stored yet (%d operation(s) queued on the server)", s.sid, sd.Document, max(queued, 0)))
			}
			return fail("the server has no document "+sd.Document+", which this machine sent on "+s.cur.SentAt.UTC().Format("2006-01-02 15:04")+" UTC",
				"nothing is queued on the server, so it is not coming",
				"backsight doctor (restore-epoch, bank): after a restore, jobs tick re-sends what the server lacks")
		case err != nil:
			return unreadable(err)
		}
		seg, _ := strconv.Atoi(doc.Metadata["segment"])
		r.ServerSegment = seg
		switch {
		case seg < sd.Segment && (busy() || recent):
			return pending(fmt.Sprintf("session %s: the server holds segment %d of %s, segment %d is still being stored", s.sid, seg, sd.Document, sd.Segment))
		case seg != sd.Segment:
			return fail(fmt.Sprintf("session %s: the server holds segment %d of %s, this machine sent segment %d", s.sid, seg, sd.Document, sd.Segment),
				"the document "+sd.Document+" does not hold what this machine recorded as sent", "backsight doctor (restore-epoch, writes)")
		case doc.Metadata["last_uuid"] != sd.LastUUID:
			return fail("session "+s.sid+": the server's document "+sd.Document+" does not match what this machine sent",
				fmt.Sprintf("segment %d ends at record %s on the server, %s here", seg, doc.Metadata["last_uuid"], sd.LastUUID), "backsight doctor (restore-epoch, writes)")
		}
		if i == 0 {
			homeSegment = seg
		}
	}
	r.Document, r.Segment, r.ServerSegment = docs[0].Document, docs[0].Segment, homeSegment

	facts, err := sessionFacts(ctx, in, s)
	if err != nil {
		return unreadable(err)
	}
	r.Facts = len(facts)
	var unconsolidated, failed int
	for _, f := range facts {
		switch {
		case f.ConsolidatedAt != "":
		case f.ConsolidationFailedAt != "":
			failed++
		default:
			unconsolidated++
		}
	}
	switch {
	case len(facts) == 0 && (busy() || recent):
		return pending("session " + s.sid + " is stored; its facts are still being extracted")
	case failed > 0:
		return fail(fmt.Sprintf("consolidation failed for %d of %d fact(s) of session %s", failed, len(facts), s.sid), "",
			"backsight server llm check; the Hindsight UI's operations page shows the provider's error")
	case unconsolidated > 0 && (busy() || recent):
		return pending(fmt.Sprintf("%d of %d fact(s) of session %s are not consolidated yet", unconsolidated, len(facts), s.sid))
	case unconsolidated > 0:
		return fail(fmt.Sprintf("%d of %d fact(s) of session %s are not consolidated, %s after it was sent", unconsolidated, len(facts), s.sid, in.Now.Sub(s.cur.SentAt).Round(time.Minute)),
			"nothing is queued on the server", "backsight doctor (bank); the Hindsight UI's operations page")
	}
	n, err := observationsFrom(ctx, in, facts, s.cur.SentAt)
	if err != nil {
		return unreadable(err)
	}
	r.Observations = n
	if n > 0 {
		r.Status = Pass
		r.Summary = fmt.Sprintf("session %s reached the server (%s) and %d observation(s) were consolidated from its %d fact(s)", s.sid, docList(docs), n, len(facts))
		return r
	}
	// Its facts formed no new observation (none extracted, transient, or
	// already known): an earlier session of this machine can still prove
	// that consolidation works.
	for _, e := range earlier {
		ef, err := sessionFacts(ctx, in, e)
		if err != nil {
			return unreadable(err)
		}
		m, err := observationsFrom(ctx, in, ef, e.cur.SentAt)
		if err != nil {
			return unreadable(err)
		}
		if m > 0 {
			r.Status, r.Observations, r.ObservedIn = Pass, m, e.sid
			r.Summary = fmt.Sprintf("session %s reached the server (%s); consolidation works", s.sid, docList(docs))
			r.Detail = fmt.Sprintf("its %d fact(s) formed no new observation (transient or already known); %d observation(s) came from the earlier session %s", len(facts), m, e.sid)
			return r
		}
	}
	return fail(fmt.Sprintf("no observation was consolidated from this machine's last %d sent session(s)", 1+len(earlier)),
		fmt.Sprintf("session %s: %d fact(s), all consolidated, none in an observation", s.sid, len(facts)),
		"backsight doctor (bank-config: the bank's missions decide what becomes an observation; backsight bank apply restores them)")
}

// sessionFacts lists the facts extracted from every document of a session.
func sessionFacts(ctx context.Context, in Input, s session) ([]hindsight.Memory, error) {
	var out []hindsight.Memory
	for _, sd := range s.cur.SentDocuments(s.sid) {
		f, err := factsOf(ctx, in, sd.Document)
		if err != nil {
			return nil, err
		}
		out = append(out, f...)
	}
	return out, nil
}

// docList names a session's documents with their segments.
func docList(docs []retain.SentDocument) string {
	parts := make([]string, 0, len(docs))
	for _, d := range docs {
		parts = append(parts, fmt.Sprintf("document %s, segment %d", d.Document, d.Segment))
	}
	return strings.Join(parts, "; ")
}

// factsOf lists the facts extracted from one document.
func factsOf(ctx context.Context, in Input, doc string) ([]hindsight.Memory, error) {
	var out []hindsight.Memory
	err := in.Server.ListMemories(ctx, in.Bank, hindsight.MemoryQuery{DocumentID: doc}, func(m hindsight.Memory) error {
		if m.FactType != "observation" {
			out = append(out, m)
		}
		return nil
	})
	return out, err
}

// observationsFrom counts the observations written since the send that
// cite one of facts as a source.
func observationsFrom(ctx context.Context, in Input, facts []hindsight.Memory, sentAt time.Time) (int, error) {
	if len(facts) == 0 {
		return 0, nil
	}
	ids := map[string]bool{}
	for _, f := range facts {
		ids[f.ID] = true
	}
	n := 0
	q := hindsight.MemoryQuery{Type: "observation"}
	if !sentAt.IsZero() {
		q.UpdatedSince = sentAt.Add(-5 * time.Minute) // clocks of client and server differ a little
	}
	err := in.Server.ListMemories(ctx, in.Bank, q, func(m hindsight.Memory) error {
		for _, src := range m.SourceMemoryIDs {
			if ids[src] {
				n++
				break
			}
		}
		return nil
	})
	return n, err
}

// seconds is a wait rounded up to the minute, at least one minute.
func seconds(d time.Duration) int {
	if d < time.Minute {
		d = time.Minute
	}
	return int((d + time.Minute - 1).Truncate(time.Minute) / time.Second)
}
