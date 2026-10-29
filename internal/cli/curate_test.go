package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

// proxyHindsight fronts the fake server on port; a request answer reports
// it answered itself is not forwarded.
func proxyHindsight(t *testing.T, port int, answer func(w http.ResponseWriter, r *http.Request) bool) int {
	t.Helper()
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	rp := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !answer(w, r) {
			rp.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	p, _ := strconv.Atoi(u.Port())
	return p
}

func fact(id, doc string) string {
	return `{"id":"` + id + `","text":"a fact","type":"world","tags":["project:billing"],"document_id":"` + doc + `"}`
}

func TestForgetARawFactIsReversible(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["f1"] = fact("f1", "conversation:s1")
	if code, out, _ := run(t, "forget", "f1"); code != output.OK || !strings.Contains(out, "--restore") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	run(t, "forget", "--restore", "f1")
	got := strings.Join(curation, "\n")
	if !strings.Contains(got, `PATCH /v1/default/banks/claude-memory/memories/f1 {"reason":"forgotten by the user (backsight forget)","state":"invalidated"}`) ||
		!strings.Contains(got, `"state":"valid"`) {
		t.Errorf("calls:\n%s", got)
	}
	if code, _, _ := run(t, "forget", "nope"); code != output.Usage {
		t.Errorf("unknown id: exit %d", code)
	}
	if code, _, _ := run(t, "forget", "f1", "--purge", "--restore"); code != output.Usage {
		t.Errorf("--purge with --restore: exit %d", code)
	}
}

// While another server answers at the joined address, forget and forget
// --restore write nothing to it (decision 0073); once the joined one
// answers again, they do.
func TestForgetWritesNothingToAnotherServer(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["f1"] = fact("f1", "conversation:s1")
	recordMismatch(t, home)
	for _, args := range [][]string{{"forget", "f1"}, {"forget", "--restore", "f1"}} {
		code, out, errOut := run(t, args...)
		if code != output.Blocked || !strings.Contains(out+errOut, "a different server answers at this address (server b, joined a)") {
			t.Errorf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
	if len(curation) != 0 {
		t.Errorf("written to another server: %q", curation)
	}
	setMismatch(t, home, "")
	if code, out, errOut := run(t, "forget", "f1"); code != output.OK || len(curation) == 0 {
		t.Errorf("the joined server: exit %d, %q\n%s%s", code, curation, out, errOut)
	}
}

// Hindsight curates raw facts only: forgetting an observation retires its
// source facts (and the observation regenerates without them).
func TestForgetAnObservationRetiresItsSources(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["o1"] = `{"id":"o1","text":"obs","type":"observation","tags":["project:billing"],"source_memories":[{"id":"f1","type":"world"},{"id":"f2","type":"world"}]}`
	memories["f1"], memories["f2"] = fact("f1", ""), fact("f2", "")
	if code, out, _ := run(t, "forget", "o1"); code != output.OK || !strings.Contains(out, "2 source fact") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	got := strings.Join(curation, "\n")
	if strings.Contains(got, "memories/o1 ") || !strings.Contains(got, "memories/f1 ") || !strings.Contains(got, "memories/f2 ") {
		t.Errorf("calls:\n%s", got)
	}
}

func TestPurgeOrderForAFactAndAnObservation(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["f9"] = fact("f9", "conversation:s9")
	documentUnits["conversation:s9"] = 14
	code, out, errOut := run(t, "forget", "f9", "--purge", "--json")
	if code != output.NeedsUser || !strings.Contains(out, `"memories": 14`) || len(curation) != 0 {
		t.Fatalf("plan: exit %d, %d calls\n%s%s", code, len(curation), out, errOut)
	}
	if code, out, _ := run(t, "forget", "f9", "--purge", "--yes"); code != output.OK || !strings.Contains(out, "deleted") || !strings.Contains(out, "all 14 memories") {
		t.Fatalf("purge: exit %d\n%s", code, out)
	}
	// observations are cleared while the fact is live, then it is retired,
	// then its document goes
	want := []string{"DELETE /v1/default/banks/claude-memory/memories/f9/observations ", "PATCH /v1/default/banks/claude-memory/memories/f9 ",
		"DELETE /v1/default/banks/claude-memory/documents/conversation%3As9 "}
	if len(curation) != len(want) {
		t.Fatalf("calls: %q", curation)
	}
	for i, w := range want {
		if !strings.HasPrefix(curation[i], w) {
			t.Errorf("call %d: want %q, got %q", i, w, curation[i])
		}
	}
}

// Like the real API, an observation's source list carries no document: the
// purge reads each source fact for it.
func TestPurgeAnObservationDeletesItsSourcesDocuments(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["o1"] = `{"id":"o1","text":"obs","type":"observation","source_memories":[{"id":"f1","type":"world"},{"id":"f2","type":"world"}]}`
	memories["f1"], memories["f2"] = fact("f1", "conversation:s1"), fact("f2", "conversation:s1")
	documentUnits["conversation:s1"] = 3
	if code, out, _ := run(t, "forget", "o1", "--purge", "--yes"); code != output.OK || !strings.Contains(out, "2 source fact") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	got := strings.Join(curation, "\n")
	if strings.Contains(got, "memories/o1") || strings.Count(got, "documents/conversation%3As1") != 1 ||
		!strings.Contains(got, "PATCH /v1/default/banks/claude-memory/memories/f2 ") ||
		strings.LastIndex(got, "memories/f") > strings.Index(got, "documents/") {
		t.Errorf("calls:\n%s", got)
	}
}

// A soft-forgotten fact (what memory scan also lists) is only in the
// archive: the purge deletes its document without curating it again.
func TestPurgeAForgottenFactDeletesItsDocumentOnly(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["f3"] = `{"id":"f3","text":"a fact","type":"world","state":"invalidated","document_id":"conversation:s3"}`
	documentUnits["conversation:s3"] = 2
	if code, out, _ := run(t, "forget", "f3", "--purge", "--yes"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if len(curation) != 1 || !strings.HasPrefix(curation[0], "DELETE /v1/default/banks/claude-memory/documents/conversation%3As3 ") {
		t.Errorf("calls: %q", curation)
	}
}

// A fact with no document cannot be deleted through the API: the purge
// retires it and says it was not deleted.
func TestPurgeSaysWhatItCouldNotDelete(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["f4"] = fact("f4", "")
	code, out, errOut := run(t, "forget", "f4", "--purge", "--yes", "--json")
	if code != output.OK || !strings.Contains(out, `"action": "partly_purged"`) || !strings.Contains(out, "\"undeletable\": [\n      \"f4\"") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := strings.Join(curation, "\n"); !strings.Contains(got, "PATCH /v1/default/banks/claude-memory/memories/f4 ") || strings.Contains(got, "documents/") {
		t.Errorf("calls:\n%s", got)
	}
}

// A source fact gone from the server is reported as such, not as "no
// memory o1".
func TestPurgeReportsAMissingSourceFact(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["o1"] = `{"id":"o1","text":"obs","type":"observation","source_memories":[{"id":"gone","type":"world"}]}`
	code, _, errOut := run(t, "forget", "o1", "--purge")
	if code != output.Blocked || !strings.Contains(errOut, "reading source fact gone") || len(curation) != 0 {
		t.Errorf("exit %d %q, calls %q", code, errOut, curation)
	}
}

func TestMemoryScanFindsSecretShapes(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	listAnswer["world"] = `[{"id":"f9","text":"deploy with GITHUB_TOKEN=ghp_` + strings.Repeat("a1B2c3D4e5F6", 3) + `","fact_type":"world","tags":["project:billing"]}]`
	listAnswer["observation"] = `[{"id":"o2","text":"Billing keeps PostgreSQL 16.","fact_type":"observation","tags":["project:billing"]},
		{"id":"o3","text":"login with --password Sup3rS3cret9","fact_type":"observation","tags":["project:billing"]}]`
	listAnswer["experience"] = `[{"id":"e1","text":"curl -u admin:` + "s3cret" + `pass9 https://x","fact_type":"experience","tags":["project:billing"]}]` // assembled: not a literal secret for gitleaks
	listAnswer["invalidated:world"] = `[{"id":"z1","text":"PGPASSWORD=Zz9yX8wV7u","fact_type":"world","tags":["project:billing"]}]`
	code, out, _ := run(t, "memory", "scan")
	if code != output.OK || !strings.Contains(out, "f9") || !strings.Contains(out, "backsight forget f9 --purge") || strings.Contains(out, "o2") ||
		!strings.Contains(out, "o3") || !strings.Contains(out, "e1") || !strings.Contains(out, "z1 (world, forgotten") || strings.Contains(out, "Zz9yX8wV7u") || strings.Contains(out, "a1B2c3D4e5F6") || strings.Contains(out, "Sup3rS3cret9") {
		t.Errorf("exit %d\n%s", code, out)
	}
	listAnswer["world"], listAnswer["observation"], listAnswer["experience"], listAnswer["invalidated:world"] = "", "", "", ""
	if _, out, _ := run(t, "memory", "scan"); !strings.Contains(out, "no secret") {
		t.Errorf("clean bank:\n%s", out)
	}
}

// memory scan uses the redaction rules themselves: the forms added by the
// M5 review rules are found in stored memories too.
func TestMemoryScanFindsTheM5RedactionForms(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	pw := "hunter" + "xyz" // assembled: not a literal secret for gitleaks
	listAnswer["world"] = `[{"id":"s1","text":"ssh with sshpass -p ` + pw + ` ssh nas","fact_type":"world","tags":["project:infra"]},
		{"id":"s2","text":"redis at redis://:` + pw + `@cache:6379/0","fact_type":"world","tags":["project:infra"]},
		{"id":"s3","text":"the NAS share needs smbclient -U bob%` + pw + `","fact_type":"world","tags":["project:infra"]},
		{"id":"s4","text":"set pw=` + pw + ` on the box","fact_type":"world","tags":["project:infra"]},
		{"id":"s5","text":"the pw field is required and api key rotation is planned","fact_type":"world","tags":["project:infra"]}]`
	t.Cleanup(func() { listAnswer["world"] = "" })
	code, out, _ := run(t, "memory", "scan")
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		if !strings.Contains(out, "backsight forget "+id+" --purge") {
			t.Errorf("scan misses %s:\n%s", id, out)
		}
	}
	if code != output.OK || strings.Contains(out, pw) || strings.Contains(out, "s5 (") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestAskReflectsOnTheProjectAndShowsTokens(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	t.Setenv("BACKSIGHT_PROJECT", "billing")
	code, out, _ := run(t, "ask", "Which", "PostgreSQL", "version", "with", "GITHUB_TOKEN=ghp_"+strings.Repeat("a1B2c3D4e5F6", 3), "?")
	if code != output.OK || !strings.Contains(out, "Billing keeps PostgreSQL 16.") || !strings.Contains(out, "1200 input, 40 output") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	body := strings.Join(recallTags, "|")
	if !strings.Contains(body, `"project:billing"`) || !strings.Contains(body, `"project:.claude"`) || strings.Contains(body, "a1B2c3D4e5F6") {
		t.Errorf("reflect body (scope + redaction): %q", body)
	}
	recallTags = nil
	run(t, "ask", "--all", "anything?")
	if strings.Contains(strings.Join(recallTags, "|"), "tags") {
		t.Errorf("--all must not scope: %q", recallTags)
	}
}

func TestProjectsListsTagsWithLastActivity(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	for _, p := range []string{"billing", "infra", "notes"} {
		projectEntities[p] = "[]"
	}
	tagCounts["billing"], tagCounts["infra"] = 12, 340
	recentByTag["project:billing"] = `[{"id":"x","text":"t","fact_type":"world","updated_at":"2026-10-20T09:00:00Z"}]`
	recentByTag["project:infra"] = `[{"id":"y","text":"t","fact_type":"world","updated_at":"2026-10-24T09:00:00Z"}]`
	code, out, _ := run(t, "projects")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != output.OK || len(lines) != 3 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// most recent first, the project with no activity last
	for i, want := range [][]string{{"infra", "340", "2026-10-24"}, {"billing", "12", "2026-10-20"}, {"notes", "1", "-"}} {
		if f := strings.Fields(lines[i]); len(f) != 3 || f[0] != want[0] || f[1] != want[1] || f[2] != want[2] {
			t.Errorf("line %d: %q, want %q", i, lines[i], want)
		}
	}
}

// A briefing page may quote the purged text: the purge deletes the page of
// every project involved (the next session rewrites it); memory scan reads
// the pages too.
func TestPurgeDeletesTheBriefingPagesAndScanReadsThem(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["f9"] = fact("f9", "conversation:s9")
	documentUnits["conversation:s9"] = 2
	page := briefingModelID("billing")
	mentalModels[page] = `{"id":"` + page + `","content":"In force: PGPASSWORD=Zz9yX8wV7u","tags":["project:billing"]}`
	mentalModels["mine"] = `{"id":"mine","content":"PGPASSWORD=Zz9yX8wV7u","tags":["project:billing"]}`
	listAnswer["world"] = ""
	_, out, _ := run(t, "memory", "scan")
	if !strings.Contains(out, page+" (briefing page") || strings.Contains(out, "Zz9yX8wV7u") || strings.Contains(out, "mine (") {
		t.Errorf("scan:\n%s", out)
	}
	code, out, _ := run(t, "forget", "f9", "--purge", "--yes")
	if _, kept := mentalModels[page]; code != output.OK || kept || !strings.Contains(out, "briefing page "+page) {
		t.Errorf("exit %d, page kept %v\n%s", code, kept, out)
	}
	if _, ok := mentalModels["mine"]; !ok {
		t.Error("a mental model backsight did not create was deleted")
	}
}

// A purge cut short says which documents are left and how to finish: the
// observation itself may be gone already.
func TestAPartialPurgeSaysWhatIsLeft(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["o1"] = `{"id":"o1","text":"obs","type":"observation","source_memories":[{"id":"f1","type":"world"},{"id":"f2","type":"world"}]}`
	memories["f1"], memories["f2"] = fact("f1", "conversation:s1"), fact("f2", "conversation:s2")
	documentUnits["conversation:s1"], documentUnits["conversation:s2"] = 1, 1
	docDeleteFails.Store(true)
	code, _, errOut := run(t, "forget", "o1", "--purge", "--yes")
	if code != output.Blocked || !strings.Contains(errOut, "documents left: conversation:s1, conversation:s2") || !strings.Contains(errOut, "backsight forget <fact id> --purge") {
		t.Errorf("exit %d %q", code, errOut)
	}
}

// The purge's writes hold the maintenance lock, so a backup cannot dump a
// half-purged database; a backup that dumped before and is still being
// encrypted is named with the finished ones.
func TestPurgeHoldsTheMaintenanceLockAndNamesABackupInProgress(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	var mu sync.Mutex
	var writes, locked int
	pointAt(t, home, proxyHindsight(t, fakeHindsight(t), func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes++
			if h := state.Inspect(d.MaintenanceLock()); h != nil && !h.Stale && h.Owner.Command == "forget --purge" {
				locked++
			}
			mu.Unlock()
		}
		return false
	}))
	withClock(t, time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC))
	backups := filepath.Join(d.Data, "backups")
	_ = os.MkdirAll(backups, 0o700)
	inProgress := filepath.Join(backups, "backsight-20261025T080000Z.tar.gz.age")
	_ = os.WriteFile(inProgress+".partial", []byte("age-encryption.org/v1"), 0o600)
	memories["f9"] = fact("f9", "conversation:s9")
	documentUnits["conversation:s9"] = 1
	code, out, errOut := run(t, "forget", "f9", "--purge", "--yes")
	if code != output.OK || !strings.Contains(out, "- "+inProgress+"\n") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	mu.Lock()
	// observations cleared, fact retired, document and briefing page
	// deleted, then the control bank set up and the tombstones written
	if writes != 7 || locked != writes {
		t.Errorf("%d of %d writes under the maintenance lock", locked, writes)
	}
	mu.Unlock()
	if _, err := os.Stat(d.MaintenanceLock()); err == nil {
		t.Error("the maintenance lock was left behind")
	}
	// a backup holding the lock makes the purge wait, before any write
	_ = os.MkdirAll(filepath.Dir(d.MaintenanceLock()), 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"command":"backup create","since":"`+time.Now().UTC().Format(time.RFC3339)+`"}`), 0o600)
	curation = nil
	if code, _, errOut := run(t, "forget", "f9", "--purge", "--yes"); code != output.Blocked || !strings.Contains(errOut, "backup create") || len(curation) != 0 {
		t.Errorf("exit %d, calls %q: %s", code, curation, errOut)
	}
}

// forget --purge deletes nothing and writes no tombstone, here or on the
// server, while another server answers at the joined address.
func TestForgetPurgeRefusesAnotherServer(t *testing.T) {
	home := devInstall(t, true)
	var mu sync.Mutex
	var writes []string
	pointAt(t, home, proxyHindsight(t, fakeHindsight(t), func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		return false
	}))
	memories["f9"] = fact("f9", "conversation:s9")
	documentUnits["conversation:s9"] = 1
	recordMismatch(t, home)
	code, out, errOut := run(t, "forget", "f9", "--purge", "--yes")
	if code != output.Blocked || !strings.Contains(out+errOut, "a different server answers at this address") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	mu.Lock()
	defer mu.Unlock()
	if st, _ := state.Load(pathsOf(home).StateFile()); len(writes) != 0 || len(st.Tombstones) != 0 {
		t.Errorf("writes %v, tombstones %+v", writes, st.Tombstones)
	}
}

// forgottenIDs is the ids of the journal's forget events, one string per
// event.
func forgottenIDs(t *testing.T, home string) []string {
	t.Helper()
	events, _, err := feedback.Read(pathsOf(home).FeedbackDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Kind == "forget" {
			out = append(out, strings.Join(e.IDs, ","))
		}
	}
	return out
}

// A forget journals the memories it retired, so the ledger drops them; a
// failed forget or a restore journals no forget.
func TestForgetJournalsTheRetiredMemories(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["f1"] = fact("f1", "conversation:s1")
	memories["o1"] = `{"id":"o1","text":"obs","type":"observation","tags":["project:billing"],"source_memories":[{"id":"f2","type":"world"},{"id":"f3","type":"world"}]}`
	memories["f2"], memories["f3"] = fact("f2", ""), fact("f3", "")
	memories["f9"] = fact("f9", "conversation:s9")
	documentUnits["conversation:s9"] = 1
	run(t, "forget", "f1")
	run(t, "forget", "--restore", "f1")
	run(t, "forget", "o1")
	run(t, "forget", "nope")
	run(t, "forget", "f9", "--purge")
	run(t, "forget", "f9", "--purge", "--yes")
	if got := forgottenIDs(t, home); strings.Join(got, " ") != "f1 o1,f2,f3 f9" {
		t.Errorf("forget events %q, want f1, o1 with its sources, the purged f9", got)
	}

	// With [feedback] enabled = false, nothing is journaled.
	home = devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	cfg, _ := os.ReadFile(pathsOf(home).ConfigFile())
	_ = os.WriteFile(pathsOf(home).ConfigFile(), append(cfg, "[feedback]\nenabled = false\n"...), 0o600)
	memories["f1"] = fact("f1", "conversation:s1")
	if code, _, _ := run(t, "forget", "f1"); code != output.OK {
		t.Fatalf("exit %d", code)
	}
	if got := forgottenIDs(t, home); len(got) != 0 {
		t.Errorf("disabled feedback journaled %q", got)
	}
}

// unhiddenIDs is the ids of the journal's unhide events with outcome,
// one "outcome:ids" string per event.
func unhiddenIDs(t *testing.T, home string) []string {
	t.Helper()
	events, _, err := feedback.Read(pathsOf(home).FeedbackDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Kind == feedback.KindUnhide {
			out = append(out, e.Outcome+":"+strings.Join(e.IDs, ","))
		}
	}
	return out
}

// forget --restore undoes a retirement: a retired raw fact is valid again
// and loses its verdicts, an observation's restored sources too, and a
// hidden observation is no longer marked hidden even when the server has nothing to
// restore (it was never invalidated there), hidden since the last settle
// included. A restore the server refuses journals nothing.
func TestRestoreUndoesARetirement(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dir := pathsOf(home).FeedbackDir()
	memories["f1"] = fact("f1", "conversation:s1")
	memories["o1"] = `{"id":"o1","text":"obs","type":"observation","tags":["project:billing"],"source_memories":[{"id":"f2","type":"world"},{"id":"f3","type":"world"}]}`
	memories["f2"], memories["f3"] = fact("f2", ""), fact("f3", "")
	memories["o3"] = `{"id":"o3","text":"obs","type":"observation","tags":["project:billing"],"source_memories":[]}`
	memoryStatus["GET o3"] = http.StatusInternalServerError
	memories["f9"] = fact("f9", "conversation:s9")
	memoryStatus["PATCH f9"] = http.StatusInternalServerError
	j := feedback.NewJournal(dir, feedback.Common{Session: "s1"})
	start := time.Now().Add(-time.Hour)
	for i, e := range []feedback.Event{
		{Kind: feedback.KindRetire, IDs: []string{"f1"}, Verdict: "wrong", Outcome: "invalidated", Note: "n1"},
		{Kind: feedback.KindRetire, IDs: []string{"f2"}, Verdict: "outdated", Outcome: "invalidated", Note: "n2"},
		{Kind: feedback.KindHide, IDs: []string{"o1"}, Verdict: "wrong", Note: "n3"},
		{Kind: feedback.KindHide, IDs: []string{"o3"}, Verdict: "wrong", Note: "n4"},
		{Kind: feedback.KindRetire, IDs: []string{"f9"}, Verdict: "wrong", Outcome: "invalidated", Note: "n5"},
	} {
		e.TS = start.Add(time.Duration(i) * time.Minute)
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := feedback.Settle(dir, feedback.Params{}, time.Now); err != nil {
		t.Fatal(err)
	}
	// Hidden after the last settle: the ledger file does not know it yet.
	if err := j.Append(feedback.Event{Kind: feedback.KindHide, IDs: []string{"o2"}, Verdict: "outdated", Note: "n6"}); err != nil {
		t.Fatal(err)
	}

	if code, out, errOut := run(t, "forget", "--restore", "f1"); code != output.OK || !strings.Contains(out, "f1 restored") {
		t.Fatalf("f1: exit %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut := run(t, "forget", "--restore", "o1"); code != output.OK || !strings.Contains(out, "no longer marked hidden on this machine") {
		t.Fatalf("o1: exit %d\n%s%s", code, out, errOut)
	}
	for _, id := range []string{"o2", "o3"} { // not on the server, or not readable there
		if code, out, errOut := run(t, "forget", "--restore", id); code != output.OK || !strings.Contains(out, id+" restored") || !strings.Contains(out, "no longer marked hidden on this machine") {
			t.Errorf("%s: exit %d\n%s%s", id, code, out, errOut)
		}
	}
	if code, _, _ := run(t, "forget", "--restore", "f9"); code == output.OK {
		t.Error("f9: a refused restore succeeded")
	}
	if code, _, _ := run(t, "forget", "--restore", "nope"); code != output.Usage {
		t.Errorf("unknown id: exit %d", code)
	}
	got := strings.Join(curation, "\n")
	for _, id := range []string{"f1", "f2", "f3"} {
		if !strings.Contains(got, "memories/"+id+` {"reason":"restored by the user (backsight forget --restore)","state":"valid"}`) {
			t.Errorf("%s not restored on the server:\n%s", id, got)
		}
	}
	if want := "restored:f1 restored:o1,f2,f3 restored:o2 restored:o3"; strings.Join(unhiddenIDs(t, home), " ") != want {
		t.Errorf("unhide events %q, want %q", unhiddenIDs(t, home), want)
	}

	if _, err := feedback.Settle(dir, feedback.Params{}, func() time.Time { return time.Now().Add(time.Minute) }); err != nil {
		t.Fatal(err)
	}
	l, err := feedback.LoadLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"f1", "f2", "o1", "o2", "o3"} {
		if m := l.Memories[id]; m.Wrong != 0 || m.Hidden != nil {
			t.Errorf("%s still retired: %+v", id, m)
		}
	}
	if l.Memories["f9"].Wrong != 1 {
		t.Errorf("f9, not restored, lost its verdict: %+v", l.Memories["f9"])
	}
}

// staleID is the full id of memory n in the memory stale tests: its first
// 8 characters are n's digit repeated.
func staleID(n int) string {
	d := strconv.Itoa(n)
	return strings.Repeat(d, 8) + "-0000-4000-8000-00000000000" + d
}

// writeStaleLedger writes a ledger whose watermark is now, with one memory
// of each kind memory stale lists or leaves out, and journals a retire
// after the watermark that only feedback.Current knows. It returns the
// ledger file's bytes.
func writeStaleLedger(t *testing.T, home string) []byte {
	t.Helper()
	dir := pathsOf(home).FeedbackDir()
	now := time.Now().UTC()
	day := 24 * time.Hour
	l := feedback.Ledger{Schema: feedback.LedgerSchema, Watermark: now, Memories: map[string]feedback.Entry{
		staleID(1): {Shown: 12, LastShown: now.Add(-3 * day)},                                                                              // never used
		staleID(2): {Shown: 15, LastShown: now.Add(-1 * day)},                                                                              // never used, shown most
		staleID(3): {Shown: 9, LastShown: now.Add(-2 * day)},                                                                               // shown too few times
		staleID(4): {Shown: 20, LastShown: now.Add(-2 * day), Credits: 0.4, CreditsAt: now.Add(-9 * day), LastCredited: now.Add(-9 * day)}, // credited
		staleID(5): {Shown: 3, LastShown: now.Add(-20 * day), Hidden: &feedback.Hidden{Since: now.Add(-8 * day), Verdict: "wrong", Note: "n5"}},
		staleID(6): {Shown: 30, LastShown: now.Add(-1 * day), Hidden: &feedback.Hidden{Since: now.Add(-2 * day), Verdict: "outdated"}}, // waits for a rewrite
		staleID(7): {Shown: 11, LastShown: now.Add(-5 * day), Wrong: 1},                                                                // reported, not never used
		staleID(8): {Shown: 2, LastShown: now.Add(-4 * day)},                                                                           // reported after the watermark
		staleID(9): {Shown: 1, Wrong: 2, Hidden: &feedback.Hidden{Since: now.Add(-10 * day)}},                                          // hidden first
	}}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	j := feedback.NewJournal(dir, feedback.Common{Session: "s1"})
	if err := j.Append(feedback.Event{Kind: feedback.KindRetire, TS: now.Add(time.Minute), IDs: []string{staleID(8)}, Verdict: "wrong", Outcome: "invalidated", Note: "n8"}); err != nil {
		t.Fatal(err)
	}
	return raw
}

type staleOut struct {
	Data struct {
		NeverUsed []staleMemory `json:"never_used"`
		Hidden    []staleMemory `json:"hidden"`
		Reported  []staleMemory `json:"reported"`
	} `json:"data"`
}

func staleIDs(ms []staleMemory) string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID[:8]+"|"+m.Command)
	}
	return strings.Join(out, " ")
}

// memory stale lists, once each, what was shown often and never used,
// what stayed hidden 7 days without a rewrite and what was reported wrong
// or outdated, each with the command that moves it on; a credited memory,
// one shown fewer than stale_shows times and one hidden for less than 7
// days are not listed.
func TestMemoryStaleListsTheThreeKinds(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	writeStaleLedger(t, home)
	code, out, errOut := run(t, "memory", "stale")
	want := "Shown often, never used\n" +
		"22222222  shown 15  1d ago  backsight forget " + staleID(2) + "\n" +
		"11111111  shown 12  3d ago  backsight forget " + staleID(1) + "\n" +
		"\nHidden, not rewritten for 7 days\n" +
		"55555555  shown 3  2w ago  backsight forget --restore " + staleID(5) + "\n" +
		"99999999  shown 1  -  backsight forget --restore " + staleID(9) + "\n" +
		"Hidden on this machine, so recall here skips them, until the server rewrites them; `backsight forget <id>` retires one for good once the server lists its source facts.\n" +
		"\nReported wrong or outdated\n" +
		"77777777  shown 11  5d ago  backsight forget " + staleID(7) + " --purge\n" +
		"88888888  shown 2  4d ago  backsight forget " + staleID(8) + " --purge\n" +
		"\nbacksight forget --restore <id> undoes a retirement; --purge alone only lists what deleting for good would remove, and adding --yes deletes it.\n"
	if code != output.OK || out != want {
		t.Fatalf("exit %d\n%s%s\nwant\n%s", code, out, errOut, want)
	}

	code, out, _ = run(t, "memory", "stale", "--json")
	var s staleOut
	if err := json.Unmarshal([]byte(out), &s); err != nil || code != output.OK {
		t.Fatalf("exit %d %v\n%s", code, err, out)
	}
	if got, want := staleIDs(s.Data.NeverUsed), "22222222|backsight forget "+staleID(2)+" 11111111|backsight forget "+staleID(1); got != want {
		t.Errorf("never_used %q, want %q", got, want)
	}
	if got, want := staleIDs(s.Data.Hidden), "55555555|backsight forget --restore "+staleID(5)+" 99999999|backsight forget --restore "+staleID(9); got != want {
		t.Errorf("hidden %q, want %q", got, want)
	}
	if got, want := staleIDs(s.Data.Reported), "77777777|backsight forget "+staleID(7)+" --purge 88888888|backsight forget "+staleID(8)+" --purge"; got != want {
		t.Errorf("reported %q, want %q", got, want)
	}
	if h := s.Data.Hidden[0]; h.Shown != 3 || h.HiddenSince.IsZero() || s.Data.Reported[0].Wrong != 1 || s.Data.NeverUsed[0].LastShown.IsZero() {
		t.Errorf("fields %+v %+v %+v", h, s.Data.Reported[0], s.Data.NeverUsed[0])
	}

	// stale_shows moves the bar
	cfgPath := pathsOf(home).ConfigFile()
	raw, _ := os.ReadFile(cfgPath)
	_ = os.WriteFile(cfgPath, append(raw, []byte("\n[feedback]\nstale_shows = 9\n")...), 0o600)
	if _, out, _ := run(t, "memory", "stale"); !strings.Contains(out, "33333333  shown 9  2d ago  backsight forget "+staleID(3)+"\n") {
		t.Errorf("stale_shows = 9 leaves out the memory shown 9 times:\n%s", out)
	}

	// a reported fact restored is no longer listed as reported: shown
	// often and never used, it is listed with a plain forget
	memories[staleID(7)] = fact(staleID(7), "conversation:s1")
	if code, out, errOut := run(t, "forget", "--restore", staleID(7)); code != output.OK {
		t.Fatalf("restore: exit %d\n%s%s", code, out, errOut)
	}
	if _, out, _ := run(t, "memory", "stale"); strings.Contains(out, staleID(7)+" --purge") || !strings.Contains(out, "77777777  shown 11  5d ago  backsight forget "+staleID(7)+"\n") {
		t.Errorf("a restored fact is still listed as reported:\n%s", out)
	}
	// a hidden memory restored as its line says leaves the list
	if code, out, errOut := run(t, "forget", "--restore", staleID(5)); code != output.OK {
		t.Fatalf("restore of a hidden memory: exit %d\n%s%s", code, out, errOut)
	}
	if _, out, _ := run(t, "memory", "stale"); strings.Contains(out, "55555555") {
		t.Errorf("a restored hidden memory is still listed:\n%s", out)
	}

	// nothing to review: one line
	empty := devInstall(t, true)
	pointAt(t, empty, fakeHindsight(t))
	if code, out, _ := run(t, "memory", "stale"); code != output.OK || out != "Nothing needs review: no memory shown often and never used, hidden for 7 days, or reported wrong or outdated.\n" {
		t.Errorf("empty: exit %d\n%s", code, out)
	}
	if code, out, _ := run(t, "memory", "stale", "--json"); code != output.OK || !strings.Contains(out, `"never_used": []`) || !strings.Contains(out, `"reported": []`) {
		t.Errorf("empty json: exit %d\n%s", code, out)
	}
}

// memory stale writes no ledger and journals one stale event naming every
// memory it listed; a journal it cannot write changes nothing it prints.
func TestMemoryStaleIsReadOnlyButJournals(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	before := writeStaleLedger(t, home)
	dir := pathsOf(home).FeedbackDir()
	code, out, _ := run(t, "memory", "stale")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "ledger.json")); string(after) != string(before) {
		t.Errorf("the ledger was written:\n%s\nwas\n%s", after, before)
	}
	stale := eventsOf(journalEvents(t, home), "stale")
	want := strings.Join([]string{staleID(2), staleID(1), staleID(5), staleID(9), staleID(7), staleID(8)}, ",")
	if len(stale) != 1 || strings.Join(stale[0].IDs, ",") != want {
		t.Fatalf("stale events %+v, want one with %s", stale, want)
	}

	// an unwritable journal: the same output, still a success
	files, _ := filepath.Glob(filepath.Join(dir, "journal-*.jsonl"))
	for _, f := range files {
		_ = os.Chmod(f, 0o400)
		t.Cleanup(func() { _ = os.Chmod(f, 0o600) })
	}
	if code, again, errOut := run(t, "memory", "stale"); code != output.OK || again != out || errOut != "" {
		t.Errorf("unwritable journal: exit %d\n%s\nstderr %q", code, again, errOut)
	}
	if n := len(eventsOf(journalEvents(t, home), "stale")); n != 1 {
		t.Errorf("%d stale events, the journal was not read-only", n)
	}
}

// A hiding is listed from exactly 7 days on; within a section, memories
// shown as often are in id order; with no reported fact, the footer says
// nothing of --purge.
func TestMemoryStaleBoundaryAndOrder(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	l := feedback.Ledger{Memories: map[string]feedback.Entry{
		"h-exact": {Shown: 1, Hidden: &feedback.Hidden{Since: at.Add(-staleHidden)}},
		"h-short": {Shown: 1, Hidden: &feedback.Hidden{Since: at.Add(-staleHidden + time.Second)}},
		"n-b":     {Shown: 10},
		"n-a":     {Shown: 10},
		"n-c":     {Shown: 11},
	}}
	r := staleOf(l, 10, at)
	if got := strings.Join(r.ids(), " "); got != "n-c n-a n-b h-exact" {
		t.Errorf("listed %q, want %q", got, "n-c n-a n-b h-exact")
	}
	if text := staleText(r, at); !strings.HasSuffix(text, "\nbacksight forget --restore <id> undoes a retirement.\n") {
		t.Errorf("footer without a reported fact:\n%s", text)
	}
}

// Nothing listed, or feedback off: no stale event, and the list is still
// shown with feedback off.
func TestMemoryStaleJournalsNothingWhenEmptyOrOff(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	if code, out, _ := run(t, "memory", "stale"); code != output.OK || !strings.HasPrefix(out, "Nothing needs review") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if n := len(eventsOf(journalEvents(t, home), "stale")); n != 0 {
		t.Errorf("%d stale events for an empty list", n)
	}
	writeStaleLedger(t, home)
	cfgPath := pathsOf(home).ConfigFile()
	raw, _ := os.ReadFile(cfgPath)
	_ = os.WriteFile(cfgPath, append(raw, []byte("\n[feedback]\nenabled = false\n")...), 0o600)
	if code, out, _ := run(t, "memory", "stale"); code != output.OK || !strings.Contains(out, "22222222  shown 15") {
		t.Fatalf("feedback off: exit %d\n%s", code, out)
	}
	if n := len(eventsOf(journalEvents(t, home), "stale")); n != 0 {
		t.Errorf("%d stale events with feedback off", n)
	}
}

// "Shown often, never used" counts the shows recorded live: an import
// never counts a miss, so imported shows alone say nothing of use.
func TestNeverUsedCountsLiveShowsOnly(t *testing.T) {
	now := time.Now().UTC()
	l := feedback.Ledger{Schema: feedback.LedgerSchema, Memories: map[string]feedback.Entry{
		staleID(1): {Shown: 20, LastShown: now, Imported: feedback.Imported{Shown: 20}}, // imported only
		staleID(2): {Shown: 20, LastShown: now},                                         // live only
		staleID(3): {Shown: 25, LastShown: now, Imported: feedback.Imported{Shown: 20}}, // 5 live
		staleID(4): {Shown: 30, LastShown: now, Imported: feedback.Imported{Shown: 15}}, // 15 live
	}}
	res := staleOf(l, 10, now)
	var got []string
	for _, m := range res.NeverUsed {
		got = append(got, m.ID[:8])
	}
	if strings.Join(got, " ") != "44444444 22222222" {
		t.Errorf("never used %v, want the memories with 10 live shows or more: 44444444 22222222", got)
	}
}

// forget --restore of a memory only another machine hid: never retired
// on the server, which is left alone, it is restored on this machine (an
// unhide journaled after that hiding) and recall here no longer skips it.
func TestRestoreOfAMemoryHiddenOnAnotherMachine(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	hiddenAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeSharedCache(t, home, otherMachine("pc-b", map[string]feedback.SharedEntry{uA: {HiddenSince: hiddenAt}}))
	cfg, _, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	hiddenHereNow := func() bool {
		adjust := ledgerAdjust(pathsOf(home), cfg, time.Now())
		if adjust == nil {
			return false
		}
		_, hidden := adjust(uA)
		return hidden
	}
	if !hiddenHereNow() {
		t.Fatal("the other machine's hiding does not apply here")
	}
	code, out, errOut := run(t, "forget", "--restore", uA)
	if code != output.OK || !strings.Contains(out, uA+" restored") || !strings.Contains(out, "visible again on this machine") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if len(curation) != 0 {
		t.Errorf("the server was written: %v", curation)
	}
	if want := "restored:" + uA; strings.Join(unhiddenIDs(t, home), " ") != want {
		t.Errorf("unhide events %q, want %q", unhiddenIDs(t, home), want)
	}
	l, err := feedback.Current(pathsOf(home).FeedbackDir())
	if err != nil || !l.Memories[uA].UnhiddenAt.After(hiddenAt) {
		t.Errorf("restore time %v (%v), hiding %v", l.Memories[uA].UnhiddenAt, err, hiddenAt)
	}
	if hiddenHereNow() {
		t.Error("still hidden here after the restore")
	}
	// Restored for good: a second restore finds it hidden nowhere, and
	// the server does not hold it.
	if code, out, errOut := run(t, "forget", "--restore", uA); code != output.Usage {
		t.Errorf("second restore: exit %d\n%s%s", code, out, errOut)
	}
	// Hidden nowhere, and not on the server: nothing to restore.
	if code, _, _ := run(t, "forget", "--restore", uB); code != output.Usage {
		t.Errorf("%s: exit %d", uB, code)
	}
}

// A connect while forget runs (after it read its config): forget,
// --restore and --purge write nothing to the server it loaded, and no
// tombstone is recorded for the server this PC joined.
func TestForgetWritesNothingAfterAConnect(t *testing.T) {
	for _, args := range [][]string{{"forget", "f9"}, {"forget", "--restore", "f9"}, {"forget", "f9", "--purge", "--yes"}} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			home := devInstall(t, true)
			var mu sync.Mutex
			var writes []string
			pointAt(t, home, proxyHindsight(t, fakeHindsight(t), func(_ http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodGet {
					mu.Lock()
					writes = append(writes, r.Method+" "+r.URL.Path)
					mu.Unlock()
				}
				return false
			}))
			memories["f9"] = fact("f9", "conversation:s9")
			documentUnits["conversation:s9"] = 1
			connectAfterLoad(t, home)
			code, out, errOut := run(t, args...)
			if code != output.Blocked || !strings.Contains(out+errOut, "joined another server") {
				t.Errorf("exit %d\n%s%s", code, out, errOut)
			}
			mu.Lock()
			defer mu.Unlock()
			if st, _ := state.Load(pathsOf(home).StateFile()); len(writes) != 0 || len(st.Tombstones) != 0 {
				t.Errorf("writes %v, tombstones %+v", writes, st.Tombstones)
			}
		})
	}
}
