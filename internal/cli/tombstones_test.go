package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

func clientOn(port int) *hindsight.Client {
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "", 5*time.Second)
	return cl
}

// withDocMeta answers GET of document doc with its metadata, like the
// server for a sent conversation segment.
func withDocMeta(t *testing.T, port int, doc, meta string) int {
	t.Helper()
	return proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/claude-memory/documents/"+doc) {
			_, _ = w.Write([]byte(`{"id":"` + doc + `","memory_unit_count":3,"document_metadata":` + meta + `}`))
			return true
		}
		return false
	})
}

// forget --purge records the document's tombstone (its session, project
// and last record) in state.json, and mirrors it into the control bank,
// for the other machines and a reinstall.
func TestPurgeRecordsATombstone(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, withDocMeta(t, port, "conversation:s9", `{"session_id":"s9","project":"billing","last_uuid":"u7","segment":"2"}`))
	withClock(t, time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC))
	memories["f9"] = fact("f9", "conversation:s9")
	if code, out, errOut := run(t, "forget", "f9", "--purge", "--yes"); code != output.OK || strings.Contains(out, "not yet in bank") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	want := state.Tombstone{Bank: "claude-memory", Document: "conversation:s9", Session: "s9", Project: "billing", LastUUID: "u7",
		PurgedAt: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
	st, _ := state.Load(pathsOf(home).StateFile())
	if len(st.Tombstones) != 1 || st.Tombstones[0] != want {
		t.Errorf("state: %+v", st.Tombstones)
	}
	if got, err := retain.ReadTombstones(context.Background(), clientOn(port), "claude-memory"); err != nil || len(got) != 1 || got[0] != want {
		t.Errorf("control bank: %+v %v", got, err)
	}
}

// A purged note's tombstone keeps a hash of its text: a backfill replays
// the save under another document id than the one stored live.
func TestPurgeOfANoteHashesItsText(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/claude-memory/documents/note:n1") {
			_, _ = w.Write([]byte(`{"id":"note:n1","memory_unit_count":1,"original_text":"Decision: the deploy uses hunter2.","document_metadata":{"source":"note"}}`))
			return true
		}
		return false
	}))
	memories["f1"] = fact("f1", "note:n1")
	if code, out, errOut := run(t, "forget", "f1", "--purge", "--yes"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if len(st.Tombstones) != 1 || st.Tombstones[0].ContentSHA256 != retain.ContentHash("Decision: the deploy uses hunter2.") || st.Tombstones[0].LastUUID != "" {
		t.Errorf("tombstones %+v", st.Tombstones)
	}
	if raw, _ := os.ReadFile(pathsOf(home).StateFile()); strings.Contains(string(raw), "hunter2") {
		t.Error("the note's text is in state.json")
	}
}

// The tombstone is written before anything is deleted: a purge whose
// document delete fails still holds it. A control bank that is not
// backsight's gets nothing: the purge says so and still succeeds.
func TestPurgeTombstoneComesFirstAndSurvivesAForeignControlBank(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, withDocMeta(t, port, "conversation:s9", `{"session_id":"s9","project":"billing","last_uuid":"u7"}`))
	memories["f9"] = fact("f9", "conversation:s9")
	docDeleteFails.Store(true)
	if code, _, _ := run(t, "forget", "f9", "--purge", "--yes"); code == output.OK {
		t.Fatal("the failed delete passed")
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); len(st.Tombstones) != 1 || st.Tombstones[0].LastUUID != "u7" {
		t.Errorf("no tombstone after a failed delete: %+v", st.Tombstones)
	}
	docDeleteFails.Store(false)
	if err := clientOn(port).CreateBank(context.Background(), "claude-memory-control", "someone else's", ""); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "forget", "f9", "--purge", "--yes")
	if code != output.OK || !strings.Contains(out, "not yet in bank claude-memory-control") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); len(st.Tombstones) != 1 {
		t.Errorf("tombstones: %+v", st.Tombstones)
	}
	if got, _ := retain.ReadTombstones(context.Background(), clientOn(port), "claude-memory"); len(got) != 0 {
		t.Errorf("written into a foreign control bank: %+v", got)
	}
}

