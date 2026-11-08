package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// sweepBank fronts the fake server with the documents of bank
// claude-memory as Hindsight 1.1.0 holds them for the sweep: the listing
// (no text), each document with its text, the facts of a document,
// clearing a fact's observations, retiring a fact, deleting a document
// with its facts. Everything else (the control bank's tombstones) goes on
// to the fake server.
type sweepBank struct {
	mu       sync.Mutex
	docs     map[string]sweepDoc
	facts    map[string]string // fact id → document
	writes   []string          // METHOD path, in order
	unlocked []string          // writes made without the maintenance lock held
	lockPath string
	// rewrite: once a document is read, its text becomes this (a resumed
	// subagent's new report landing between the sweep's read and purge).
	rewrite map[string]string
}

type sweepDoc struct {
	text    string
	created time.Time
}

func newSweepBank(t *testing.T, home string) *sweepBank {
	t.Helper()
	b := &sweepBank{docs: map[string]sweepDoc{}, facts: map[string]string{}, rewrite: map[string]string{}, lockPath: pathsOf(home).MaintenanceLock()}
	pointAt(t, home, proxyHindsight(t, fakeHindsight(t), b.answer))
	return b
}

func (b *sweepBank) add(id, text string, age time.Duration, facts ...string) {
	b.docs[id] = sweepDoc{text: text, created: time.Now().UTC().Add(-age)}
	for _, f := range facts {
		b.facts[f] = id
	}
}

