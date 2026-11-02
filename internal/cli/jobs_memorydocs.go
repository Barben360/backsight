package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/metricsfile"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/session"
)

// The memory-docs step's bounds: list calls (the two filter checks, the
// probes and the pages) and document reads a run, the workers that make
// them, and what a day list asks for.
const (
	memoryDocsListCalls = 200
	memoryDocsDocReads  = 200
	memoryDocsWorkers   = 4
	memoryDocsPage      = 500   // items asked per page; the server may give fewer
	memoryDocsHugeDay   = 50000 // above it, a day is read last, by the hour
	memoryDocsOpsPages  = 5     // operation pages read per status
	memoryDocsBatch     = 4     // older days read at once by the backfill
	// memoryDocsSentReach: the sends whose operations are recorded.
	memoryDocsSentReach = 7 * 24 * time.Hour
	// memoryDocsDayLag: a UTC day is listed once it ended an hour ago
	// (the server's clock may be behind, a write may commit just after
	// midnight).
	memoryDocsDayLag = 25 * time.Hour
)

// memoryDocsStepTimeout bounds the whole step (a var for the tests).
var memoryDocsStepTimeout = 60 * time.Second

// errMDSpent: the run's list calls are spent; the rest waits for the next run.
var errMDSpent = errors.New("the run's list calls are spent")

// memoryDocsReader is what the step reads of the server (hindsight.ReadOnly).
type memoryDocsReader interface {
	ListCreatedMemories(context.Context, time.Time, time.Time, int, int) ([]hindsight.ListedMemory, int, error)
	RetainOperations(context.Context, string, int) ([]hindsight.Operation, error)
	DocumentUnits(context.Context, string) (int, bool, error)
}

// memoryDocsIndex is what the step reads of the metrics index.
type memoryDocsIndex interface {
	CatchUp(context.Context, time.Duration) (index.Freshness, error)
	ListedDays(context.Context) ([]string, bool, error)
	SentDocuments(context.Context, time.Time) ([]index.SentDocument, error)
	RecordedOperations(context.Context) (map[string]index.RecordedOp, error)
}

// memoryDocsReaderFor is readOnlyServer as the step's reader (a nil
// client is a nil reader). Tests replace it.
var memoryDocsReaderFor = func() (memoryDocsReader, error) {
	ro, err := readOnlyServer()
	if ro == nil {
		return nil, err
	}
	return ro, nil
}

// maintainMemoryDocs is jobs maintain's memory-docs step: its result line,
// never an error.
func maintainMemoryDocs(ctx context.Context, d paths.Dirs, home string, clock func() time.Time) string {
	r, err := memoryDocsReaderFor()
	switch {
	case err != nil:
		return "memory docs: not reached (not_configured)"
	case r == nil:
		return "memory docs: no server set up"
	}
	// As the index's pruning: maintain never creates the index (ticks
	// build it); the step waits for it.
	if _, err := os.Lstat(d.IndexDir()); errors.Is(err, fs.ErrNotExist) {
		return "memory docs: not run, no metrics index yet"
	}
	octx, cancel := context.WithTimeout(ctx, indexStepTimeout)
	defer cancel()
	o := indexOptions(d, home, nil)
	o.Now = clock
	ix, err := index.Open(octx, o)
	if err != nil {
		return "memory docs: not run, index " + indexStepError(err)
	}
	defer func() { _ = ix.Close() }()
	w := metricsfile.New(d.MetricsDir(), buildinfo.Version)
	w.Now = clock
	return collectMemoryDocs(ctx, w, ix, r, clock())
}

// collectMemoryDocs records each memory's source document and creation day
// from the server's day lists, and the retain operations of this PC's
// recent sends. In order: the index caught up (the days already listed),
// the two filter checks, the days after the oldest listed one, the
// operations and their documents' unit counts, the older days (the
// backfill, until a probe finds nothing older), and last any day too large
// for one list, read by the hour. A day is written only when all its
// pages were read in this run; at a bound or the deadline what is whole is
// written and the rest waits for the next run. The result is a line for
// the job's report, never an error.
func collectMemoryDocs(ctx context.Context, w *metricsfile.Writer, ix memoryDocsIndex, r memoryDocsReader, now time.Time) string {
	if r == nil {
		return "memory docs: no server set up"
	}
	ctx, cancel := context.WithTimeout(ctx, memoryDocsStepTimeout)
	defer cancel()
	// Best effort: lines the index has not read yet are read again, which
	// the later line wins.
	_, _ = ix.CatchUp(ctx, index.TickBudget)
	listed, complete, err := ix.ListedDays(ctx)
	if err != nil {
		return "memory docs: not run, index " + indexStepError(err)
	}
	m := &mdRun{w: w, r: r, now: now, listed: map[string]bool{}, complete: complete}
	m.lists.Store(memoryDocsListCalls)
	m.docs.Store(memoryDocsDocReads)
	for _, d := range listed {
		m.listed[d] = true
	}
	days := m.filterChecks(ctx)
	if days == "" {
		m.forward(ctx, listed)
	}
	ops := m.operations(ctx, ix)
	if days == "" {
		m.backfill(ctx)
		m.hugeDays(ctx)
		days = m.daysPart()
	}
	return "memory docs: " + days + ", " + ops
}

