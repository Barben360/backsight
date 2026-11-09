package cli

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/livecheck"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// feedbackParams is the [feedback] table as the ledger's parameters. It
// lives here, not in config: feedback imports state, which imports config.
func feedbackParams(cfg config.Config) feedback.Params {
	const day = 24 * time.Hour
	return feedback.Params{
		ConfidentRank:  cfg.Feedback.ConfidentRank,
		MissHalfLife:   time.Duration(cfg.Feedback.MissHalfLifeDays) * day,
		CreditHalfLife: time.Duration(cfg.Feedback.CreditHalfLifeDays) * day,
		TurnTimeout:    feedback.TurnTimeout,
		Weight:         cfg.Feedback.BoostWeight,
		PriorCredits:   cfg.Feedback.PriorCredits,
		PriorMisses:    cfg.Feedback.PriorMisses,
	}
}

// ledgerAdjust is recall's Engine.Adjust from this machine's ledger, with
// what was journaled since its last fold (a hiding applies to the next
// recall) and the other machines' counts and hidings (sharedCounts): for
// the ids of one search's hits, read once through ledgerReader (one open,
// one read, the hook's wait), each memory's boost at now and whether it
// is hidden. Nil when feedback is off; it answers nil, so recall is as
// without a ledger, when no ledger can be read or both hold nothing.
func ledgerAdjust(d paths.Dirs, cfg config.Config, at time.Time, tw *telemetry.Writer) func(ids []string) map[string]recall.Adjustment {
	r := ledgerReader(d, cfg, true, tw)
	if r == nil {
		return nil
	}
	return func(ids []string) map[string]recall.Adjustment {
		entries, has, err := r.GetAny(ids)
		if err != nil {
			return nil
		}
		shared := sharedCounts(d, cfg, at)
		if !has && shared.Empty() {
			return nil
		}
		p := feedbackParams(cfg)
		out := make(map[string]recall.Adjustment, len(ids))
		for _, id := range ids {
			m := withShared(entries[id], shared, id, p, at)
			if m.Hidden != nil {
				out[id] = recall.Adjustment{Hidden: true}
			} else {
				out[id] = recall.Adjustment{Boost: m.Boost(p, at)}
			}
		}
		return out
	}
}

// How long a ledger reader waits for a writer's lock: a hook or the MCP
// server answers within its budget, a command can wait for a fold.
const (
	hookLedgerBusy    = 20 * time.Millisecond
	commandLedgerBusy = 5 * time.Second
)

// ledgerReader is how this machine's ledger is read, nil when feedback is
// off. Each call opens ledger.db read-only (waiting at most
// hookLedgerBusy in a hook or the MCP server, commandLedgerBusy
// elsewhere), reads it with the journal lines since its last fold, and
// closes it. A command meeting a ledger.db of a newer schema reads the
// side file of this binary's writers when there is one. When ledger.db
// cannot be read it logs one feedback_error naming why (ledger_missing,
// ledger_busy, ledger_unreadable, ledger_schema) and reads ledger.json
// instead; when that fails too, one more (ledger_json), and the error
// comes back: the caller acts as without a ledger.
func ledgerReader(d paths.Dirs, cfg config.Config, inHook bool, tw *telemetry.Writer) feedback.LedgerReader {
	if !cfg.Feedback.Enabled {
		return nil
	}
	c := ledgerChain{o: ledgerdb.Options{Dirs: d}, busy: commandLedgerBusy, side: !inHook, tw: tw,
		fallback: feedback.CurrentReader{Dir: d.FeedbackDir()}}
	if inHook {
		c.busy = hookLedgerBusy
	}
	return c
}

// commandLedger is the ledger a command shows: ledgerReader's for a
// command, its fallbacks logged to tw; with feedback off, the same chain
// read silently (nothing is journaled then, but what was counted is
// still shown).
func commandLedger(d paths.Dirs, cfg config.Config, tw *telemetry.Writer) feedback.LedgerReader {
	if r := ledgerReader(d, cfg, false, tw); r != nil {
		return r
	}
	cfg.Feedback.Enabled = true
	return ledgerReader(d, cfg, false, nil)
}

