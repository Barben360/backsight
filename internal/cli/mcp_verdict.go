package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The retirement a wrong or outdated verdict carries out once its checks
// passed and its correction is stored (decision 0074): a raw fact is
// invalidated on the server (reversible, never purged); an observation is
// retired through the source facts the agent names, so a first call on
// one lists them; an observation whose sources cannot be listed is hidden
// on this machine. The MCP server journals retire and hide itself: only
// it knows what the server answered.

// memory_credit's answers to a verdict, stable for the PostToolUse hook:
// the first three are credits; Sources lists facts and is none.
const (
	creditRetiredPrefix = "Retired."
	creditPartlyPrefix  = "Partly retired."
	creditHiddenPrefix  = "Hidden."
	creditSourcesPrefix = "Sources:"
	maxReason           = 300 // characters of the invalidation reason, before its note
	sourceTextChars     = 300 // characters of a source fact's text in a Sources answer
	sourcesAsk          = "Call memory_credit again with the verdict, naming the source facts that are wrong (or all of them)."
)

// sourceFact is one fact an observation is drawn from, as listed.
type sourceFact struct{ id, project, text string }

// retirePlan is what a verdict does with each id it names: retire (a raw
// fact), hide (no listable sources; unread among them when the server
// could not be read), skip (an observation whose facts named in the same
// call carry the verdict) or, for an observation none of whose facts is
// named, list its sources first.
type retirePlan struct {
	retire, hide, unread, skip []string
	sources                    map[string][]sourceFact // observation → its facts, in order
	listed                     []string                // the observations of sources, in id order
}

// acted is how many memories the plan retires or hides: what counts
// toward feedback.max_retirements.
func (p retirePlan) acted() []string { return append(slices.Clone(p.retire), p.hide...) }

// onServer runs f with the server's client, once more after a key the
// server refused was rotated in secrets.toml.
func onServer(key *mcpKey, f func(*hindsight.Client) error) error {
	sec, eng := key.get()
	err := f(eng.Client)
	if unauthorized(err) && key.refresh(sec.APIKey) {
		_, eng = key.get()
		err = f(eng.Client)
	}
	return err
}

// classify reads every id from the server before anything is stored or
// retired: a world or experience fact is retired; an observation with
// sources is skipped when one of them is named too, else its sources are
// listed; a read that fails, another type, or no sources: hidden.
func (d mcpDeps) classify(ctx context.Context, key *mcpKey, ids []string, owner func(string) string) retirePlan {
	plan := retirePlan{sources: map[string][]sourceFact{}}
	for _, id := range ids {
		var m hindsight.MemoryDetail
		err := onServer(key, func(cl *hindsight.Client) (err error) {
			m, err = cl.GetMemory(ctx, d.cfg.Server.Bank, id)
			return err
		})
		switch {
		case err != nil:
			plan.hide = append(plan.hide, id)
			plan.unread = append(plan.unread, id)
		case m.Type == "world" || m.Type == "experience":
			plan.retire = append(plan.retire, id)
		case m.Type == "observation" && len(m.Sources) > 0:
			fallback := recall.ProjectOf(m.Tags)
			if fallback == "" {
				fallback = owner(id)
			}
			var facts []sourceFact
			named := false
			for _, s := range m.Sources {
				named = named || slices.Contains(ids, s.ID)
				p := recall.ProjectOf(s.Tags)
				if p == "" {
					p = fallback
				}
				facts = append(facts, sourceFact{id: s.ID, project: p, text: s.Text})
			}
			if named {
				plan.skip = append(plan.skip, id)
				continue
			}
			plan.sources[id] = facts
			plan.listed = append(plan.listed, id)
		default:
			plan.hide = append(plan.hide, id)
		}
	}
	return plan
}

// sourcesText is the answer to a first verdict on observations: the facts
// each is drawn from, one line each, for the agent to name the wrong ones.
// A fact's text is one line, so it cannot pose as another fact's line.
func sourcesText(p retirePlan) string {
	var b strings.Builder
	b.WriteString(creditSourcesPrefix + " only raw facts are retired, and an observation is drawn from them; nothing was retired or stored.\n")
	for _, obs := range p.listed {
		fmt.Fprintf(&b, "%s is drawn from:\n", shortID(obs))
		for _, s := range p.sources[obs] {
			fmt.Fprintf(&b, "- %s %s (%s)\n", memoryTag(s.project, s.id), clip(singleLine(s.text), sourceTextChars), safeProject(s.id))
		}
	}
	b.WriteString(sourcesAsk)
	return b.String()
}

