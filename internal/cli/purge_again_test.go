package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
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

// recreatedServer fronts the fake server with conversation:s9, purged on
// this machine and recreated since by an older client's append, holding
// the fact f9 extracted from that append.
type recreatedServer struct {
	mu      sync.Mutex
	exists  bool
	deleted int
	// listStatus, deleteStatus: non-zero, the document listing or the
	// document's delete answers with it.
	listStatus, deleteStatus int
	// hang: the document listing answers only once the client gave up.
	hang   bool
	writes []string // every request but a GET
	// order: "send" for each retain into the memory bank, "look" for each
	// listing of new documents, in arrival order.
	order []string
}

func withRecreated(t *testing.T, home string) *recreatedServer {
	t.Helper()
	rs := &recreatedServer{exists: true}
	port := fakeHindsight(t)
	memories["f9"] = fact("f9", "conversation:s9")
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		if r.Method != http.MethodGet {
			rs.writes = append(rs.writes, r.Method+" "+r.URL.Path)
		}
		path := r.URL.Path
		if r.Method == http.MethodPost && path == "/v1/default/banks/claude-memory/memories" {
			rs.order = append(rs.order, "send")
		}
		switch {
		case r.Method == http.MethodGet && path == "/v1/default/banks/claude-memory/documents" && r.URL.Query().Get("time_field") == "created_at" && !r.URL.Query().Has("tags"):
			rs.order = append(rs.order, "look")
			if rs.listStatus != 0 {
				w.WriteHeader(rs.listStatus)
				return true
			}
			if rs.hang {
				rs.mu.Unlock()
				<-r.Context().Done()
				rs.mu.Lock()
				return true
			}
			items := `[]`
			if rs.exists {
				items = `[{"id":"conversation:s9","created_at":"2026-10-26T09:00:00.123456+00:00"}]`
			}
			_, _ = w.Write([]byte(`{"items":` + items + `,"total":` + strconv.Itoa(strings.Count(items, "id")) + `}`))
		case strings.HasSuffix(path, "/claude-memory/documents/conversation:s9"):
			switch {
			case !rs.exists:
				w.WriteHeader(http.StatusNotFound)
			case r.Method == http.MethodDelete && rs.deleteStatus != 0:
				w.WriteHeader(rs.deleteStatus)
			case r.Method == http.MethodDelete:
				rs.exists = false
				rs.deleted++
				_, _ = w.Write([]byte(`{}`))
			default:
				_, _ = w.Write([]byte(`{"id":"conversation:s9","memory_unit_count":1,"document_metadata":{"session_id":"s9"}}`))
			}
		case strings.HasSuffix(path, "/claude-memory/memories/list") && r.URL.Query().Get("document_id") == "conversation:s9":
			items := `[]`
			if rs.exists && r.URL.Query().Get("offset") == "0" {
				items = `[{"id":"f9","fact_type":"world","tags":["project:billing"],"document_id":"conversation:s9"}]`
			}
			_, _ = w.Write([]byte(`{"items":` + items + `,"total":` + strconv.Itoa(strings.Count(items, `"id"`)) + `}`))
		default:
			return false
		}
		return true
	}))
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.AddTombstones(state.Tombstone{Bank: "claude-memory", Document: "conversation:s9", Session: "s9", Project: "billing", LastUUID: "u7",
			PurgedAt: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rs
}

func (rs *recreatedServer) state() (bool, int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.exists, rs.deleted
}

func (rs *recreatedServer) set(fn func()) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	fn()
}