// indexLedger is the ledger the metrics index mirrors, and the one
// feedback rebuild compares with: ledgerReader's chain for a command,
// whatever feedback's switch, without its telemetry (the index records
// its read errors itself, the rebuild reports them), and with its Stamp
// and StampedAll, so that the index reads it again only when the file it
// read changed.
func indexLedger(d paths.Dirs) index.LedgerSource {
	return ledgerChain{o: ledgerdb.Options{Dirs: d}, busy: commandLedgerBusy, side: true,
		fallback: feedback.CurrentReader{Dir: d.FeedbackDir()}}
}

// ledgerChain reads ledger.db, else fallback (ledgerReader).
type ledgerChain struct {
	o        ledgerdb.Options
	busy     time.Duration
	side     bool              // a newer ledger.db: read the side file
	tw       *telemetry.Writer // nil: no telemetry
	fallback feedback.CurrentReader
}

func (c ledgerChain) Get(ids []string) (map[string]feedback.Entry, error) {
	return chainRead(c, func(r feedback.LedgerReader) (map[string]feedback.Entry, error) { return r.Get(ids) })
}

func (c ledgerChain) Hidden() (map[string]feedback.Entry, error) {
	return chainRead(c, feedback.LedgerReader.Hidden)
}

func (c ledgerChain) All() (feedback.Ledger, error) { return chainRead(c, feedback.LedgerReader.All) }

func (c ledgerChain) Any() (bool, error) { return chainRead(c, feedback.LedgerReader.Any) }

func (c ledgerChain) GetAny(ids []string) (map[string]feedback.Entry, bool, error) {
	type got struct {
		m   map[string]feedback.Entry
		has bool
	}
	g, err := chainRead(c, func(r feedback.LedgerReader) (got, error) {
		m, has, err := r.GetAny(ids)
		return got{m, has}, err
	})
	return g.m, g.has, err
}

// Stamp is the database reader's stamp, else the fallback's.
func (c ledgerChain) Stamp() (feedback.Stamp, error) {
	var st feedback.Stamp
	err := c.read(func(r *ledgerdb.Reader) (err error) { st, err = r.Stamp(); return err })
	if err != nil {
		return c.fallback.Stamp()
	}
	return st, nil
}

// StampedAll is All with the stamp of the file it read, taken just
// before it: the database's, or, when All fell back, ledger.json's
// (stamped false when that stamp fails). A stamp taken apart could name
// the database while All read ledger.json.
func (c ledgerChain) StampedAll() (l feedback.Ledger, st feedback.Stamp, stamped bool, err error) {
	err = c.read(func(r *ledgerdb.Reader) error {
		var err error
		if st, err = r.Stamp(); err != nil {
			return err
		}
		l, err = r.All()
		return err
	})
	if err == nil {
		return l, st, true, nil
	}
	c.emit(ledgerReason(err))
	st, serr := c.fallback.Stamp()
	if l, err = c.fallback.All(); err != nil {
		c.emit(telemetry.FeedbackReasonLedgerJSON)
		return l, feedback.Stamp{}, false, err
	}
	return l, st, serr == nil, nil
}

// read runs fn on the database reader, opened and closed around it, its
// file checked in fn's read transaction (one busy window): ledger.db, or
// for a command the side file beside a newer one.
func (c ledgerChain) read(fn func(*ledgerdb.Reader) error) error {
	err := ledgerdb.Read(c.o, c.busy, fn)
	if c.side && errors.Is(err, ledgerdb.ErrSchema) {
		if serr := ledgerdb.ReadSide(c.o, c.busy, fn); serr == nil {
			return nil
		}
	}
	return err
}

// chainRead is read through c's database reader; on its failure, through
// c's fallback, each failure logged.
func chainRead[T any](c ledgerChain, read func(feedback.LedgerReader) (T, error)) (T, error) {
	var v T
	err := c.read(func(r *ledgerdb.Reader) (err error) { v, err = read(r); return err })
	if err == nil {
		return v, nil
	}
	c.emit(ledgerReason(err))
	v, err = read(c.fallback)
	if err != nil {
		c.emit(telemetry.FeedbackReasonLedgerJSON)
	}
	return v, err
}