// capRefusal is the refusal of a verdict over feedback.max_retirements,
// with the command that retires each memory from a terminal.
func capRefusal(limit, done int, ids []string, owner func(string) string) error {
	reason := fmt.Sprintf("at most %d retirements per session", limit)
	var lines []string
	for _, id := range ids {
		lines = append(lines, instructions.NotRetiredLine(safeProject(owner(id)), id, reason, true))
	}
	return fmt.Errorf("retirements: at most %d per session (feedback.max_retirements), %d already: nothing stored; the user can retire them from a terminal; %s\n%s",
		limit, done, printLines(lines), strings.Join(lines, "\n"))
}

// printLines asks the agent to print lines, the feedback lines a tool
// hands back.
func printLines(lines []string) string {
	if len(lines) == 1 {
		return "Print this line in your final message:"
	}
	return "Print these lines in your final message:"
}

// retire carries out plan in id order after the correction note was
// stored: each raw fact invalidated with reason, each hidden one
// journaled. A server error stops it: the answer names what was retired
// and what was not, and only what was is journaled.
func (d mcpDeps) retire(ctx context.Context, key *mcpKey, live session.Live, verdict, note, reason string, ids []string, plan retirePlan, owner func(string) string) (string, error) {
	var retired, hidden, notRetired []string
	var failure error
	local := false // the failure is this PC's sends gate, not the server's answer
	for i, id := range ids {
		switch {
		case slices.Contains(plan.hide, id):
			hidden = append(hidden, id)
			d.journal(live, feedback.Event{Kind: feedback.KindHide, IDs: []string{id}, Verdict: verdict, Note: note})
		case slices.Contains(plan.retire, id):
			// Each retirement checks the sends generation: a connect
			// meanwhile moved this PC to another server.
			if failure = sendsGate(d.dirs, d.generation)(); failure != nil {
				failure, local = joinedElsewhere(failure), true
			} else {
				failure = onServer(key, func(cl *hindsight.Client) error {
					return cl.Invalidate(ctx, d.cfg.Server.Bank, id, reason, false)
				})
			}
			if failure == nil {
				retired = append(retired, id)
				d.journal(live, feedback.Event{Kind: feedback.KindRetire, IDs: []string{id}, Verdict: verdict, Outcome: "invalidated", Note: note})
				continue
			}
			for _, rest := range ids[i:] {
				if !slices.Contains(plan.skip, rest) {
					notRetired = append(notRetired, rest)
				}
			}
		}
		if failure != nil {
			break
		}
	}
	moved := errors.Is(failure, errJoinedElsewhere)
	if failure != nil && moved && len(retired)+len(hidden) == 0 {
		return "", fmt.Errorf("%w: nothing retired. The correction is queued as %s: do not call memory_credit again for these memories", failure, note)
	}
	if failure != nil && local && len(retired)+len(hidden) == 0 {
		return "", fmt.Errorf("%w: nothing retired (backsight doctor). The correction is already stored as %s: do not call memory_credit again for these memories", failure, note)
	}
	if failure != nil && len(retired)+len(hidden) == 0 {
		var lines []string
		for _, id := range notRetired {
			lines = append(lines, instructions.NotRetiredLine(safeProject(owner(id)), id, "the server refused it", true))
		}
		return "", fmt.Errorf("the server refused the retirement (%w): nothing retired. The correction is already stored as %s: do not call memory_credit again for these memories; the user can retire them from a terminal; %s\n%s",
			failure, note, printLines(lines), strings.Join(lines, "\n"))
	}
	// Counts and enums only, as the answer decided them.
	switch {
	case failure != nil:
		d.tw.Emit(telemetry.Retire{Action: "partly", IDs: len(retired)})
	case len(retired) > 0:
		d.tw.Emit(telemetry.Retire{Action: "retired", IDs: len(retired)})
	}
	if len(hidden) > 0 {
		d.tw.Emit(telemetry.Retire{Action: "hidden", IDs: len(hidden)})
	}
	var b strings.Builder
	switch {
	case local && !moved:
		fmt.Fprintf(&b, "%s The rest was not sent: %v. Correction stored: %s.\n", creditPartlyPrefix, failure, note)
	case moved:
		fmt.Fprintf(&b, "%s This PC joined another server since this session started (restart Claude Code: its memory tools then use that server). Correction stored: %s.\n", creditPartlyPrefix, note)
	case failure != nil:
		fmt.Fprintf(&b, "%s The server refused a retirement (%v). Correction stored: %s.\n", creditPartlyPrefix, failure, note)
	case len(retired) > 0:
		fmt.Fprintf(&b, "%s Correction stored: %s.\n", creditRetiredPrefix, note)
	default:
		fmt.Fprintf(&b, "%s Correction stored: %s.\n", creditHiddenPrefix, note)
	}
	if len(retired) > 0 {
		b.WriteString("Invalidated on the server: " + strings.Join(retired, ", ") + "\n")
	}
	var empty, unread []string
	for _, id := range hidden {
		if slices.Contains(plan.unread, id) {
			unread = append(unread, id)
		} else {
			empty = append(empty, id)
		}
	}
	if len(empty) > 0 {
		b.WriteString("Hidden on this machine (the server lists no facts to retire): " + strings.Join(empty, ", ") + "\n")
	}
	if len(unread) > 0 {
		b.WriteString("Hidden on this machine only (the server could not be read; the user can retire them on the server from a terminal: backsight forget <id>): ")
		for i, id := range unread {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("backsight forget " + id)
		}
		b.WriteString("\n")
	}
	if len(plan.skip) > 0 {
		b.WriteString("Observations retired through the facts named: " + strings.Join(plan.skip, ", ") + "\n")
	}
	switch {
	case len(notRetired) > 0 && moved:
		b.WriteString("Not retired (this PC no longer writes to that server): " + strings.Join(notRetired, ", ") + "\n")
	case len(notRetired) > 0:
		b.WriteString("Not retired (the user can retire them from a terminal: backsight forget <id>): " + strings.Join(notRetired, ", ") + "\n")
	}
	// The lines to print, in id order: one per memory the call names.
	notReason, retireCmd := "the server refused a retirement", true
	switch {
	case moved:
		notReason, retireCmd = "this PC joined another server since this session started", false
	case local:
		notReason = "not sent from this PC (backsight doctor)"
	}
	var lines []string
	for _, id := range ids {
		p := safeProject(owner(id))
		switch {
		case slices.Contains(retired, id):
			lines = append(lines, instructions.RetiredLine(verdict, p, id))
		case slices.Contains(hidden, id):
			lines = append(lines, instructions.HiddenLine(verdict, p, id, slices.Contains(plan.unread, id)))
		case slices.Contains(notRetired, id):
			lines = append(lines, instructions.NotRetiredLine(p, id, notReason, retireCmd))
		}
	}
	if len(lines) > 0 {
		b.WriteString(printLines(lines) + "\n" + strings.Join(lines, "\n"))
	}
	return b.String(), nil
}

