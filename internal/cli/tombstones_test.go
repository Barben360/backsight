package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
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
	var purges string
	for _, s := range env.Data.Steps {
		if s.Name == "purges" {
			purges = s.Detail
		}
	}
	if !strings.Contains(purges, "conversation:s8 (2026-10-25T08:00:00Z)") || strings.Contains(purges, "conversation:s2") {
		t.Errorf("purges step: %q", purges)
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
// to re-read. A purged document the archive brings back is named.
func TestRestoreKeepsThePurges(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	seedTombstones(t, home, port)
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
	if got := strings.Join(written, ","); got != retain.TombstonesDocument+","+retain.EpochDocument {
		t.Errorf("control documents written after the restore: %s", got)
	}
	got, err := retain.ReadTombstones(context.Background(), clientOn(port), "claude-memory")
	if err != nil || len(got) != 2 {
		t.Fatalf("control bank after the restore: %+v %v", got, err)
	}
	if !strings.Contains(stepsOf(env), "purges done") || !strings.Contains(strings.Join(env.Data.Warnings, "\n"), "conversation:s8, purged at 2026-10-25T08:00:00Z, came back") ||
		strings.Contains(strings.Join(env.Data.Warnings, "\n"), "conversation:s2") {
		t.Errorf("steps %s, warnings %q", stepsOf(env), env.Data.Warnings)
	}
}
