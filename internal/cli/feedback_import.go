package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/stats"
)

// Sources of earlier history, as import events and the result name them.
const (
	sourceTelemetry   = "telemetry"
	sourceTranscripts = "transcripts"
)

// The ids to import are checked on the server importChecks at a time,
// for at most importCheckBudget in all; importCheckTimeout bounds one
// request.
const (
	importChecks       = 4
	importCheckBudget  = 30 * time.Second
	importCheckTimeout = 10 * time.Second
)

// importSource is what one source adds: the sessions with a show to
// import, those shows and their credits, and the memory ids the server no
// longer knows (their shows not counted).
type importSource struct {
	Sessions int `json:"sessions"`
	Shows    int `json:"shows"`
	Credits  int `json:"credits"`
	Dropped  int `json:"ids_dropped"`
}

type importResult struct {
	Plan    bool                     `json:"plan"`
	Since   string                   `json:"since,omitempty"`
	Sources map[string]*importSource `json:"sources"`
	// Already: shows an earlier import journaled (skipped).
	Already int `json:"already_imported"`
	// Unchecked: memory ids the server could not be asked about (kept).
	Unchecked int    `json:"ids_unchecked"`
	Why       string `json:"unchecked_reason,omitempty"`
	Events    int    `json:"events"` // import events journaled
	// Complete: the run went through every source and journaled the mark
	// that ends the import (never with --plan).
	Complete bool `json:"complete"`
}

// totals is the shows and credits of every source.
func (r importResult) totals() (shows, credits int) {
	for _, s := range r.Sources {
		shows, credits = shows+s.Shows, credits+s.Credits
	}
	return shows, credits
}

func newFeedbackImportCmd(e *env) *cobra.Command {
	var since string
	var plan bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Count the memories shown and the unambiguous credits of earlier history (telemetry, retained transcripts), once",
		Long: "Import reads this machine's telemetry and the Claude Code transcripts still on disk, and adds to each memory's counts the " +
			"times it was shown there, and, from telemetry only, a credit for a turn whose reply cited memory and that showed exactly " +
			"one. It never counts a miss. Imported counts stay apart (feedback rebuild --no-import leaves them out), a show already " +
			"imported or journaled live is not counted again, and ids the server no longer knows are dropped (checked read-only). The " +
			"jobs tick runs it until one run goes through every source; one import runs at a time. --plan counts per source and writes nothing.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var from time.Time
			if since != "" {
				t, err := time.Parse("2006-01-02", since)
				if err != nil {
					return &output.Error{Code: output.Usage, Message: "--since: want a date (YYYY-MM-DD), got " + since}
				}
				from = t
			}
			d, home, err := machine()
			if err != nil {
				return err
			}
			res, err := importHistory(cmd.Context(), d, home, from, plan, "feedback import")
			if running := (importRunning{}); errors.As(err, &running) {
				err = &output.Error{Code: output.Blocked, Message: running.Error()}
			}
			return e.report("feedback import", res, func() string { return importText(res) }, err)
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only shows from this date on (YYYY-MM-DD, UTC)")
	cmd.Flags().BoolVar(&plan, "plan", false, "count per source, writing nothing")
	return cmd
}

// afterImportUnit, when set (tests), runs after each unit of an import
// (telemetry, then each transcript) is journaled.
var afterImportUnit func()