// errJoinedElsewhere: connect moved this PC to another server since this
// memory server loaded its config (retain.ErrStartedOver, said for the
// agent).
var errJoinedElsewhere = errors.New("this PC joined another server since this session started (restart Claude Code: its memory tools then use that server)")

// joinedElsewhere is a failed sends gate, as a memory tool says it.
func joinedElsewhere(err error) error {
	if errors.Is(err, retain.ErrStartedOver) {
		return errJoinedElsewhere
	}
	return fmt.Errorf("cannot read this PC's sends generation (%w)", err)
}

// invalidationReason is "<verdict>: <evidence> → <correction>" on one
// line, cut to maxReason characters, then the note it comes with.
func invalidationReason(verdict, evidence, correction, note string) string {
	return clip(singleLine(verdict+": "+evidence+" → "+correction), maxReason) + " (" + note + ")"
}

// singleLine is s with its newlines and control characters collapsed into
// single spaces.
func singleLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// journal appends a retire or hide e for the session this server belongs
// to, under its current turn (the hooks' pointer; the agent stays empty),
// under the ledger lock; a failure is logged, never fails the call.
func (d mcpDeps) journal(live session.Live, e feedback.Event) {
	dir := d.dirs.FeedbackDir()
	if t, ok := feedback.CurrentTurn(dir, live.SessionID); ok {
		e.Turn = t.ID
	}
	j := feedback.NewJournal(dir, feedback.Common{Session: live.SessionID, Project: live.Project, Binary: buildinfo.Version, V: instructions.Version})
	if err := j.AppendFolded(e); err != nil {
		d.tw.Emit(telemetry.FeedbackError(telemetry.FeedbackReasonJournalWrite))
	}
}
