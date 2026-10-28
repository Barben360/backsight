package cli

import (
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