// mdRun is one run of the step. The counters and the record of what was
// listed are only touched by the step's own goroutine, but for the hour
// windows (under mu).
type mdRun struct {
	w   *metricsfile.Writer
	wmu sync.Mutex // one Append at a time
	r   memoryDocsReader
	now time.Time

	lists, docs atomic.Int64 // calls left

	mu        sync.Mutex
	listed    map[string]bool // days with a listed line, earlier or now
	complete  bool            // the record covers the bank
	open      bool            // the backfill stopped before the bank's first day
	left      int             // days left unlisted for the next run
	huge      []mdHuge        // days put off to the end of the run
	tooLarge  []string        // the result's parts of the huge days not finished
	nDays     int
	nMemories int
	writeErr  string
}

// mdHuge is a day read last: page 0's total.
type mdHuge struct {
	day   time.Time
	total int
}

// mdSpan is what was read of one time span.
type mdSpan struct {
	items []hindsight.ListedMemory // distinct ids, in the order read
	total int                      // page 0's
	whole bool                     // every page read, the distinct ids equal the total
	huge  bool                     // a day put off (page 0 only read)
}

// mdPool runs the jobs in order, at most memoryDocsWorkers at once.
func mdPool(jobs []func()) {
	sem := make(chan struct{}, memoryDocsWorkers)
	var wg sync.WaitGroup
	for _, j := range jobs {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			j()
		})
	}
	wg.Wait()
}

// mdTurn orders the reservations of spans read at once: a span decides
// after the one dispatched before it, so the earlier (the older day going
// forward, the newer going back) always goes first.
type mdTurn struct {
	prev, mine chan struct{}
	once       sync.Once
}

// mdTurns are the turns of n spans, in their order.
func mdTurns(n int) []*mdTurn {
	out := make([]*mdTurn, n)
	var prev chan struct{}
	for i := range out {
		out[i] = &mdTurn{prev: prev, mine: make(chan struct{})}
		prev = out[i].mine
	}
	return out
}

// take runs decide (nil: nothing to decide) once the turn before has, then
// lets the next one go; only its first call counts. A nil turn decides at
// once.
func (t *mdTurn) take(decide func()) {
	if t == nil {
		if decide != nil {
			decide()
		}
		return
	}
	t.once.Do(func() {
		if t.prev != nil {
			<-t.prev
		}
		if decide != nil {
			decide()
		}
		close(t.mine)
	})
}

// reserve takes n list calls from what is left: all of them, or none.
func (m *mdRun) reserve(n int64) bool {
	for {
		left := m.lists.Load()
		if left < n {
			return false
		}
		if m.lists.CompareAndSwap(left, left-n) {
			return true
		}
	}
}

// list is one list call within the run's budget and the call's bound.
func (m *mdRun) list(ctx context.Context, start, end time.Time, offset, limit int) ([]hindsight.ListedMemory, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if !m.reserve(1) {
		return nil, 0, errMDSpent
	}
	return m.call(ctx, start, end, offset, limit)
}

// call is one list call already paid for, within the call's bound.
func (m *mdRun) call(ctx context.Context, start, end time.Time, offset, limit int) ([]hindsight.ListedMemory, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	type page struct {
		items []hindsight.ListedMemory
		total int
	}
	p, err := boundedWithin(ctx, serverDailyTimeout, func(c context.Context) (page, error) {
		items, total, err := m.r.ListCreatedMemories(c, start, end, offset, limit)
		return page{items, total}, err
	})
	return p.items, p.total, err
}

// pagesLeft is how many pages after page 0 a span of total needs at
// page 0's size n.
func pagesLeft(total, n int) int64 {
	if n == 0 || total <= n {
		return 0
	}
	return int64((total+n-1)/n - 1)
}