func (b *sweepBank) docIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for id := range b.docs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (b *sweepBank) answer(w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/default/banks/claude-memory/")
	if !ok {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Method != http.MethodGet {
		b.writes = append(b.writes, r.Method+" /"+rest)
		if h := state.Inspect(b.lockPath); h == nil || h.Stale {
			b.unlocked = append(b.unlocked, r.Method+" /"+rest)
		}
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && rest == "documents":
		var items []string
		for id, d := range b.docs {
			items = append(items, fmt.Sprintf(`{"id":%q,"tags":["source:subagent"],"document_metadata":{},"created_at":%q}`, id, d.created.Format(time.RFC3339Nano)))
		}
		sort.Strings(items)
		fmt.Fprintf(w, `{"items":[%s],"total":%d}`, strings.Join(items, ","), len(items))
	case strings.HasPrefix(rest, "documents/"):
		id := strings.TrimPrefix(rest, "documents/")
		d, ok := b.docs[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		n := 0
		for f, doc := range b.facts {
			if doc == id {
				n++
				if r.Method == http.MethodDelete {
					delete(b.facts, f)
				}
			}
		}
		if r.Method == http.MethodDelete {
			delete(b.docs, id)
		}
		if nt, ok := b.rewrite[id]; ok && r.Method == http.MethodGet {
			delete(b.rewrite, id)
			b.docs[id] = sweepDoc{text: nt, created: d.created}
		}
		text, _ := json.Marshal(d.text)
		fmt.Fprintf(w, `{"id":%q,"memory_unit_count":%d,"original_text":%s,"document_metadata":{}}`, id, n, text)
	case r.Method == http.MethodGet && rest == "memories/list":
		var items []string
		if off := q.Get("offset"); off == "" || off == "0" {
			for f, doc := range b.facts {
				if doc == q.Get("document_id") {
					items = append(items, fmt.Sprintf(`{"id":%q,"text":"t","fact_type":"world","document_id":%q,"state":"valid","tags":["project:billing"]}`, f, doc))
				}
			}
		}
		fmt.Fprintf(w, `{"items":[%s],"total":%d}`, strings.Join(items, ","), len(items))
	case strings.HasPrefix(rest, "memories/") && strings.HasSuffix(rest, "/observations") && r.Method == http.MethodDelete:
		_, _ = w.Write([]byte(`{"deleted_count":1}`))
	case strings.HasPrefix(rest, "memories/") && r.Method == http.MethodPatch:
		if _, ok := b.facts[strings.TrimPrefix(rest, "memories/")]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	default:
		return false
	}
	return true
}

// sweepFixture: three stubs (with 2, 0 and 1 facts), a long report, short
// ones with a URL and a named PR, an empty text with facts (unread), an
// empty one without (a stub), and a conversation.
func sweepFixture(t *testing.T) (string, *sweepBank) {
	t.Helper()
	home := devInstall(t, true)
	b := newSweepBank(t, home)
	appendConfig(t, home, "[backup]\nenabled = false") // no docker here
	day := 48 * time.Hour
	b.add("subagent:s1:a1", "Keep going with block 2", day, "f1", "f2")
	b.add("subagent:s1:a2", "Reading x.go", day)
	b.add("subagent:s2:a1", "I'm waiting for CI", day, "f3")
	b.add("subagent:s2:a2", padTo("Found the leak in pool.go: the pool never closed idle conns", 600), day, "f4")
	b.add("subagent:s3:a1", "Opened https://github.com/o/r/pull/12", day, "f5")
	b.add("subagent:s4:a1", "", day, "f7")
	b.add("subagent:s5:a1", "", day)
	b.add("subagent:s6:a1", "Merged PR 205", time.Hour)
	b.add("conversation:s1", "ok", day, "f6")
	return home, b
}

func sweep(t *testing.T, home string, backupFailed bool) string {
	t.Helper()
	d := pathsOf(home)
	return maintainSubagentSweep(context.Background(), d, telemetry.New(d.State, telemetry.Common{}), backupFailed)
}

func TestMaintainSweepsTheStubSubagentReportsOncePerGate(t *testing.T) {
	home, b := sweepFixture(t)
	code, out, errOut := run(t, "jobs", "maintain")
	if code != output.OK && code != output.Partial {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "subagent sweep: 4 short subagent report(s) purged (3 fact(s)) of 8, 1 with facts but no readable text left") {
		t.Fatalf("maintain:\n%s", out)
	}
	want := []string{"conversation:s1", "subagent:s2:a2", "subagent:s3:a1", "subagent:s4:a1", "subagent:s6:a1"}
	if got := b.docIDs(); !slices.Equal(got, want) {
		t.Fatalf("documents left %v, want %v", got, want)
	}
	// Through the purge path: each fact's observations cleared and the
	// fact retired, then the documents deleted, all under the lock.
	writes := strings.Join(b.writes, "\n")
	for _, w := range []string{"DELETE /memories/f1/observations", "PATCH /memories/f2", "DELETE /memories/f3/observations", "DELETE /documents/subagent:s1:a2", "DELETE /documents/subagent:s5:a1"} {
		if !strings.Contains(writes, w) {
			t.Errorf("no %s in the writes:\n%s", w, writes)
		}
	}
	for _, kept := range []string{"f4", "f5", "f6", "f7"} {
		if strings.Contains(writes, "/"+kept) {
			t.Errorf("a kept document's fact %s was touched:\n%s", kept, writes)
		}
	}
	if len(b.unlocked) > 0 {
		t.Errorf("written without the maintenance lock: %v", b.unlocked)
	}
	st, err := state.Load(pathsOf(home).StateFile())
	if err != nil {
		t.Fatal(err)
	}
	// No tombstone: a resumed subagent may report again under a swept id.
	if tombs := st.TombstonesOf("claude-memory"); len(tombs) != 0 {
		t.Errorf("tombstones %+v", tombs)
	}
	if m := st.SubagentSweep; m == nil || m.Purged != 4 || m.Gate != "v2:min_chars=400" || m.Epoch != 0 {
		t.Errorf("marker %+v", m)
	}
	ev := eventsNamed(t, home, "subagent_sweep")
	if len(ev) != 1 || ev[0]["status"] != "done" || ev[0]["purged"] != float64(4) || ev[0]["facts"] != float64(3) || ev[0]["checked"] != float64(8) || ev[0]["unread"] != float64(1) {
		t.Errorf("events %v", ev)
	}

	// The same gate: nothing read, nothing written.
	b.writes = nil
	if line := sweep(t, home, false); line != "" || len(b.writes) != 0 {
		t.Errorf("second run %q, writes %v", line, b.writes)
	}

	// A new threshold sweeps again: the 600-character report is now short.
	appendConfig(t, home, "[hooks]\nsubagent_report_min_chars = 1000")
	if line := sweep(t, home, false); !strings.Contains(line, "1 short subagent report(s) purged") {
		t.Errorf("after the threshold changed: %q", line)
	}
	if got := b.docIDs(); !slices.Equal(got, []string{"conversation:s1", "subagent:s3:a1", "subagent:s4:a1", "subagent:s6:a1"}) {
		t.Errorf("documents left %v", got)
	}
}

// A subagent resumed after the sweep reports again under its swept id: the
// report is kept, and the daily look for recreated purged documents leaves
// it alone.
func TestASweptReportSentAgainSurvivesThePurgeGuard(t *testing.T) {
	home, b := sweepFixture(t)
	if line := sweep(t, home, false); !strings.Contains(line, "4 short subagent report(s) purged") {
		t.Fatalf("sweep: %q", line)
	}
	b.mu.Lock()
	b.add("subagent:s1:a1", padTo("Resumed: the pool leak was the idle timeout, fixed in pool.go", 600), 0, "f9")
	b.writes = nil
	b.mu.Unlock()
	d := pathsOf(home)
	_ = guardMaintain(context.Background(), d, home)
	if !slices.Contains(b.docIDs(), "subagent:s1:a1") {
		t.Errorf("the purge guard deleted the resumed report; writes:\n%s", strings.Join(b.writes, "\n"))
	}
	if line := sweep(t, home, false); line != "" {
		t.Errorf("a second sweep ran: %q", line)
	}
}

// A restore brings the swept reports back: the next sweep after its epoch
// reached this PC runs again.
func TestARestoreRunsTheSweepAgain(t *testing.T) {
	home, b := sweepFixture(t)
	if line := sweep(t, home, false); !strings.Contains(line, "4 short subagent report(s) purged") {
		t.Fatalf("sweep: %q", line)
	}
	b.mu.Lock()
	b.add("subagent:s1:a2", "Reading x.go", 48*time.Hour) // restored
	b.mu.Unlock()
	if line := sweep(t, home, false); line != "" {
		t.Fatalf("before the epoch reached this PC: %q", line)
	}
	d := pathsOf(home)
	if err := state.Update(d, "test", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: "claude-memory", Epoch: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if line := sweep(t, home, false); !strings.Contains(line, "1 short subagent report(s) purged") {
		t.Errorf("after the restore: %q", line)
	}
	if slices.Contains(b.docIDs(), "subagent:s1:a2") {
		t.Error("the restored stub is still there")
	}
}

// A report rewritten between the sweep's read and its purge (a resumed
// subagent) is read again under the lock and left.
func TestTheSweepLeavesAReportThatChangedBeforeItsPurge(t *testing.T) {
	home, b := sweepFixture(t)
	b.rewrite["subagent:s1:a2"] = "Reading x.go again"                     // still short, but not what was judged
	b.rewrite["subagent:s2:a1"] = padTo("Found it: the CI cache key", 600) // now passes
	if line := sweep(t, home, false); !strings.Contains(line, "2 short subagent report(s) purged (2 fact(s)) of 8") {
		t.Fatalf("sweep: %q", line)
	}
	left := b.docIDs()
	for _, id := range []string{"subagent:s1:a2", "subagent:s2:a1"} {
		if !slices.Contains(left, id) {
			t.Errorf("%s was purged after it changed", id)
		}
	}
	if strings.Contains(strings.Join(b.writes, "\n"), "/memories/f3") {
		t.Error("the changed report's fact was touched")
	}
	if ev := eventsNamed(t, home, "subagent_sweep"); len(ev) != 1 || ev[0]["changed"] != float64(2) {
		t.Errorf("events %v", ev)
	}
}

func TestMaintainSweepWaitsForTheLockTheServerAndTheBackup(t *testing.T) {
	home, b := sweepFixture(t)
	d := pathsOf(home)
	all := b.docIDs()
	check := func(what, line string) {
		t.Helper()
		if !strings.Contains(line, "subagent sweep: not run") || !strings.Contains(line, "the next run tries again") {
			t.Errorf("%s: %q", what, line)
		}
		if got := b.docIDs(); !slices.Equal(got, all) {
			t.Errorf("%s: documents left %v", what, got)
		}
		if st, _ := state.Load(d.StateFile()); st.SubagentSweep != nil {
			t.Errorf("%s: marker written %+v", what, st.SubagentSweep)
		}
	}

	// A backup (or a purge) holds the maintenance lock.
	l, err := state.TryAcquire(d.MaintenanceLock(), "backup create")
	if err != nil {
		t.Fatal(err)
	}
	check("lock held", sweep(t, home, false))
	_ = l.Release()

	// Today's backup failed: the sweep waits for one that holds its data.
	check("backup failed", sweep(t, home, true))

	// The server does not answer.
	cfg := readFile(t, d.ConfigFile())
	pointAt(t, home, closedPort(t))
	check("server down", sweep(t, home, false))

	// The next day, everything back: it runs.
	if err := os.WriteFile(d.ConfigFile(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if line := sweep(t, home, false); !strings.Contains(line, "4 short subagent report(s) purged") {
		t.Errorf("retry: %q", line)
	}
}
