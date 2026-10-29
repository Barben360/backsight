package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/recall"
)

// staleHidden: a memory hidden on this machine is listed once it stayed
// hidden this long without the server rewriting it; before, it waits.
const staleHidden = 7 * 24 * time.Hour

// staleMemory is one memory memory stale lists, with the command that
// moves it on.
type staleMemory struct {
	ID          string    `json:"id"`
	Shown       int       `json:"shown"`
	LastShown   time.Time `json:"last_shown,omitzero"`
	HiddenSince time.Time `json:"hidden_since,omitzero"`
	Wrong       int       `json:"wrong,omitempty"`
	Command     string    `json:"command"`
}

type staleResult struct {
	NeverUsed []staleMemory `json:"never_used"`
	Hidden    []staleMemory `json:"hidden"`
	Reported  []staleMemory `json:"reported"`
}

// ids are the memories r lists, in the order it shows them.
func (r staleResult) ids() []string {
	var out []string
	for _, ms := range [][]staleMemory{r.NeverUsed, r.Hidden, r.Reported} {
		for _, m := range ms {
			out = append(out, m.ID)
		}
	}
	return out
}

// newMemoryStaleCmd is `memory stale`: what the ledger (with what was
// journaled since its last settle) says needs the user's review. This
// machine's ledger only: the other machines' counts and hidings, which
// recall weighs, are theirs to list. Local files only, the ledger never
// written; it journals what it listed, best effort.
func newMemoryStaleCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "stale",
		Short: "List the memories to review: shown often and never used, hidden for 7 days, or reported wrong or outdated, with the command for each",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			l, err := feedback.Current(d.FeedbackDir())
			if err != nil {
				return blocked(err)
			}
			at := now()
			res := staleOf(l, cfg.Feedback.StaleShows, at)
			if ids := res.ids(); len(ids) > 0 && cfg.Feedback.Enabled {
				_ = feedback.NewJournal(d.FeedbackDir(), feedback.Common{Binary: buildinfo.Version}).Append(feedback.Event{Kind: feedback.KindStale, IDs: ids})
			}
			return e.emit("memory stale", res, func() string { return staleText(res, at) })
		},
	}
}

// staleOf sorts l's memories into what needs review, each once: hidden
// for staleHidden without a rewrite, else reported wrong or outdated,
// else shown live at least shows times and never used. A memory hidden for
// less is left out: the server may still rewrite it. Each list is most
// shown first.
func staleOf(l feedback.Ledger, shows int, at time.Time) staleResult {
	res := staleResult{NeverUsed: []staleMemory{}, Hidden: []staleMemory{}, Reported: []staleMemory{}}
	for id, m := range l.Memories {
		s := staleMemory{ID: id, Shown: m.Shown, LastShown: m.LastShown, Wrong: m.Wrong, Command: "backsight forget " + id}
		switch {
		case m.Hidden != nil && at.Sub(m.Hidden.Since) >= staleHidden:
			// Hidden because the server could not list its source facts:
			// a forget fails on it until it can, a restore always works.
			s.HiddenSince, s.Command = m.Hidden.Since, "backsight forget --restore "+id
			res.Hidden = append(res.Hidden, s)
		case m.Hidden != nil:
		case m.Wrong > 0:
			// Already invalidated: a plain forget changes nothing on the
			// server. --purge lists what deleting it for good takes, and
			// deletes only with --yes.
			s.Command += " --purge"
			res.Reported = append(res.Reported, s)
		case m.Shown-m.Imported.Shown >= shows && m.Credits == 0:
			// Live shows only: an import never counts a miss, so its
			// shows alone say nothing of use.
			res.NeverUsed = append(res.NeverUsed, s)
		}
	}
	for _, ms := range [][]staleMemory{res.NeverUsed, res.Hidden, res.Reported} {
		slices.SortFunc(ms, func(a, b staleMemory) int {
			if a.Shown != b.Shown {
				return b.Shown - a.Shown
			}
			return strings.Compare(a.ID, b.ID)
		})
	}
	return res
}

func staleText(r staleResult, at time.Time) string {
	if len(r.ids()) == 0 {
		return "Nothing needs review: no memory shown often and never used, hidden for 7 days, or reported wrong or outdated.\n"
	}
	var b strings.Builder
	for _, sec := range []struct {
		title, footer string
		ms            []staleMemory
	}{
		{"Shown often, never used", "", r.NeverUsed},
		{"Hidden, not rewritten for 7 days", "Hidden on this machine, so recall here skips them, until the server rewrites them; `backsight forget <id>` retires one for good once the server lists its source facts.", r.Hidden},
		{"Reported wrong or outdated", "", r.Reported},
	} {
		if len(sec.ms) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(sec.title + "\n")
		for _, m := range sec.ms {
			age := "-"
			if !m.LastShown.IsZero() {
				age = recall.Age(at.Sub(m.LastShown)) + " ago"
			}
			fmt.Fprintf(&b, "%s  shown %d  %s  %s\n", shortID(m.ID), m.Shown, age, m.Command)
		}
		if sec.footer != "" {
			b.WriteString(sec.footer + "\n")
		}
	}
	b.WriteString("\nbacksight forget --restore <id> undoes a retirement")
	if len(r.Reported) > 0 {
		b.WriteString("; --purge alone only lists what deleting for good would remove, and adding --yes deletes it")
	}
	b.WriteString(".\n")
	return b.String()
}