// readSpan reads the memories created in [start, end), page after page,
// each from the offset the items so far reached. With hugeCheck, a span
// whose page 0 says it is over memoryDocsHugeDay, or needs more pages
// than the run's whole budget, is not read further. After page 0 the
// span reserves the pages it still needs, in its turn: when they are not
// left, it is not read further (the next run reads it), so spans read at
// once are never all cut part way. A reservation not spent goes back.
func (m *mdRun) readSpan(ctx context.Context, start, end time.Time, hugeCheck bool, turn *mdTurn) mdSpan {
	defer turn.take(nil) // whatever happens, the next span's turn comes
	var s mdSpan
	var reserved int64
	defer func() { m.lists.Add(reserved) }()
	seen := map[string]bool{}
	for offset := 0; ; {
		var items []hindsight.ListedMemory
		var total int
		var err error
		if reserved > 0 {
			reserved--
			items, total, err = m.call(ctx, start, end, offset, memoryDocsPage)
		} else {
			items, total, err = m.list(ctx, start, end, offset, memoryDocsPage)
		}
		if err != nil {
			return mdSpan{}
		}
		if offset == 0 {
			s.total = total
			if hugeCheck && (total > memoryDocsHugeDay || len(items) > 0 && (total+len(items)-1)/len(items) > memoryDocsListCalls) {
				return mdSpan{total: total, huge: true}
			}
			if need := pagesLeft(total, len(items)); need > 0 {
				turn.take(func() {
					if m.reserve(need) {
						reserved = need
					}
				})
				if reserved == 0 {
					return mdSpan{}
				}
			}
			turn.take(nil)
		}
		if len(items) == 0 { // before the total: the span is not read whole
			break
		}
		for _, it := range items {
			if !seen[it.ID] {
				seen[it.ID] = true
				s.items = append(s.items, it)
			}
		}
		offset += len(items)
		if offset >= s.total {
			break
		}
	}
	s.whole = len(s.items) == s.total
	return s
}

// mdFactType is a fact type in the record's closed set.
func mdFactType(t string) string {
	switch t {
	case "world", "experience", "observation":
		return t
	}
	return "other"
}

// mdOpStatus is an operation status in the record's closed set.
func mdOpStatus(s string) string {
	switch s {
	case "pending", "processing", "completed", "failed":
		return s
	}
	return "other"
}

// memoryLines are the memory lines of items listed on day: the document
// and its session only for a session's document, any other its kind.
func memoryLines(items []hindsight.ListedMemory, day string) []metricsfile.Record {
	out := make([]metricsfile.Record, 0, len(items)+1)
	for _, it := range items {
		l := metricsfile.MemoryDoc{Memory: it.ID, Outcome: "found", DocKind: session.KindOfDocument(it.DocumentID), CreatedDay: day, FactType: mdFactType(it.FactType)}
		if sid, ok := session.OfDocument(it.DocumentID); ok {
			l.Document, l.Session = it.DocumentID, sid
		}
		out = append(out, l)
	}
	return out
}

// append writes recs in one Append, keeping the first failure's class.
func (m *mdRun) append(recs ...metricsfile.Record) error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	err := m.w.Append(recs...)
	if err != nil {
		m.mu.Lock()
		if m.writeErr == "" {
			m.writeErr = writeClass(err)
		}
		m.mu.Unlock()
	}
	return err
}

// filterChecks asks for a day of 2000 and for everything from 2099: a
// server honouring both bounds of the creation filter lists nothing. ""
// when it does, else the result's part in place of the days.
func (m *mdRun) filterChecks(ctx context.Context) string {
	checks := []struct{ start, end time.Time }{
		{time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)},
		{time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}},
	}
	totals, errs := make([]int, len(checks)), make([]error, len(checks))
	jobs := make([]func(), len(checks))
	for i, c := range checks {
		jobs[i] = func() { _, totals[i], errs[i] = m.list(ctx, c.start, c.end, 0, 1) }
	}
	mdPool(jobs)
	for _, err := range errs {
		if err != nil {
			return "days not listed (" + metrics.ServerErrorClass(err) + ")"
		}
	}
	for _, t := range totals {
		if t != 0 {
			return "the server cannot list memories by creation day"
		}
	}
	return ""
}