func (c ledgerChain) emit(reason string) {
	if c.tw != nil {
		c.tw.Emit(telemetry.FeedbackError(reason))
	}
}

// ledgerReason is the feedback_error reason of a database reader's error.
func ledgerReason(err error) string {
	switch {
	case errors.Is(err, ledgerdb.ErrMissing):
		return telemetry.FeedbackReasonLedgerMissing
	case errors.Is(err, ledgerdb.ErrBusy):
		return telemetry.FeedbackReasonLedgerBusy
	case errors.Is(err, ledgerdb.ErrSchema):
		return telemetry.FeedbackReasonLedgerSchema
	}
	return telemetry.FeedbackReasonLedgerUnreadable
}

// sharedCounts is the other machines' counts and hidings as jobs tick
// last combined them (shared-combined): one file read, each memory
// looked up when ranked, no document parsed, never the server. Empty, so
// recall uses the local counts alone, when share_ledger is off, or that
// file is missing, of another version or more than 30 days old.
func sharedCounts(d paths.Dirs, cfg config.Config, at time.Time) feedback.Combined {
	if !cfg.Feedback.ShareLedger {
		return feedback.Combined{}
	}
	c, err := feedback.LoadSharedCombined(d.FeedbackDir(), at)
	if err != nil {
		return feedback.Combined{}
	}
	return c
}

// withShared is m, id's local entry, with what shared holds of id added
// at now: a view, never saved.
func withShared(m feedback.Entry, shared feedback.Combined, id string, p feedback.Params, at time.Time) feedback.Entry {
	if s, ok := shared.Get(id); ok {
		return m.WithRemote(s, p, at)
	}
	return m
}

// ledgerOptions says where this machine's ledger database lives and how
// its writer opens it.
func ledgerOptions(d paths.Dirs, home string) ledgerdb.Options {
	return ledgerdb.Options{Dirs: d, Home: home, Getenv: os.Getenv, BinaryVersion: buildinfo.Version}
}

// settleFeedback is the tick's settle into the ledger database: local
// files only, so a server down never delays it. A failure is logged and
// never fails the tick: a failed settle or fold as ledger_fold (nothing
// counted), a failed export of ledger.json as ledger_export (the settle
// stands, the next tick exports again).
func settleFeedback(d paths.Dirs, home string, cfg config.Config, tw *telemetry.Writer) feedback.SettleResult {
	if !cfg.Feedback.Enabled {
		return feedback.SettleResult{}
	}
	res, err := ledgerdb.Settle(context.Background(), ledgerOptions(d, home), feedbackParams(cfg), now)
	switch {
	case errors.Is(err, ledgerdb.ErrExport):
		tw.Emit(telemetry.FeedbackError(telemetry.FeedbackReasonLedgerExport))
	case err != nil:
		tw.Emit(telemetry.FeedbackError(telemetry.FeedbackReasonLedgerFold))
		return feedback.SettleResult{}
	}
	if res != (feedback.SettleResult{}) {
		tw.Emit(telemetry.FeedbackSettle{Shows: res.Shows, Credits: res.Credits, Misses: res.Misses, Unknown: res.Unknown, Late: res.Late})
	}
	return res
}

// journalForget records memories the user retired, for the ledger to drop
// them. Best effort: a failed write never fails the forget.
func journalForget(ids []string) { journalUser(feedback.Event{Kind: "forget", IDs: ids}) }

// journalUnhide records memories the user restored, for the ledger to
// clear their verdicts and hiding. Best effort, like journalForget.
func journalUnhide(ids []string) {
	journalUser(feedback.Event{Kind: feedback.KindUnhide, IDs: ids, Outcome: "restored"})
}

// journalUser journals a forget or unhide of a user command, under the
// ledger lock, when feedback is on.
func journalUser(e feedback.Event) {
	cfg, _, err := loadServerConfig()
	if err != nil || !cfg.Feedback.Enabled {
		return
	}
	d, _, err := machine()
	if err != nil {
		return
	}
	_ = feedback.NewJournal(d.FeedbackDir(), feedback.Common{Binary: buildinfo.Version}).AppendFolded(e)
}