// importHistory finds the shows of earlier history from from on (zero:
// all), keeps those neither imported already nor journaled live, of
// memories not forgotten and still known to the server, and journals
// them (unless plan) as import events by source and session: telemetry
// first, then each transcript, so that a run ctx cuts short has kept
// what it did and the next one goes on (keys make it idempotent). A run
// that goes through every source journals the mark of a complete import.
// Refused while feedback is off. One run at a time, as command, under
// the import lock: beside a run that holds it, importRunning and nothing
// done (a plan, which writes nothing, takes no lock).
func importHistory(ctx context.Context, d paths.Dirs, home string, from time.Time, plan bool, command string) (importResult, error) {
	res := importResult{Plan: plan, Sources: map[string]*importSource{sourceTelemetry: {}, sourceTranscripts: {}}}
	if !from.IsZero() {
		res.Since = from.Format("2006-01-02")
	}
	cfg, sec, err := loadServerConfig()
	configured := err == nil
	var oe *output.Error
	switch {
	case errors.As(err, &oe) && oe.Code == output.NeedsUser: // no config.toml
		cfg = config.Default(d, home)
	case err != nil:
		return res, err
	}
	if !cfg.Feedback.Enabled {
		return res, &output.Error{Code: output.NeedsUser, Message: "feedback is off on this machine (feedback.enabled = false in config.toml): nothing records which memories are used, so there is nothing to import into"}
	}
	if !plan {
		// Taken before the journal is read: what a run before it
		// journaled counts as imported already.
		release, err := lockImport(d, command)
		if err != nil {
			return res, err
		}
		defer release()
	}
	dir := d.FeedbackDir()
	journal, _, err := feedback.Read(dir, time.Time{})
	if err != nil {
		return res, blocked(err)
	}
	tel, err := telemetryEvents(d.TelemetryDir(), from)
	if err != nil {
		return res, blocked(err)
	}
	ck := &memoryCheck{status: map[string]string{}}
	switch mm := otherServer(d, cfg); {
	case !configured:
		ck.why = "no server configured on this machine"
	case mm != nil:
		ck.why = mm.Error() // its answers would say every memory is gone
	default:
		ck.cl, err = hindsight.New(cfg.Server.URL, sec.APIKey, importCheckTimeout)
		if err != nil {
			ck.why = err.Error()
		}
		ck.bank = cfg.Server.Bank
	}
	checkCtx, cancel := context.WithTimeout(ctx, importCheckBudget)
	defer cancel()
	im := &historyImport{ctx: ctx, checkCtx: checkCtx, res: &res, from: from, plan: plan, known: journalKnown(journal), check: ck,
		taken:    journalIndex{keys: map[string]bool{}, records: map[string]bool{}},
		sessions: map[string]map[string]bool{}, dropped: map[string]map[string]bool{},
		journal: feedback.NewJournal(dir, feedback.Common{Binary: buildinfo.Version}), params: feedbackParams(cfg)}

	telSessions := map[string]bool{}
	for _, e := range tel {
		telSessions[e.Session] = true
	}
	if !im.unit(sourceTelemetry, feedback.TelemetryTurns(tel)) {
		return res, im.err
	}
	// A session telemetry knows is imported from it alone: its transcript
	// holds the same searches, and cannot tell its credits.
	for _, f := range transcripts(home) {
		turns, err := transcriptTurns(ctx, f)
		if ctx.Err() != nil {
			return res, nil
		}
		if err != nil {
			continue // a transcript that cannot be read adds nothing
		}
		turns = slices.DeleteFunc(turns, func(t feedback.ImportTurn) bool { return len(t.Shows) == 0 || telSessions[t.Shows[0].Session] })
		if !im.unit(sourceTranscripts, turns) {
			return res, im.err
		}
	}
	if plan {
		return res, nil
	}
	var held *state.HeldError
	if err := im.journal.AppendFolded(feedback.Event{Kind: feedback.KindImport, Outcome: feedback.ImportComplete}); err != nil && !errors.As(err, &held) {
		return res, blocked(err)
	}
	res.Complete = true
	return res, nil
}

// importRunning: another import holds the import lock (by: its command).
type importRunning struct{ by string }

func (e importRunning) Error() string {
	return "an import of earlier history is already running (" + e.by + "): try again when it is over"
}

// lockImport takes <state>/locks/feedback-import.lock for command,
// without waiting, breaking a lock whose owner is gone; importRunning
// while a live owner holds it.
func lockImport(d paths.Dirs, command string) (release func(), err error) {
	l, _, err := state.Acquire(filepath.Join(d.LocksDir(), "feedback-import.lock"), command)
	var held *state.HeldError
	if errors.As(err, &held) {
		return nil, importRunning{by: held.Owner.Command}
	}
	if err != nil {
		return nil, blocked(err)
	}
	return func() { _ = l.Release() }, nil
}

// historyImport is one run of importHistory, unit after unit.
type historyImport struct {
	ctx, checkCtx context.Context
	res           *importResult
	from          time.Time
	plan          bool
	known, taken  journalIndex // the journal's, and this run's (a resumed session's transcript repeats its records)
	check         *memoryCheck
	// sessions and dropped: by source, the sessions with a show kept and
	// the ids the server no longer knows.
	sessions, dropped map[string]map[string]bool
	journal           *feedback.Journal
	params            feedback.Params
	err               error
}