// readDays reads whole UTC days at once and writes those read whole, in
// the order given; a huge day is put off to the end of the run, any other
// waits for the next run.
func (m *mdRun) readDays(ctx context.Context, days []time.Time) {
	spans, turns := make([]mdSpan, len(days)), mdTurns(len(days))
	jobs := make([]func(), len(days))
	for i, d := range days {
		jobs[i] = func() { spans[i] = m.readSpan(ctx, d, d.AddDate(0, 0, 1), true, turns[i]) }
	}
	mdPool(jobs)
	for i, d := range days {
		s, day := spans[i], d.Format(time.DateOnly)
		switch {
		case s.huge:
			m.huge = append(m.huge, mdHuge{day: d, total: s.total})
		case !s.whole:
			m.left++
		default:
			recs := append(memoryLines(s.items, day), metricsfile.Listed{Day: day, N: int64(s.total)})
			if m.append(recs...) != nil {
				m.left++
				continue
			}
			m.listed[day] = true
			m.nDays++
			m.nMemories += len(s.items)
		}
	}
}

// forward reads the past days after the oldest listed one that have no
// listed line, oldest first; nothing listed yet: the newest past day.
func (m *mdRun) forward(ctx context.Context, listed []string) {
	newest := utcDate(m.now.Add(-memoryDocsDayLag))
	var days []time.Time
	if len(listed) == 0 {
		days = []time.Time{newest}
	} else if oldest, err := time.Parse(time.DateOnly, listed[0]); err == nil {
		for d := oldest.AddDate(0, 0, 1); !d.After(newest); d = d.AddDate(0, 0, 1) {
			if !m.listed[d.Format(time.DateOnly)] {
				days = append(days, d)
			}
		}
	}
	m.readDays(ctx, days)
}

// backfill reads older days, memoryDocsBatch at once, newest first, while
// a probe before the oldest day reached finds memories; when it finds
// none, the day before is written complete: the record covers the bank.
func (m *mdRun) backfill(ctx context.Context) {
	if m.complete {
		return
	}
	var oldest string
	for d := range m.listed {
		if oldest == "" || d < oldest {
			oldest = d
		}
	}
	for _, h := range m.huge {
		if d := h.day.Format(time.DateOnly); oldest == "" || d < oldest {
			oldest = d
		}
	}
	cursor, err := time.Parse(time.DateOnly, oldest)
	if err != nil { // nothing listed: the newest day is read again first
		m.open = true
		return
	}
	for {
		_, total, err := m.list(ctx, time.Time{}, cursor, 0, 1)
		if err != nil {
			m.open = true
			return
		}
		if total == 0 {
			if m.append(metricsfile.Listed{Day: cursor.AddDate(0, 0, -1).Format(time.DateOnly), N: 0, Complete: true}) != nil {
				m.open = true
				return
			}
			m.complete = true
			return
		}
		batch := make([]time.Time, memoryDocsBatch)
		for i := range batch {
			batch[i] = cursor.AddDate(0, 0, -(i + 1))
		}
		m.readDays(ctx, batch)
		cursor = batch[len(batch)-1]
	}
}

// hugeDays reads each day put off in 24 one-hour windows, oldest first:
// each window read whole is written at once, and the day's listed line
// follows its 24th.
func (m *mdRun) hugeDays(ctx context.Context) {
	slices.SortFunc(m.huge, func(a, b mdHuge) int { return a.day.Compare(b.day) })
	for _, h := range m.huge {
		day := h.day.Format(time.DateOnly)
		var whole, written int
		jobs, turns := make([]func(), 24), mdTurns(24)
		for hr := range jobs {
			jobs[hr] = func() {
				start := h.day.Add(time.Duration(hr) * time.Hour)
				s := m.readSpan(ctx, start, start.Add(time.Hour), false, turns[hr])
				if !s.whole || m.append(memoryLines(s.items, day)...) != nil {
					return
				}
				m.mu.Lock()
				whole, written = whole+1, written+len(s.items)
				m.mu.Unlock()
			}
		}
		mdPool(jobs)
		if whole == len(jobs) && m.append(metricsfile.Listed{Day: day, N: int64(h.total)}) == nil {
			m.listed[day] = true
			m.nDays++
			m.nMemories += written
			continue
		}
		m.tooLarge = append(m.tooLarge, fmt.Sprintf("day %s too large, %d memories", day, h.total))
	}
}

// daysPart is the result's part of the day lists.
func (m *mdRun) daysPart() string {
	s := fmt.Sprintf("%d day(s) listed (%d memories)", m.nDays, m.nMemories)
	switch {
	case !m.complete:
		s += ", older days on the next run"
	case m.left == 0 && len(m.tooLarge) == 0:
		s += ", every day listed"
	}
	if m.left > 0 {
		s += fmt.Sprintf(", %d day(s) left for the next run", m.left)
	}
	for _, p := range m.tooLarge {
		s += ", " + p
	}
	if m.writeErr != "" {
		s += ", not written (" + m.writeErr + ")"
	}
	return s
}

