package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
)

// newFeedbackCmd is `feedback`: the journal of the memories shown to
// Claude and the ones it reported using, and the per-memory counts the
// tick settles from it.
func newFeedbackCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feedback",
		Short: "Read which memories were shown to Claude and which it used, and recompute their counts",
	}
	cmd.AddCommand(newFeedbackLogCmd(e), newFeedbackRebuildCmd(e), newFeedbackImportCmd(e), newFeedbackReplayCmd(e))
	return cmd
}

// feedbackSetup is the feedback directory and the ledger's parameters; a
// machine without config.toml uses the defaults (these commands only read
// local files).
func feedbackSetup() (string, feedback.Params, error) {
	dir, cfg, err := feedbackConfig()
	return dir, feedbackParams(cfg), err
}

// feedbackConfig is the feedback directory and this machine's
// configuration, the defaults without config.toml.
func feedbackConfig() (string, config.Config, error) {
	d, home, err := machine()
	if err != nil {
		return "", config.Config{}, err
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", config.Config{}, blocked(err)
	}
	return d.FeedbackDir(), cfg, nil
}

// logEvent is one line of a timeline: a journal event narrowed to the
// memory asked for, or one settled show of a settle event.
type logEvent struct {
	TS      time.Time       `json:"ts"`
	Kind    string          `json:"kind"`
	Session string          `json:"session,omitempty"`
	Turn    string          `json:"turn,omitempty"`
	N       int             `json:"turn_number,omitempty"`
	Agent   string          `json:"agent,omitempty"`
	Machine bool            `json:"machine,omitempty"`
	Path    string          `json:"path,omitempty"`
	Items   []feedback.Item `json:"items,omitempty"`
	IDs     []string        `json:"ids,omitempty"`
	Verdict string          `json:"verdict,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
	Flags   []string        `json:"flags,omitempty"`
	ID      string          `json:"id,omitempty"`      // settle: the memory settled
	ShownAt time.Time       `json:"shown_at,omitzero"` // settle: when that show was
}

type logResult struct {
	ID      string     `json:"id,omitempty"`
	Session string     `json:"session,omitempty"`
	Since   time.Time  `json:"since,omitzero"`
	Events  []logEvent `json:"events"`
	// Entry: with --id, the ledger's entry, credits and misses read at At.
	Entry       *feedback.Entry `json:"entry,omitempty"`
	At          time.Time       `json:"at,omitzero"`
	LedgerError string          `json:"ledger_error,omitempty"`
	Skipped     int             `json:"skipped,omitempty"` // journal lines that did not parse
}

func newFeedbackLogCmd(e *env) *cobra.Command {
	var id, sess, since string
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show the timeline of one memory or one session: when it was shown, at which rank, credited, settled",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			if id == "" && sess == "" {
				sess = os.Getenv("BACKSIGHT_SESSION")
			}
			if id == "" && sess == "" {
				return &output.Error{Code: output.Usage, Message: "give --id <memory id> or --session <session id>"}
			}
			if sess != "" && !session.ValidID(sess) {
				return &output.Error{Code: output.Usage, Message: "invalid session id " + sess}
			}
			var from time.Time
			if since != "" {
				t, err := parseWhen(since)
				if err != nil {
					return &output.Error{Code: output.Usage, Message: "--since: " + err.Error()}
				}
				from = t
			}
			dir, p, err := feedbackSetup()
			if err != nil {
				return err
			}
			events, skipped, err := feedback.Read(dir, from)
			if err != nil {
				return blocked(err)
			}
			if id != "" {
				if id, err = resolveLogged(id, events); err != nil {
					return &output.Error{Code: output.Usage, Message: err.Error()}
				}
			}
			if sess != "" {
				if sess, err = resolveSession(sess, events); err != nil {
					return &output.Error{Code: output.Usage, Message: err.Error()}
				}
			}
			res := logResult{ID: id, Session: sess, Since: from, Events: timeline(events, id, sess, from), Skipped: skipped}
			if id != "" {
				l, err := feedback.LoadLedger(dir)
				if err != nil {
					res.LedgerError = err.Error()
				} else if entry, ok := l.Memories[id]; ok {
					res.At = now().UTC()
					entry = entry.DecayedTo(res.At, p)
					res.Entry = &entry
				}
			}
			return e.emit("feedback log", res, func() string { return logText(res) })
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "memory id, or its first 8 characters or more")
	cmd.Flags().StringVar(&sess, "session", "", "session id, or its first 8 characters or more (default without --id: $BACKSIGHT_SESSION)")
	cmd.Flags().StringVar(&since, "since", "", "only events from then on (RFC 3339, YYYY-MM-DDTHH:MMZ or YYYY-MM-DD, UTC)")
	return cmd
}

// resolveLogged is the memory id ref names among the ids the journal
// holds: the id, or a prefix of 8 characters or more that one id starts
// with. An id the journal does not hold is kept: the ledger may.
func resolveLogged(ref string, events []feedback.Event) (string, error) {
	var known []feedback.Show
	for _, e := range events {
		for _, it := range e.Items {
			known = append(known, feedback.Show{ID: it.ID})
		}
		for _, id := range e.IDs {
			known = append(known, feedback.Show{ID: id})
		}
	}
	id, err := feedback.Resolve(ref, known)
	switch {
	case errors.Is(err, feedback.ErrAmbiguous):
		return "", fmt.Errorf("%s starts several memory ids: give more characters", ref)
	case err != nil:
		return ref, nil
	}
	return id, nil
}

// resolveSession is the session ref names among the journal's sessions:
// the session itself, or a prefix of 8 characters or more that one
// session starts with. A session the journal does not hold is kept.
func resolveSession(ref string, events []feedback.Event) (string, error) {
	if len(ref) < shortIDChars || slices.ContainsFunc(events, func(e feedback.Event) bool { return e.Session == ref }) {
		return ref, nil
	}
	match := ""
	for _, e := range events {
		if strings.HasPrefix(e.Session, ref) && e.Session != match {
			if match != "" {
				return "", fmt.Errorf("%s starts several sessions: give more characters", ref)
			}
			match = e.Session
		}
	}
	if match == "" {
		return ref, nil
	}
	return match, nil
}

// timeline is the events of memory id and of session sess ("" for any)
// from since on, oldest first; each settled show of a settle event is a
// line of its own, under the session and turn of its show.
func timeline(events []feedback.Event, id, sess string, since time.Time) []logEvent {
	turnN := map[string]int{} // session|turn: the prompt's number
	for _, e := range events {
		if e.Kind == "turn" {
			turnN[e.Session+"|"+e.Turn] = e.N
		}
	}
	sorted := slices.Clone(events)
	slices.SortStableFunc(sorted, func(a, b feedback.Event) int {
		if c := a.TS.Compare(b.TS); c != 0 {
			return c
		}
		return int(a.Seq - b.Seq)
	})
	out := []logEvent{}
	for _, e := range sorted {
		if e.TS.Before(since) {
			continue
		}
		ev := logEvent{TS: e.TS, Kind: e.Kind, Session: e.Session, Turn: e.Turn, Agent: e.Agent, Machine: e.Machine, Path: e.Path,
			Items: e.Items, IDs: e.IDs, Verdict: e.Verdict, Outcome: e.Outcome, Flags: e.Flags}
		switch e.Kind {
		case "settle":
			for _, s := range e.Settled {
				ss, turn := showOf(s.Key)
				if id != "" && s.ID != id || sess != "" && ss != sess {
					continue
				}
				out = append(out, logEvent{TS: e.TS, Kind: e.Kind, Session: ss, Turn: turn, N: turnN[ss+"|"+turn], Outcome: s.Outcome, ID: s.ID, ShownAt: s.At})
			}
			continue
		case "shows":
			if id != "" {
				i := slices.IndexFunc(e.Items, func(it feedback.Item) bool { return it.ID == id })
				if i < 0 {
					continue
				}
				ev.Items = e.Items[i : i+1]
			}
		case "credit", "forget":
			if id != "" {
				if !slices.Contains(e.IDs, id) {
					continue
				}
				ev.IDs = []string{id}
			}
		default: // turn, stop: no memory named
			if id != "" {
				continue
			}
		}
		if sess != "" && e.Session != sess {
			continue
		}
		if e.Turn != "" {
			ev.N = turnN[e.Session+"|"+e.Turn]
		}
		out = append(out, ev)
	}
	return out
}

// showOf is the session and turn of a settled show's key
// (session|turn|<ts>|id, see feedback.Show).
func showOf(key string) (sess, turn string) {
	parts := strings.SplitN(key, "|", 3)
	if len(parts) < 3 {
		return "", ""
	}
	return parts[0], parts[1]
}

func logText(r logResult) string {
	var b strings.Builder
	if len(r.Events) == 0 {
		what := "session " + r.Session
		if r.ID != "" {
			what = "memory " + r.ID
		}
		fmt.Fprintf(&b, "nothing journaled for %s\n", what)
	}
	for _, ev := range r.Events {
		turn := "-"
		if ev.N > 0 {
			turn = fmt.Sprintf("turn %d", ev.N)
		}
		fmt.Fprintf(&b, "%s  %-8s  %-8s  %-6s  %s\n", ev.TS.UTC().Format("2006-01-02 15:04:05"), shortID(ev.Session), turn, ev.Kind, logDetail(ev, r.ID == ""))
	}
	if r.Skipped > 0 {
		fmt.Fprintf(&b, "%d journal line(s) skipped: torn by a crash, or written by another version\n", r.Skipped)
	}
	if r.ID == "" {
		return b.String()
	}
	switch {
	case r.LedgerError != "":
		fmt.Fprintf(&b, "ledger unreadable (%s): backsight feedback rebuild recomputes it\n", r.LedgerError)
	case r.Entry == nil:
		fmt.Fprintf(&b, "ledger  %s: no entry (nothing of it settled yet, or it was retired)\n", shortID(r.ID))
	default:
		en := r.Entry
		fmt.Fprintf(&b, "ledger  %s: shown %d, credits %.2f, misses %.2f (decayed to %s)", shortID(r.ID), en.Shown, en.Credits, en.Misses, r.At.Format("2006-01-02 15:04"))
		if !en.LastShown.IsZero() {
			fmt.Fprintf(&b, ", last shown %s", en.LastShown.UTC().Format("2006-01-02 15:04"))
		}
		if !en.LastCredited.IsZero() {
			fmt.Fprintf(&b, ", last credited %s", en.LastCredited.UTC().Format("2006-01-02 15:04"))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// logDetail is what an event says: path and ranks, verdict or outcome;
// withIDs names the memories (a session's timeline).
func logDetail(ev logEvent, withIDs bool) string {
	var parts []string
	switch ev.Kind {
	case "turn":
		parts = append(parts, "prompt")
		if ev.Machine {
			parts[0] = "message from Claude Code"
		}
	case "shows":
		parts = append(parts, ev.Path)
		var items []string
		for _, it := range ev.Items {
			var s []string
			if withIDs {
				s = append(s, shortID(it.ID))
			}
			if it.Rank > 0 {
				s = append(s, fmt.Sprintf("#%d", it.Rank))
			}
			if len(s) > 0 {
				items = append(items, strings.Join(s, " "))
			}
		}
		if len(items) > 0 {
			parts = append(parts, strings.Join(items, ", "))
		}
	case "credit":
		parts = append(parts, ev.Verdict)
		if withIDs && len(ev.IDs) > 0 {
			parts = append(parts, shortIDs(ev.IDs))
		}
	case "stop":
		parts = append(parts, ev.Outcome)
		if len(ev.Flags) > 0 {
			parts = append(parts, "("+strings.Join(ev.Flags, ", ")+")")
		}
	case "settle":
		if withIDs {
			parts = append(parts, shortID(ev.ID))
		}
		parts = append(parts, ev.Outcome)
	case "forget":
		parts = append(parts, "retired: dropped from the ledger")
		if withIDs {
			parts = append(parts, shortIDs(ev.IDs))
		}
	}
	if ev.Agent != "" {
		parts = append(parts, "(subagent "+shortID(ev.Agent)+")")
	}
	return strings.Join(parts, " ")
}

func shortIDs(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = shortID(id)
	}
	return strings.Join(out, ", ")
}

// ledgerTotals sums a ledger, credits and misses read at one time.
type ledgerTotals struct {
	Memories int     `json:"memories"`
	Shown    int     `json:"shown"`
	Credits  float64 `json:"credits"`
	Misses   float64 `json:"misses"`
}

type rebuildResult struct {
	Plan     bool      `json:"plan"`
	NoImport bool      `json:"no_import,omitempty"`
	At       time.Time `json:"at"` // credits and misses read at it
	// Rebuilt is the journal's fold, Current the ledger before.
	Rebuilt      ledgerTotals `json:"rebuilt"`
	Current      ledgerTotals `json:"current"`
	CurrentError string       `json:"current_error,omitempty"`
	New          int          `json:"new"`
	Dropped      int          `json:"dropped"`
	Changed      int          `json:"changed"`
}

func newFeedbackRebuildCmd(e *env) *cobra.Command {
	var plan, noImport bool
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Recompute every memory's counts of times shown, credited and missed from the journal",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			dir, p, err := feedbackSetup()
			if err != nil {
				return err
			}
			// Read before the rebuild takes the lock: a tick settling in
			// between shows as a difference.
			current, curErr := feedback.LoadLedger(dir)
			var rebuilt feedback.Ledger
			if plan {
				// Reads only, without the lock: a plan never waits for a tick.
				events, _, err := feedback.Read(dir, time.Time{})
				if err != nil {
					return blocked(err)
				}
				if noImport {
					events = feedback.DropImports(events)
				}
				rebuilt = feedback.Fold(events, p)
			} else if rebuilt, err = feedback.Rebuild(dir, p, true, !noImport); err != nil {
				var held *state.HeldError
				if errors.As(err, &held) {
					return &output.Error{Code: output.Blocked, Message: "the ledger is being settled (" + held.Owner.Command + "): try again in a moment"}
				}
				return blocked(err)
			}
			res := compareLedgers(rebuilt, current, p, now().UTC())
			res.Plan, res.NoImport = plan, noImport
			if curErr != nil {
				res.CurrentError = curErr.Error()
			}
			return e.emit("feedback rebuild", res, func() string { return rebuildText(res) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show the counts and the difference with the current ledger, writing nothing")
	cmd.Flags().BoolVar(&noImport, "no-import", false, "leave out the counts feedback import added from earlier history")
	return cmd
}

// compareLedgers totals both ledgers read at t and counts the memories
// only the rebuilt one holds, only the current one holds, and that differ.
func compareLedgers(rebuilt, current feedback.Ledger, p feedback.Params, t time.Time) rebuildResult {
	res := rebuildResult{At: t, Rebuilt: totals(rebuilt, p, t), Current: totals(current, p, t)}
	same := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	// An entry holding only a restore time is no memory (RestoreOnly).
	for id, r := range rebuilt.Memories {
		if r.RestoreOnly() {
			continue
		}
		c, ok := current.Memories[id]
		if !ok || c.RestoreOnly() {
			res.New++
			continue
		}
		r, c = r.DecayedTo(t, p), c.DecayedTo(t, p)
		if r.Shown != c.Shown || !same(r.Credits, c.Credits) || !same(r.Misses, c.Misses) ||
			!r.LastShown.Equal(c.LastShown) || !r.LastCredited.Equal(c.LastCredited) {
			res.Changed++
		}
	}
	for id, c := range current.Memories {
		if r, ok := rebuilt.Memories[id]; !c.RestoreOnly() && (!ok || r.RestoreOnly()) {
			res.Dropped++
		}
	}
	return res
}

func totals(l feedback.Ledger, p feedback.Params, t time.Time) ledgerTotals {
	tot := ledgerTotals{Memories: l.Counted()}
	for _, m := range l.Memories {
		m = m.DecayedTo(t, p)
		tot.Shown += m.Shown
		tot.Credits += m.Credits
		tot.Misses += m.Misses
	}
	return tot
}

func rebuildText(r rebuildResult) string {
	var b strings.Builder
	before := "ledger now"
	if r.Plan {
		fmt.Fprintf(&b, "Rebuild plan, nothing written (credits and misses as of %s UTC)\n", r.At.Format("2006-01-02 15:04"))
	} else {
		before = "ledger before"
		fmt.Fprintf(&b, "Ledger rebuilt from the journal (credits and misses as of %s UTC)\n", r.At.Format("2006-01-02 15:04"))
	}
	if r.NoImport {
		b.WriteString("without the counts feedback import added from earlier history\n")
	}
	fmt.Fprintf(&b, "%-10s  %-12s  %s\n", "", "journal", before)
	fmt.Fprintf(&b, "%-10s  %-12d  %d\n", "memories", r.Rebuilt.Memories, r.Current.Memories)
	fmt.Fprintf(&b, "%-10s  %-12d  %d\n", "shows", r.Rebuilt.Shown, r.Current.Shown)
	fmt.Fprintf(&b, "%-10s  %-12.2f  %.2f\n", "credits", r.Rebuilt.Credits, r.Current.Credits)
	fmt.Fprintf(&b, "%-10s  %-12.2f  %.2f\n", "misses", r.Rebuilt.Misses, r.Current.Misses)
	if r.CurrentError != "" {
		fmt.Fprintf(&b, "the ledger could not be read: %s\n", r.CurrentError)
	}
	fmt.Fprintf(&b, "%d new, %d dropped, %d changed\n", r.New, r.Dropped, r.Changed)
	if r.Plan {
		if r.NoImport {
			b.WriteString("backsight feedback rebuild --no-import writes it\n")
		} else {
			b.WriteString("backsight feedback rebuild writes it\n")
		}
	}
	return b.String()
}
