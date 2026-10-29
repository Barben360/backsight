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
}

func newFeedbackImportCmd(e *env) *cobra.Command {
	var since string
	var plan bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Count the memories shown and the unambiguous credits of earlier history (telemetry, retained transcripts), once",
		Long: "Import reads this machine's telemetry and the Claude Code transcripts still on disk, and adds to each memory's counts the " +
			"times it was shown there, and a credit for a turn whose reply cited memory and that showed exactly one. It never counts a " +
			"miss. Imported counts stay apart (feedback rebuild --no-import leaves them out), a show already imported or journaled live " +
			"is not counted again, and ids the server no longer knows are dropped (checked read-only). --plan counts per source and " +
			"writes nothing.",
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
			res, err := importHistory(cmd.Context(), d, home, from, plan)
			return e.report("feedback import", res, func() string { return importText(res) }, err)
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only shows from this date on (YYYY-MM-DD, UTC)")
	cmd.Flags().BoolVar(&plan, "plan", false, "count per source, writing nothing")
	return cmd
}

// importHistory finds the shows of earlier history from from on (zero:
// all), keeps those neither imported already nor journaled live, of
// memories not forgotten and still known to the server, and journals
// them (unless plan) as one import event per source and session.
func importHistory(ctx context.Context, d paths.Dirs, home string, from time.Time, plan bool) (importResult, error) {
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
	dir := d.FeedbackDir()
	journal, _, err := feedback.Read(dir, time.Time{})
	if err != nil {
		return res, blocked(err)
	}
	known := journalKnown(journal)

	tel, err := telemetryEvents(d.TelemetryDir(), from)
	if err != nil {
		return res, blocked(err)
	}
	telSessions := map[string]bool{}
	for _, e := range tel {
		telSessions[e.Session] = true
	}
	found := map[string][]feedback.ImportTurn{sourceTelemetry: feedback.TelemetryTurns(tel)}
	// A session telemetry knows is imported from it alone: its transcript
	// holds the same searches.
	for _, f := range transcripts(home) {
		turns, err := transcriptTurns(f)
		if err != nil {
			continue // a transcript that cannot be read adds nothing
		}
		for _, t := range turns {
			if len(t.Shows) > 0 && !telSessions[t.Shows[0].Session] {
				found[sourceTranscripts] = append(found[sourceTranscripts], t)
			}
		}
	}

	candidates := map[string][]feedback.ImportShow{}
	ids := map[string]bool{}
	taken := journalIndex{keys: map[string]bool{}, records: map[string]bool{}} // a resumed session's transcript repeats its records
	for _, source := range []string{sourceTelemetry, sourceTranscripts} {
		for _, t := range found[source] {
			for _, s := range t.Credited() {
				if !from.IsZero() && s.At.Before(from) || known.live(s) || known.forgotten[s.ID] || taken.imported(s) {
					continue
				}
				if known.imported(s) {
					res.Already++
					continue
				}
				taken.take(s)
				candidates[source] = append(candidates[source], s)
				ids[s.ID] = true
			}
		}
	}

	var gone map[string]bool
	if configured {
		gone, res.Unchecked, res.Why = checkMemories(ctx, cfg, sec, slices.Sorted(maps.Keys(ids)))
	} else {
		res.Unchecked, res.Why = len(ids), "no server configured on this machine"
	}

	kept := map[string]map[string][]feedback.ImportShow{} // source → session → shows
	for source, shows := range candidates {
		src, dropped, sessions := res.Sources[source], map[string]bool{}, map[string][]feedback.ImportShow{}
		for _, s := range shows {
			if gone[s.ID] {
				dropped[s.ID] = true
				continue
			}
			sessions[s.Session] = append(sessions[s.Session], s)
			src.Shows++
			if s.Credit {
				src.Credits++
			}
		}
		src.Sessions, src.Dropped, kept[source] = len(sessions), len(dropped), sessions
	}
	if plan {
		return res, nil
	}

	p := feedbackParams(cfg)
	j := feedback.NewJournal(dir, feedback.Common{Binary: buildinfo.Version})
	for _, source := range []string{sourceTelemetry, sourceTranscripts} {
		for _, session := range slices.Sorted(maps.Keys(kept[source])) {
			// Under the ledger lock; one held too long still journals it.
			var held *state.HeldError
			if err := j.AppendFolded(feedback.ImportEvent(source, session, kept[source][session], p)); err != nil && !errors.As(err, &held) {
				return res, blocked(err)
			}
			res.Events++
		}
	}
	return res, nil
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
func transcriptTurns(path string) ([]feedback.ImportTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if !containsWord(f, []byte("memory_search")) {
		return nil, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	return feedback.TranscriptTurns(f, func(reply string) bool {
		k := replyKind(reply)
		return k != nil && (*k == "exact" || *k == "free")
	})
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

// checkMemories asks the server for each id, importChecks at a time
// within importCheckBudget, reading only: gone holds the ids it answers
// not found for. An id it fails on, or not asked about in time, is kept
// and counted unchecked, with the first reason.
func checkMemories(ctx context.Context, cfg config.Config, sec config.Secrets, ids []string) (gone map[string]bool, unchecked int, why string) {
	gone = map[string]bool{}
	if len(ids) == 0 {
		return gone, 0, ""
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, importCheckTimeout)
	if err != nil {
		return gone, len(ids), err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, importCheckBudget)
	defer cancel()
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
					_, err = cl.GetMemory(ctx, cfg.Server.Bank, id)
				}
				mu.Lock()
				switch {
				case err == nil:
				case notFound(err):
					gone[id] = true
				default:
					unchecked++
					if why == "" {
						why = err.Error()
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
	return gone, unchecked, why
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
	if !r.Plan {
		fmt.Fprintf(&b, "imported %d shows and %d credits (never a miss)\n", shows, credits)
	}
	return b.String()
}