// mdUnits is a document's unit count as read this run.
type mdUnits struct {
	n        int
	ok, read bool // read: the server answered (ok: with a count)
}

// operations records the completed and failed retain operations of this
// PC's session documents sent ok in the last memoryDocsSentReach: a line
// when an operation is new or its status changed, and a later one with the
// document's unit count once read (completed operations only). Its result
// part.
func (m *mdRun) operations(ctx context.Context, ix memoryDocsIndex) string {
	var o, u, k int
	part := func() string {
		s := fmt.Sprintf("%d operation(s), %d unit count(s)", o, u)
		if k > 0 {
			s += fmt.Sprintf(", %d unit count(s) left for the next run", k)
		}
		return s
	}
	sent, err := ix.SentDocuments(ctx, m.now.Add(-memoryDocsSentReach))
	if err != nil || len(sent) == 0 {
		return part()
	}
	recorded, err := ix.RecordedOperations(ctx)
	if err != nil {
		return part()
	}
	sessionOf := make(map[string]string, len(sent))
	for _, s := range sent {
		sessionOf[s.Document] = s.Session
	}
	statuses := []string{"completed", "failed"}
	lists, errs := make([][]hindsight.Operation, len(statuses)), make([]error, len(statuses))
	jobs := make([]func(), len(statuses))
	for i, st := range statuses {
		jobs[i] = func() {
			if errs[i] = ctx.Err(); errs[i] != nil {
				return
			}
			lists[i], errs[i] = boundedWithin(ctx, memoryDocsOpsPages*serverDailyTimeout, func(c context.Context) ([]hindsight.Operation, error) {
				return m.r.RetainOperations(c, st, memoryDocsOpsPages)
			})
		}
	}
	mdPool(jobs)
	var ops []hindsight.Operation
	seen := map[string]bool{}
	for i := range statuses {
		if errs[i] != nil { // nothing of a list that failed
			continue
		}
		for _, op := range lists[i] {
			if _, mine := sessionOf[op.DocumentID]; mine && op.ID != "" && !seen[op.ID] {
				seen[op.ID] = true
				ops = append(ops, op)
			}
		}
	}

	// The unit counts the completed operations lack: one read per
	// document, newest operations first, within the run's reads.
	needs := func(op hindsight.Operation) bool {
		rec, known := recorded[op.ID]
		return mdOpStatus(op.Status) == "completed" && (!known || !rec.HasUnits && !rec.NoUnits)
	}
	var docs []string
	at := map[string]int{}
	for _, op := range ops {
		if _, dup := at[op.DocumentID]; needs(op) && !dup {
			at[op.DocumentID] = len(docs)
			docs = append(docs, op.DocumentID)
		}
	}
	units := make([]mdUnits, len(docs))
	jobs = make([]func(), len(docs))
	for i, doc := range docs {
		jobs[i] = func() {
			if ctx.Err() != nil || m.docs.Add(-1) < 0 {
				return
			}
			type count struct {
				n  int
				ok bool
			}
			c, err := boundedWithin(ctx, serverDailyTimeout, func(cc context.Context) (count, error) {
				n, ok, err := m.r.DocumentUnits(cc, doc)
				return count{n, ok}, err
			})
			if err == nil {
				units[i] = mdUnits{n: c.n, ok: c.ok, read: true}
			}
		}
	}
	mdPool(jobs)

	for _, op := range ops {
		status := mdOpStatus(op.Status)
		line := metricsfile.DocOp{Operation: op.ID, Document: op.DocumentID, Session: sessionOf[op.DocumentID], Status: status,
			CreatedAt: op.CreatedAt, UpdatedAt: op.UpdatedAt, Retries: op.RetryCount}
		if needs(op) {
			switch got := units[at[op.DocumentID]]; {
			case got.read && got.ok:
				n := int64(got.n)
				line.Units = &n
			case got.read: // no count: remembered, not asked again
				line.NoUnits = true
			case !got.read:
				k++
			}
		}
		rec, known := recorded[op.ID]
		if (known && rec.Status == status && line.Units == nil && !line.NoUnits) || m.append(line) != nil {
			continue
		}
		o++
		if line.Units != nil {
			u++
		}
	}
	return part()
}