// seedTombstones: conversation:s2 purged at 03:00 on this machine only
// (before the 04:00 archive), conversation:s8 purged at 08:00 on another
// machine (in the control bank only, after the archive).
func seedTombstones(t *testing.T, home string, port int) {
	t.Helper()
	early := state.Tombstone{Bank: "claude-memory", Document: "conversation:s2", Session: "s2", Project: "billing", LastUUID: "a2",
		PurgedAt: time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)}
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error { s.AddTombstones(early); return nil }); err != nil {
		t.Fatal(err)
	}
	late := state.Tombstone{Bank: "claude-memory", Document: "conversation:s8", Session: "s8", Project: "infra", LastUUID: "a8",
		PurgedAt: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)}
	if _, err := retain.WriteTombstones(context.Background(), clientOn(port), "claude-memory", []state.Tombstone{late}); err != nil {
		t.Fatal(err)
	}
	bankCalls = nil
}

// restore --plan lists the purges made after the archive was taken, the
// control bank's and this machine's, and writes nothing.
func TestRestorePlanListsThePurgesAfterTheArchive(t *testing.T) {
	home, archive, _, port := restoreSetup(t)
	seedTombstones(t, home, port)
	code, out, errOut := run(t, "restore", archive, "--plan", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var env struct {
		Data struct {
			Steps  []step            `json:"steps"`
			Purged []state.Tombstone `json:"purged_after_archive"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Purged) != 1 || env.Data.Purged[0].Document != "conversation:s8" {
		t.Errorf("purged after the archive: %+v", env.Data.Purged)
	}
	var purges, again string
	for _, s := range env.Data.Steps {
		switch s.Name {
		case "purges":
			purges = s.Detail
		case "purge again":
			again = s.Detail
		}
	}
	if !strings.Contains(purges, "conversation:s8 (2026-10-25T08:00:00Z)") || strings.Contains(purges, "conversation:s2") {
		t.Errorf("purges step: %q", purges)
	}
	// The documents the archive may bring back, which restore deletes again.
	// Every tombstone is checked (2); only those after the archive are named.
	if !strings.Contains(again, "would check every tombstone (2)") || !strings.Contains(again, "conversation:s8 (2026-10-25T08:00:00Z)") || strings.Contains(again, "conversation:s2") {
		t.Errorf("purge again step: %q", again)
	}
	for _, c := range bankCalls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("a plan wrote to the server: %q", c)
		}
	}
	if code, out, _ := run(t, "restore", archive, "--plan"); code != output.OK || !strings.Contains(out, "purged after the archive was taken: conversation:s8") {
		t.Errorf("text plan: exit %d\n%s", code, out)
	}
}

// The restored control bank is the archive's, without the purges made
// since: restore writes every purge it knew (the server's before the stop
// and this machine's) back into it, before the epoch that sends the clients
// to re-read. A purged document the archive brings back is deleted again,
// after the epoch, still on the temporary key (issue 285).
func TestRestoreKeepsThePurges(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	seedTombstones(t, home, port)
	sec, _ := config.LoadSecrets(secretsFile(home))
	documentUnits["conversation:s8"] = 4 // the archive holds the document purged since
	var mu sync.Mutex
	var written []string // control-bank documents written after the database was replaced
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		restored := dk.restored() > 0
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claude-memory-control/memories") && restored {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var b struct {
				Items []struct {
					DocumentID string `json:"document_id"`
				} `json:"items"`
			}
			_ = json.Unmarshal(body, &b)
			written = append(written, b.Items[0].DocumentID)
		}
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/documents/") && restored {
			written = append(written, "DELETE "+r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]+" "+strconv.FormatBool(r.Header.Get("Authorization") != "Bearer "+sec.APIKey))
		}
		// the archive's control bank: no tombstones until restore writes them
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/documents/"+retain.TombstonesDocument) && restored && len(written) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return false
	}))
	code, env, out, errOut := runRestoreJSON(t, archive)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	// The tombstones, the epoch, then the delete on the temporary key.
	if got := strings.Join(written, ","); got != retain.TombstonesDocument+","+retain.EpochDocument+",DELETE conversation:s8 true" {
		t.Errorf("writes after the restore: %s", got)
	}
	got, err := retain.ReadTombstones(context.Background(), clientOn(port), "claude-memory")
	if err != nil || len(got) != 2 {
		t.Fatalf("control bank after the restore: %+v %v", got, err)
	}
	if !strings.Contains(stepsOf(env), "purges done, epoch done, purge again done, bank done, key done") {
		t.Errorf("steps %s", stepsOf(env))
	}
	var again string
	for _, st := range env.Data.Steps {
		if st.Name == "purge again" {
			again = st.Detail
		}
	}
	if !strings.Contains(again, "bank claude-memory, 1 document(s): conversation:s8") {
		t.Errorf("purge again: %q", again)
	}
	// conversation:s2, which the archive does not hold, is not deleted.
	if strings.Contains(strings.Join(curation, "\n"), "s2") {
		t.Errorf("deleted what the archive did not bring back: %q", curation)
	}
}

// A purged document the archive brings back is purged again the way
// forget --purge purges: its facts' observations are cleared (they may
// quote it), the document goes, and so does the briefing page of each
// project its facts were tagged with; the step names all three (issue 285
// review H1).
func TestRestorePurgesAgainLikeAPurge(t *testing.T) {
	home, archive, _, port := restoreSetup(t)
	seedTombstones(t, home, port)
	documentUnits["conversation:s8"] = 1
	f8 := `{"id":"f8","text":"the deploy token","type":"world","tags":["project:infra"],"document_id":"conversation:s8"}`
	listAnswer[""] = `[` + f8 + `]`
	memories["f8"] = f8
	page := briefingModelID("infra")
	mentalModels[page] = `{"id":"` + page + `","content":"infra: the deploy token is …"}`
	curation, mentalCalls = nil, nil
	code, env, out, errOut := runRestoreJSON(t, archive)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	// In forget --purge's order: the observations while the fact is still
	// seen, the fact retired, then the document.
	at := func(prefix string) int {
		for i, c := range curation {
			if strings.HasPrefix(c, prefix) {
				return i
			}
		}
		return -1
	}
	obs, retire, doc := at("DELETE /v1/default/banks/claude-memory/memories/f8/observations"), at(`PATCH /v1/default/banks/claude-memory/memories/f8 {"reason":"purged by the user`),
		at("DELETE /v1/default/banks/claude-memory/documents/conversation%3As8")
	if obs < 0 || retire < 0 || doc < 0 || obs >= retire || retire >= doc {
		t.Errorf("observations %d, retire %d, document %d in\n%s", obs, retire, doc, strings.Join(curation, "\n"))
	}
	if !strings.Contains(strings.Join(mentalCalls, "\n"), "DELETE "+page) {
		t.Errorf("the briefing page that may quote it is kept: %q", mentalCalls)
	}
	var detail string
	for _, st := range env.Data.Steps {
		if st.Name == "purge again" {
			detail = st.Detail
		}
	}
	if !strings.Contains(detail, "conversation:s8") || !strings.Contains(detail, "observations of 1 fact(s) cleared") || !strings.Contains(detail, page) {
		t.Errorf("purge again: %q", detail)
	}
}

// A retain queued in the archive runs once the restored server starts, and
// would recreate a purged document after it was purged again (append
// recreates it): restore waits for the queues first, with no deadline,
// still on the temporary key and with the marker at its purge step; it
// prints its progress (issue 285 review M1, MED-1).
func TestRestoreWaitsForTheRestoredQueueBeforePurgingAgain(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	seedTombstones(t, home, port)
	documentUnits["conversation:s8"] = 1
	oldPoll := queuePoll
	queuePoll = time.Millisecond
	t.Cleanup(func() { queuePoll = oldPoll })
	var mu sync.Mutex
	left, atDelete := 3, -1
	var markerAtWait string
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if dk.restored() == 0 {
			return false
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/claude-memory/operations") && r.URL.Query().Get("status") == "pending":
			if m, _ := state.ReadRestoreMarker(pathsOf(home)); m != nil && left > 0 {
				markerAtWait = m.Step
			}
			_, _ = w.Write([]byte(`{"total":` + strconv.Itoa(left) + `,"operations":[]}`))
			if left > 0 {
				left--
			}
			return true
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/documents/conversation:s8"):
			atDelete = left
		}
		return false
	}))
	code, env, out, errOut := runRestoreJSON(t, archive)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if atDelete != 0 || markerAtWait != state.PurgeStep || !strings.Contains(stepsOf(env), "purge waiting") {
		t.Errorf("queue at the delete %d, marker step while waiting %q, steps %s", atDelete, markerAtWait, stepsOf(env))
	}
}

// Interrupted while it waits (Ctrl-C: its context cancelled), restore
// leaves the server on its temporary key, the marker at its purge step,
// and exits 40 naming server start, which resumes: it waits, purges again
// on a temporary key, applies the bank template the restore did not reach,
// then starts the server on its API key and finishes (issue 285 review
// MED-1, MED-B). Doctor names it too.
func TestRestoreInterruptedWhileWaitingResumesWithServerStart(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	seedTombstones(t, home, port)
	documentUnits["conversation:s8"] = 1
	sec, _ := config.LoadSecrets(secretsFile(home))
	oldPoll := queuePoll
	queuePoll = time.Millisecond
	t.Cleanup(func() { queuePoll = oldPoll })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	drained := false
	var deletes []string // the key each purge-again delete came with
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if dk.restored() == 0 {
			return false
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/claude-memory/operations") && r.URL.Query().Get("status") == "pending":
			n := 1
			if drained {
				n = 0
			} else {
				cancel() // Ctrl-C while it waits
			}
			_, _ = w.Write([]byte(`{"total":` + strconv.Itoa(n) + `,"operations":[]}`))
			return true
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/documents/conversation:s8"):
			deletes = append(deletes, strconv.FormatBool(r.Header.Get("Authorization") == "Bearer "+sec.APIKey))
		}
		return false
	}))
	var outb, errb bytes.Buffer
	code := ExecuteContext(ctx, []string{"restore", archive, "--json"}, &outb, &errb)
	out := outb.String()
	if code != output.Partial || !strings.Contains(out, `"resume": "backsight server start"`) {
		t.Fatalf("exit %d\n%s%s", code, out, errb.String())
	}
	d := pathsOf(home)
	m, err := state.ReadRestoreMarker(d)
	if err != nil || m == nil || m.Step != state.PurgeStep || !slices.Contains(m.PurgeBanks, "claude-memory") || len(deletes) != 0 {
		t.Fatalf("marker %+v %v, deletes %v", m, err, deletes)
	}
	if k := dk.keys[len(dk.keys)-1]; k == sec.APIKey {
		t.Error("the server runs on its API key while the purge waits")
	}
	if _, out, _ := run(t, "doctor"); !strings.Contains(out, "backsight server start (it waits for the queues") {
		t.Errorf("doctor:\n%s", out)
	}
	// The bank step was not reached: server start does it.
	if err := state.Update(d, "test", func(s *state.State) error { s.BankConfigHash = "stale"; return nil }); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	drained = true
	mu.Unlock()
	code, out, errOut := run(t, "server", "start")
	if code != output.OK || !strings.Contains(out, "is finished") || !strings.Contains(out, "conversation:s8") {
		t.Fatalf("server start: exit %d\n%s%s", code, out, errOut)
	}
	if !slices.Equal(deletes, []string{"false"}) || state.RestoreIncomplete(d) || dk.keys[len(dk.keys)-1] != sec.APIKey {
		t.Errorf("deletes (on the API key?) %v, marker %v, key %v", deletes, state.RestoreIncomplete(d), dk.keys[len(dk.keys)-1] == sec.APIKey)
	}
	if st, _ := state.Load(d.StateFile()); st.BankConfigHash != server.TemplateHash() {
		t.Errorf("the resumed restore did not bring the bank to the template: hash %q", st.BankConfigHash)
	}
}

