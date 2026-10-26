package cli

import (
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
// the fact f9 extracted from that append; deleted lists its deletes.
type recreatedServer struct {
	mu      sync.Mutex
	exists  bool
	deleted int
}

func withRecreated(t *testing.T, home string) *recreatedServer {
	t.Helper()
	rs := &recreatedServer{exists: true}
	port := fakeHindsight(t)
	memories["f9"] = fact("f9", "conversation:s9")
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/v1/default/banks/claude-memory/documents" && r.URL.Query().Get("time_field") == "created_at":
			items := `[]`
			if rs.exists {
				items = `[{"id":"conversation:s9","created_at":"2026-10-26T09:00:00Z"}]`
			}
			_, _ = w.Write([]byte(`{"items":` + items + `,"total":` + strconv.Itoa(strings.Count(items, "id")) + `}`))
		case strings.HasSuffix(path, "/claude-memory/documents/conversation:s9"):
			switch {
			case !rs.exists:
				w.WriteHeader(http.StatusNotFound)
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
				items = `[{"id":"f9","tags":["project:billing"],"document_id":"conversation:s9"}]`
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

func useClaudeCode(t *testing.T, home string) {
	t.Helper()
	cfgPath := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfgPath)
	if err := os.WriteFile(cfgPath, append(raw, []byte("[llm]\nprovider = \"claude-code\"\n")...), 0o600); err != nil {
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

// With the claude-code provider, nothing is purged unattended (the server
// consolidates again on the user's subscription): the tick records it for
// doctor; retain purge-again asks for --allow-subscription, --plan lists
// it, and with the consent it goes. A second run does nothing.
func TestClaudeCodeLeavesTheRecreatedDocumentToTheUser(t *testing.T) {
	home := devInstall(t, true)
	rs := withRecreated(t, home)
	useClaudeCode(t, home) // after: withRecreated rewrites the config
	if code, out, errOut := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "waiting") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if exists, _ := rs.state(); !exists || st.PurgeGuard == nil || len(st.PurgeGuard.Waiting) != 1 || !strings.Contains(st.PurgeGuard.Waiting[0].Why, "--allow-subscription") {
		t.Fatalf("purged unattended, or not recorded: %+v", st.PurgeGuard)
	}
	if code, out, errOut := run(t, "retain", "purge-again"); code != output.NeedsUser || !strings.Contains(out+errOut, "--allow-subscription") {
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