// hiddenHere: id is hidden on this machine, by the ledger with what was
// journaled since its last fold (local, ledgerReader), or else by
// another machine's hiding recall here follows (remote: sharedCounts).
// Both false when feedback is off or the ledger cannot be read.
func hiddenHere(id string) (local, remote bool) {
	cfg, _, err := loadServerConfig()
	if err != nil || !cfg.Feedback.Enabled {
		return false, false
	}
	d, _, err := machine()
	if err != nil {
		return false, false
	}
	tw := telemetry.New(d.State, telemetry.Common{BinaryVersion: buildinfo.Version})
	got, err := ledgerReader(d, cfg, false, tw).Get([]string{id})
	if err != nil {
		return false, false
	}
	if got[id].Hidden != nil {
		return true, false
	}
	at := now()
	return false, withShared(got[id], sharedCounts(d, cfg, at), id, feedbackParams(cfg), at).Hidden != nil
}

// A tick reads at most hiddenChecks histories, within hiddenCheckTimeout.
const (
	hiddenChecks       = 20
	hiddenCheckTimeout = 10 * time.Second
)

// checkHidden lifts the hidings the server made moot, the hidings those
// of ledger (the tick's one read) and the lifts journaled in dir: an observation with a change
// in its history after it was hidden is journaled unhide rewritten, one
// the server no longer holds (its history and the memory itself both not
// found) unhide gone. Any other answer leaves the hiding as it is. The
// hidings, longest hidden first, are read hiddenChecks at a time from a
// place that moves on by hiddenChecks every tick period of at (the
// tick's time, which stamps the events), so each is read within
// ceil(n/hiddenChecks) ticks. The loop stops once hiddenCheckTimeout has
// passed on the clock. Best effort: it returns how many it lifted.
func checkHidden(ctx context.Context, cl *hindsight.Client, bank string, ledger func() (feedback.Ledger, error), dir string, at time.Time) (lifted int) {
	l, err := ledger()
	if err != nil {
		return 0
	}
	type hiding struct {
		id    string
		since time.Time
	}
	var hidden []hiding
	for id, m := range l.Memories {
		if m.Hidden != nil {
			hidden = append(hidden, hiding{id, m.Hidden.Since})
		}
	}
	if len(hidden) == 0 {
		return 0
	}
	slices.SortFunc(hidden, func(a, b hiding) int {
		if c := a.since.Compare(b.since); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	n := len(hidden)
	first := int(at.Unix()/int64(livecheck.TickEvery/time.Second)%int64(n)) * hiddenChecks % n
	deadline := now().Add(hiddenCheckTimeout)
	ctx, cancel := context.WithTimeout(ctx, hiddenCheckTimeout)
	defer cancel()
	j := feedback.NewJournal(dir, feedback.Common{Binary: buildinfo.Version})
	for k := range min(n, hiddenChecks) {
		if !now().Before(deadline) || ctx.Err() != nil {
			break
		}
		h := hidden[(first+k)%n]
		outcome := ""
		changes, err := cl.History(ctx, bank, h.id)
		switch {
		case notFound(err):
			if _, err := cl.GetMemory(ctx, bank, h.id); !notFound(err) {
				continue // the memory answers, or the server fails: not known gone
			}
			outcome = "gone"
		case err != nil:
			continue
		case slices.ContainsFunc(changes, func(c hindsight.HistoryEntry) bool { return c.ChangedAt.After(h.since) }):
			outcome = "rewritten"
		default:
			continue
		}
		// Under the ledger lock, which the tick's settle has released; a
		// lock held too long still journals it.
		var held *state.HeldError
		if err := j.AppendFolded(feedback.Event{Kind: feedback.KindUnhide, TS: at.UTC(), IDs: []string{h.id}, Outcome: outcome}); err == nil || errors.As(err, &held) {
			lifted++
		}
	}
	return lifted
}

// notFound: err is the server saying the memory does not exist.
func notFound(err error) bool {
	var apiErr *hindsight.APIError
	return errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound
}