// unit imports the shows of one unit's turns; false when the run stops
// there: ctx done (nothing of the unit journaled), or a journal error.
func (im *historyImport) unit(source string, turns []feedback.ImportTurn) bool {
	var shows []feedback.ImportShow
	for _, t := range turns {
		for _, s := range t.Credited() {
			if !im.from.IsZero() && s.At.Before(im.from) || im.known.live(s) || im.known.forgotten[s.ID] || im.taken.imported(s) {
				continue
			}
			if im.known.imported(s) {
				im.res.Already++
				continue
			}
			im.taken.take(s)
			shows = append(shows, s)
		}
	}
	ids := map[string]bool{}
	for _, s := range shows {
		ids[s.ID] = true
	}
	im.check.run(im.checkCtx, slices.Sorted(maps.Keys(ids)))
	if im.ctx.Err() != nil {
		return false
	}
	src := im.res.Sources[source]
	if im.sessions[source] == nil {
		im.sessions[source], im.dropped[source] = map[string]bool{}, map[string]bool{}
	}
	bySession := map[string][]feedback.ImportShow{}
	for _, s := range shows {
		if im.check.status[s.ID] == checkGone {
			im.dropped[source][s.ID] = true
			continue
		}
		bySession[s.Session] = append(bySession[s.Session], s)
		im.sessions[source][s.Session] = true
		src.Shows++
		if s.Credit {
			src.Credits++
		}
	}
	src.Sessions, src.Dropped = len(im.sessions[source]), len(im.dropped[source])
	im.res.Unchecked, im.res.Why = im.check.unchecked, im.check.why
	if im.plan {
		return true
	}
	for _, session := range slices.Sorted(maps.Keys(bySession)) {
		// Under the ledger lock; one held too long still journals it.
		var held *state.HeldError
		if err := im.journal.AppendFolded(feedback.ImportEvent(source, session, bySession[session], im.params)); err != nil && !errors.As(err, &held) {
			im.err = blocked(err)
			return false
		}
		im.res.Events++
	}
	if afterImportUnit != nil {
		afterImportUnit()
	}
	return true
}

// journalIndex is what the journal says of the shows to import: the
// import keys already journaled (a transcript record's also without its
// session, since a resumed session repeats the records it resumes),
// when each session's live journaling began, and the memories forgotten
// and not restored since.
type journalIndex struct {
	keys, records map[string]bool
	liveFrom      map[string]time.Time
	forgotten     map[string]bool
}

func journalKnown(events []feedback.Event) journalIndex {
	k := journalIndex{keys: map[string]bool{}, records: map[string]bool{}, liveFrom: map[string]time.Time{}, forgotten: map[string]bool{}}
	sorted := slices.Clone(events)
	slices.SortStableFunc(sorted, func(a, b feedback.Event) int { return a.TS.Compare(b.TS) })
	for _, e := range sorted {
		switch e.Kind {
		case feedback.KindImport:
			for _, s := range e.Settled {
				k.keys[s.Key] = true
				if _, record, ok := strings.Cut(s.Key, "|"); ok && !numeric(strings.SplitN(record, "|", 2)[0]) {
					k.records[record] = true
				}
			}
			continue
		case "forget":
			for _, id := range e.IDs {
				k.forgotten[id] = true
			}
		case feedback.KindUnhide:
			for _, id := range e.IDs {
				delete(k.forgotten, id)
			}
		}
		if _, ok := k.liveFrom[e.Session]; e.Session != "" && !ok {
			k.liveFrom[e.Session] = e.TS
		}
	}
	return k
}

// recordKey is a transcript show's key without its session; "" for a
// telemetry show, whose prompt number names it only in its session.
func recordKey(s feedback.ImportShow) string {
	if numeric(s.Ref) {
		return ""
	}
	return s.Ref + "|" + s.Path + "|" + s.ID
}

func (k journalIndex) imported(s feedback.ImportShow) bool {
	r := recordKey(s)
	return k.keys[s.Key()] || r != "" && k.records[r]
}

func (k journalIndex) take(s feedback.ImportShow) {
	k.keys[s.Key()] = true
	if r := recordKey(s); r != "" {
		k.records[r] = true
	}
}

// live: s is journaled already, its session's journal having begun at or
// before it.
func (k journalIndex) live(s feedback.ImportShow) bool {
	t, ok := k.liveFrom[s.Session]
	return ok && !s.At.Before(t)
}