// stuckQueue answers the claude-memory queue after the restore with one
// pending operation forever (or status instead of an answer when not 0),
// listing it when asked.
func stuckQueue(t *testing.T, home string, port int, dk *restoreDocker, status int) {
	t.Helper()
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		if dk.restored() == 0 || !strings.HasSuffix(r.URL.Path, "/claude-memory/operations") {
			return false
		}
		if status != 0 {
			http.Error(w, `{"detail":"nope"}`, status)
			return true
		}
		switch r.URL.Query().Get("status") {
		case "pending":
			_, _ = w.Write([]byte(`{"total":1,"operations":[{"id":"op-7","operation_type":"batch_retain","status":"pending","document_id":"conversation:s8"}]}`))
		default:
			_, _ = w.Write([]byte(`{"total":0,"operations":[]}`))
		}
		return true
	}))
}

// A queue that does not move for stallAfter stops the wait: exit 40, the
// marker at its purge step with the operations named, which doctor shows
// with both ways out; an unreadable queue stalls the same way (issue 285
// review MED-A).
func TestRestoreStallsOnAQueueThatDoesNotMove(t *testing.T) {
	for name, status := range map[string]int{"never drains": 0, "unreadable": http.StatusInternalServerError} {
		t.Run(name, func(t *testing.T) {
			home, archive, dk, port := restoreSetup(t)
			seedTombstones(t, home, port)
			oldPoll, oldStall := queuePoll, stallAfter
			queuePoll, stallAfter = time.Millisecond, 30*time.Millisecond
			t.Cleanup(func() { queuePoll, stallAfter = oldPoll, oldStall })
			stuckQueue(t, home, port, dk, status)
			code, _, out, errOut := runRestoreJSON(t, archive)
			want := "op-7 (batch_retain, pending, document conversation:s8)"
			if status != 0 {
				want = "queue unreadable"
			}
			if code != output.Partial || !strings.Contains(out, want) || !strings.Contains(out, `"resume": "backsight server start"`) {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			m, _ := state.ReadRestoreMarker(pathsOf(home))
			if m == nil || m.Step != state.PurgeStep || !strings.Contains(strings.Join(m.Stuck, "\n"), want) {
				t.Fatalf("marker %+v", m)
			}
			_, doc, _ := run(t, "doctor")
			if !strings.Contains(doc, want) || !strings.Contains(doc, "backsight server start") || !strings.Contains(doc, m.Safety+" --no-safety") {
				t.Errorf("doctor:\n%s", doc)
			}
		})
	}
}

// A bank the server answers not found for has no queue: restore does not
// wait on it (issue 285 review MED-A).
func TestRestoreQueueOfAMissingBankIsEmpty(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	seedTombstones(t, home, port)
	oldPoll, oldStall := queuePoll, stallAfter
	queuePoll, stallAfter = time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { queuePoll, stallAfter = oldPoll, oldStall })
	stuckQueue(t, home, port, dk, http.StatusNotFound)
	if code, _, out, errOut := runRestoreJSON(t, archive); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

// server start on a purge-step marker whose banks' tombstones cannot be
// read keeps the marker and names both ways out (issue 285 review LOW-B).
func TestResumePurgeWithUnreadableTombstones(t *testing.T) {
	home, _, dk, port := restoreSetup(t)
	d := pathsOf(home)
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/claude-memory-control/documents/"+retain.TombstonesDocument) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return true
		}
		return false
	}))
	if code, out, _ := run(t, "server", "render"); code != output.OK {
		t.Fatalf("render %d %s", code, out)
	}
	if err := state.WriteRestoreMarker(d, state.RestoreMarker{Command: "restore", Safety: "/b/s.age", Archive: "/b/a.age",
		Step: state.PurgeStep, PurgeBanks: []string{"claude-memory"}}); err != nil {
		t.Fatal(err)
	}
	dk.calls = nil
	code, _, errOut := run(t, "server", "start")
	if code != output.Blocked || !strings.Contains(errOut, "backsight restore /b/s.age --no-safety") || !strings.Contains(errOut, "rerun backsight server start") {
		t.Errorf("exit %d %s", code, errOut)
	}
	if m, _ := state.ReadRestoreMarker(d); m == nil || m.Step != state.PurgeStep {
		t.Errorf("marker %+v", m)
	}
}
