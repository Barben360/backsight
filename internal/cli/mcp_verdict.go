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
	restoreAsk          = "Tell the user which memory you retired, why, and that `backsight forget --restore <id>` undoes it."
	sourcesAsk          = "Call memory_credit again with the verdict, naming the source facts that are wrong (or all of them)."
)

// sourceFact is one fact an observation is drawn from, as listed.
type sourceFact struct{ id, project, text string }

// retirePlan is what a verdict does with each id it names: retire (a raw
// fact), hide (no listable sources), skip (an observation whose facts
// named in the same call carry the verdict) or, for an observation none
// of whose facts is named, list its sources first.
type retirePlan struct {
	retire, hide, skip []string
	sources            map[string][]sourceFact // observation → its facts, in order
	listed             []string                // the observations of sources, in id order
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
func capRefusal(limit, done int, ids []string) error {
	lines := []string{fmt.Sprintf("retirements: at most %d per session (feedback.max_retirements), %d already: nothing stored; tell the user, who can retire them from a terminal:", limit, done)}
	for _, id := range ids {
		lines = append(lines, "backsight forget "+id)
	}
	return errors.New(strings.Join(lines, "\n"))
}

// retire carries out plan in id order after the correction note was
// stored: each raw fact invalidated with reason, each hidden one
// journaled. A server error stops it: the answer names what was retired
// and what was not, and only what was is journaled.
func (d mcpDeps) retire(ctx context.Context, key *mcpKey, live session.Live, verdict, note, reason string, ids []string, plan retirePlan) (string, error) {
	var retired, hidden, notRetired []string
	var failure error
	for i, id := range ids {
		switch {
		case slices.Contains(plan.hide, id):
			hidden = append(hidden, id)
			d.journal(live, feedback.Event{Kind: feedback.KindHide, IDs: []string{id}, Verdict: verdict, Note: note})
		case slices.Contains(plan.retire, id):
			failure = onServer(key, func(cl *hindsight.Client) error {
				return cl.Invalidate(ctx, d.cfg.Server.Bank, id, reason, false)
			})
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
	if failure != nil && len(retired)+len(hidden) == 0 {
		lines := []string{fmt.Sprintf("the server refused the retirement (%v): nothing retired; the correction is stored (%s), do not store it again; tell the user, who can retire them from a terminal:", failure, note)}
		for _, id := range notRetired {
			lines = append(lines, "backsight forget "+id)
		}
		return "", errors.New(strings.Join(lines, "\n"))
	}
	var b strings.Builder
	switch {
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
	if len(hidden) > 0 {
		b.WriteString("Hidden on this machine (the server listed no facts to retire): " + strings.Join(hidden, ", ") + "\n")
	}
	if len(plan.skip) > 0 {
		b.WriteString("Observations retired through the facts named: " + strings.Join(plan.skip, ", ") + "\n")
	}
	if len(notRetired) > 0 {
		b.WriteString("Not retired (the user can retire them from a terminal: backsight forget <id>): " + strings.Join(notRetired, ", ") + "\n")
	}
	b.WriteString(restoreAsk)
	return b.String(), nil
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

// journal appends e for the session this server belongs to, under its
// current turn (the hooks' pointer; the agent stays empty); a failure is
// logged, never fails the call.
func (d mcpDeps) journal(live session.Live, e feedback.Event) {
	dir := d.dirs.FeedbackDir()
	if t, ok := feedback.CurrentTurn(dir, live.SessionID); ok {
		e.Turn = t.ID
	}
	j := feedback.NewJournal(dir, feedback.Common{Session: live.SessionID, Project: live.Project, Binary: buildinfo.Version, V: instructions.Version})
	if err := j.Append(e); err != nil {
		d.tw.Emit(telemetry.Raw{Name: "feedback_error", Fields: map[string]any{"error": err.Error()}})
	}
}