func numeric(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// telemetryEvents is dir's telemetry from from on (zero: from its first
// month) as the import reads it.
func telemetryEvents(dir string, from time.Time) ([]feedback.TelemetryEvent, error) {
	if from.IsZero() {
		files, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
		if len(files) == 0 {
			return nil, nil
		}
		slices.Sort(files)
		first, err := time.Parse("2006-01", strings.TrimSuffix(strings.TrimPrefix(filepath.Base(files[0]), "events-"), ".jsonl"))
		if err != nil {
			return nil, nil
		}
		from = first
	}
	events, err := stats.Load(dir, from, now().Add(time.Minute))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	out := make([]feedback.TelemetryEvent, 0, len(events))
	for _, e := range events {
		switch e.Event {
		case "prompt", "tool_use", "reply", "outcome":
			out = append(out, feedback.TelemetryEvent{TS: e.TS, Event: e.Event, Session: e.Session, Name: e.Name, N: e.N, Reason: e.Reason,
				IDs: e.IDs, MemoryIDs: e.MemoryIDs, Credit: e.Credit, Credited: e.Credited})
		}
	}
	return out, nil
}

// transcriptTurns is the turns of the transcript at path that searched
// memory; nothing, without decoding it, when it never called memory_search.
// Reading stops once ctx is done.
func transcriptTurns(ctx context.Context, path string) ([]feedback.ImportTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if !containsWord(ctxReader{ctx, f}, []byte("memory_search")) {
		return nil, ctx.Err()
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	return feedback.TranscriptTurns(ctxReader{ctx, f})
}

// containsWord: r holds word, read in chunks.
func containsWord(r io.Reader, word []byte) bool {
	buf := make([]byte, 1<<16+len(word))
	keep := 0
	for {
		n, err := r.Read(buf[keep:])
		end := keep + n
		if bytes.Contains(buf[:end], word) {
			return true
		}
		keep = min(end, len(word)-1)
		copy(buf, buf[end-keep:end])
		if err != nil {
			return false
		}
	}
}

// What the server said of an id: it holds it, or no longer.
const (
	checkKnown = "known"
	checkGone  = "gone"
)

// memoryCheck asks the server for ids, reading only, importChecks at a
// time: status holds what it answered, each id asked once. An id it fails
// on, or not asked about before ctx is done, is kept: counted unchecked,
// with the first reason. Without a client, every id is unchecked for why.
type memoryCheck struct {
	cl        *hindsight.Client
	bank, why string
	status    map[string]string
	unchecked int
}

func (c *memoryCheck) run(ctx context.Context, ids []string) {
	ids = slices.DeleteFunc(ids, func(id string) bool { _, asked := c.status[id]; return asked })
	if c.cl == nil {
		for _, id := range ids {
			c.status[id], c.unchecked = "unchecked", c.unchecked+1
		}
		return
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan string)
	for range importChecks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range next {
				var err error
				if err = ctx.Err(); err == nil {
					_, err = c.cl.GetMemory(ctx, c.bank, id)
				}
				mu.Lock()
				switch {
				case err == nil:
					c.status[id] = checkKnown
				case notFound(err):
					c.status[id] = checkGone
				default:
					c.status[id], c.unchecked = "unchecked", c.unchecked+1
					if c.why == "" {
						c.why = err.Error()
					}
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		next <- id
	}
	close(next)
	wg.Wait()
}

func importText(r importResult) string {
	var b strings.Builder
	if r.Plan {
		b.WriteString("plan: nothing written\n")
	}
	shows, credits := 0, 0
	for _, source := range []string{sourceTelemetry, sourceTranscripts} {
		s := r.Sources[source]
		shows, credits = shows+s.Shows, credits+s.Credits
		fmt.Fprintf(&b, "%-12s %d sessions, %d shows, %d credits", source, s.Sessions, s.Shows, s.Credits)
		if s.Dropped > 0 {
			fmt.Fprintf(&b, ", %d memory ids dropped (no longer on the server)", s.Dropped)
		}
		b.WriteString("\n")
	}
	if r.Already > 0 {
		fmt.Fprintf(&b, "%d shows imported before: skipped\n", r.Already)
	}
	if r.Unchecked > 0 {
		fmt.Fprintf(&b, "%d memory ids not checked (%s): kept\n", r.Unchecked, r.Why)
	}
	switch {
	case r.Plan:
	case r.Complete:
		fmt.Fprintf(&b, "imported %d shows and %d credits (never a miss)\n", shows, credits)
	default:
		fmt.Fprintf(&b, "imported %d shows and %d credits so far (never a miss); stopped before the end: run it again\n", shows, credits)
	}
	return b.String()
}