// configWith appends lines to the config (after withRecreated, which
// rewrites it).
func configWith(t *testing.T, home, toml string) {
	t.Helper()
	cfgPath := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfgPath)
	if err := os.WriteFile(cfgPath, append(raw, []byte(toml)...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// jobs tick purges again a purged document an older client recreated: its
// fact's observations cleared, the fact retired, the document deleted;
// the report says so. The next tick does nothing.
func TestJobsTickPurgesARecreatedPurgedDocument(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	code, out, errOut := run(t, "jobs", "tick", "--json")
	var env tickEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if exists, _ := rs.state(); code != output.OK || exists || !strings.Contains(env.Data.Purges, "conversation:s9") {
		t.Fatalf("exit %d, purges %q\n%s\n%s", code, env.Data.Purges, out, errOut)
	}
	got := strings.Join(curation, "\n")
	if !strings.Contains(got, "DELETE /v1/default/banks/claude-memory/memories/f9/observations") || !strings.Contains(got, "PATCH /v1/default/banks/claude-memory/memories/f9") {
		t.Errorf("the fact was not purged: %s", got)
	}
	code, out, _ = run(t, "jobs", "tick", "--json")
	env = tickEnvelope{}
	_ = json.Unmarshal([]byte(out), &env)
	if _, deleted := rs.state(); code != output.OK || deleted != 1 || env.Data.Purges != "" {
		t.Errorf("second tick: exit %d, %d deletes, purges %q", code, deleted, env.Data.Purges)
	}
}

// The look runs after the sends: a server whose listing fails still gets
// the tick's sessions, and the report says the look did not run.
func TestJobsTickSendsWhenTheLookFails(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	rs.set(func() { rs.listStatus = http.StatusInternalServerError })
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // retain_since 08:00
	writeTranscript(t, home, "live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	before := retained.Load()
	rs.set(func() { rs.order = nil })
	code, out, errOut := run(t, "jobs", "tick", "--json")
	var env tickEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || retained.Load() != before+1 || !strings.HasPrefix(env.Data.Purges, "not looked at:") {
		t.Fatalf("exit %d, %d retained, purges %q\n%s\n%s", code, retained.Load()-before, env.Data.Purges, out, errOut)
	}
	rs.mu.Lock()
	order := strings.Join(rs.order, ",")
	rs.mu.Unlock()
	if !strings.HasPrefix(order, "send") || !strings.Contains(order, "look") {
		t.Errorf("the look ran before the sends: %s", order)
	}
	if exists, _ := rs.state(); !exists {
		t.Error("purged without a look")
	}
}

// A server that does not answer the look holds the tick for its budget
// only, after the sends.
func TestJobsTickBoundsTheLook(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	rs.set(func() { rs.hang = true })
	old := guardTickBudget
	guardTickBudget = 300 * time.Millisecond
	t.Cleanup(func() { guardTickBudget = old })
	start := time.Now()
	code, out, errOut := run(t, "jobs", "tick", "--json")
	var env tickEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || !strings.HasPrefix(env.Data.Purges, "not looked at:") || time.Since(start) > 10*time.Second {
		t.Errorf("exit %d after %v, purges %q\n%s", code, time.Since(start), env.Data.Purges, errOut)
	}
}

// jobs maintain looks at every tombstone, and purges what it finds.
func TestJobsMaintainLooksAtEveryTombstone(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	configWith(t, home, "\n[backup]\nenabled = false\n")
	code, out, errOut := run(t, "jobs", "maintain")
	if exists, _ := rs.state(); code != output.OK || exists || !strings.Contains(out, "1 recreated purged document(s) purged again (conversation:s9)") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.PurgeGuard == nil || st.PurgeGuard.FullAt.IsZero() {
		t.Errorf("no full look recorded: %+v", st.PurgeGuard)
	}
}

// With the claude-code provider, nothing is purged unattended (the server
// consolidates again on the user's subscription): the tick records it for
// doctor; retain purge-again asks for --allow-subscription, --plan lists
// it and writes nothing, and with the consent it goes. A second run does
// nothing.
func TestClaudeCodeLeavesTheRecreatedDocumentToTheUser(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	configWith(t, home, "[llm]\nprovider = \"claude-code\"\n")
	if code, out, errOut := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "waiting") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if exists, _ := rs.state(); !exists || st.PurgeGuard == nil || len(st.PurgeGuard.Waiting) != 1 || !st.PurgeGuard.Waiting[0].NeedsUser || !strings.Contains(st.PurgeGuard.Waiting[0].Why, "--allow-subscription") {
		t.Fatalf("purged unattended, or not recorded: %+v", st.PurgeGuard)
	}
	code, out, errOut := run(t, "retain", "purge-again", "--json")
	if code != output.NeedsUser || !strings.Contains(out+errOut, "--allow-subscription") || !strings.Contains(out, `"action": "needs_consent"`) {
		t.Errorf("no consent: exit %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ := run(t, "retain", "purge-again", "--plan"); code != output.OK || !strings.Contains(out, "conversation:s9 (session s9)") {
		t.Errorf("--plan: exit %d\n%s", code, out)
	}
	if exists, _ := rs.state(); !exists {
		t.Fatal("deleted without consent")
	}
	if code, out, errOut := run(t, "retain", "purge-again", "--allow-subscription"); code != output.OK || !strings.Contains(out, "purged again: 1 document(s): conversation:s9") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); len(st.PurgeGuard.Waiting) != 0 {
		t.Errorf("still waiting: %+v", st.PurgeGuard)
	}
	if code, out, _ := run(t, "retain", "purge-again", "--allow-subscription"); code != output.OK || !strings.Contains(out, "no purged document") {
		t.Errorf("second run: exit %d\n%s", code, out)
	}
	if _, deleted := rs.state(); deleted != 1 {
		t.Errorf("%d deletes", deleted)
	}
}

// On a server this machine does not run, its owner's provider does the
// consolidation: whatever this machine's provider, nothing is purged
// unattended, and purge-again needs the consent.
func TestRemoteServerLeavesTheRecreatedDocumentToTheUser(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	cfgPath := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfgPath)
	_ = os.WriteFile(cfgPath, bytes.Replace(raw, []byte("[server]\n"), []byte("[server]\nmode = \"remote\"\n"), 1), 0o600)
	if code, out, errOut := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "waiting") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if exists, _ := rs.state(); !exists {
		t.Fatal("purged unattended on a remote server")
	}
	if code, out, errOut := run(t, "retain", "purge-again"); code != output.NeedsUser || !strings.Contains(out+errOut, "owner") {
		t.Errorf("no consent: exit %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := run(t, "retain", "purge-again", "--allow-subscription"); code != output.OK || !strings.Contains(out, "purged again") {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// --plan changes nothing: neither state.json nor anything on the server
// (not even the control bank's tombstones).
func TestRetainPurgeAgainPlanWritesNothing(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	before, _ := os.ReadFile(pathsOf(home).StateFile())
	rs.set(func() { rs.writes = nil })
	code, out, errOut := run(t, "retain", "purge-again", "--plan", "--json")
	if code != output.OK || !strings.Contains(out, `"action": "planned"`) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	after, _ := os.ReadFile(pathsOf(home).StateFile())
	rs.mu.Lock()
	writes := rs.writes
	rs.mu.Unlock()
	if !bytes.Equal(before, after) || len(writes) != 0 {
		t.Errorf("a plan wrote: state changed %v, requests %v", !bytes.Equal(before, after), writes)
	}
	if exists, _ := rs.state(); !exists {
		t.Error("a plan deleted")
	}
}

// A delete that fails leaves the document waiting: exit 40, run it again.
// A server that cannot be read: exit 20.
func TestRetainPurgeAgainExitCodes(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	rs.set(func() { rs.deleteStatus = http.StatusInternalServerError })
	if code, out, errOut := run(t, "retain", "purge-again"); code != output.Partial || !strings.Contains(out+errOut, "conversation:s9") {
		t.Errorf("failed delete: exit %d\n%s\n%s", code, out, errOut)
	}
	rs.set(func() { rs.deleteStatus, rs.listStatus = 0, http.StatusInternalServerError })
	if code, out, errOut := run(t, "retain", "purge-again"); code != output.Blocked || !strings.Contains(out+errOut, "cannot look") {
		t.Errorf("server error: exit %d\n%s\n%s", code, out, errOut)
	}
	if exists, _ := rs.state(); !exists {
		t.Error("deleted")
	}
}

// Under the maintenance lock (a backup, an update, a restore) purge-again
// refuses and deletes nothing.
func TestRetainPurgeAgainRefusesUnderMaintenance(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	lock := pathsOf(home).MaintenanceLock()
	_ = os.MkdirAll(filepath.Dir(lock), 0o700)
	if err := os.WriteFile(lock, []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"command":"backup create","since":"`+time.Now().UTC().Format(time.RFC3339)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run(t, "retain", "purge-again"); code != output.Blocked || !strings.Contains(out+errOut, "maintenance") {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
	if exists, _ := rs.state(); !exists {
		t.Error("deleted under the maintenance lock")
	}
}

// controlWrites are the requests that wrote to the control bank.
func (rs *recreatedServer) controlWrites() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var out []string
	for _, w := range rs.writes {
		if strings.Contains(w, "-control") {
			out = append(out, w)
		}
	}
	return out
}

// While another server answers at the joined address, jobs maintain's
// look neither mirrors this PC's tombstones into its control bank nor
// deletes there; once the joined server answers, it does.
func TestJobsMaintainWritesNothingToAnotherServer(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	configWith(t, home, "\n[backup]\nenabled = false\n")
	recordMismatch(t, home)
	rs.set(func() { rs.writes = nil })
	_, out, _ := run(t, "jobs", "maintain")
	if w := rs.controlWrites(); len(w) != 0 || !strings.Contains(out, "not looked at: a different server answers at the joined address") {
		t.Fatalf("control bank writes %v\n%s", w, out)
	}
	if exists, _ := rs.state(); !exists {
		t.Error("deleted on another server")
	}
	setMismatch(t, home, "")
	run(t, "jobs", "maintain")
	if w := rs.controlWrites(); len(w) == 0 {
		t.Error("the joined server got no tombstones: the check above proves nothing")
	}
}

// retain purge-again, --plan included, refuses while another server
// answers at the joined address, before any write.
func TestRetainPurgeAgainRefusesAnotherServer(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	recordMismatch(t, home)
	rs.set(func() { rs.writes = nil })
	for _, args := range [][]string{{"--plan"}, {"--allow-subscription"}} {
		code, out, errOut := run(t, append([]string{"retain", "purge-again"}, args...)...)
		if code != output.Blocked || !strings.Contains(out+errOut, "a different server answers at this address (server b, joined a)") {
			t.Errorf("%v: exit %d\n%s\n%s", args, code, out, errOut)
		}
	}
	rs.mu.Lock()
	writes := rs.writes
	rs.mu.Unlock()
	if exists, _ := rs.state(); !exists || len(writes) != 0 {
		t.Errorf("wrote to another server: %v", writes)
	}
}
