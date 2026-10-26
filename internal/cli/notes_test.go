package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
)

// storeJSON runs store with --json and decodes its result.
func storeJSON(t *testing.T, args ...string) (int, storeResult, string) {
	t.Helper()
	code, out, errOut := run(t, append(append([]string{"store"}, args...), "--json")...)
	var env struct {
		Data storeResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	return code, env.Data, out + errOut
}

// Without --project a note goes to the current directory's project.
func TestStoreDefaultsToTheCurrentProject(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dir := filepath.Join(home, "work", "billing")
	_ = os.MkdirAll(dir, 0o700)
	t.Chdir(dir)
	code, res, out := storeJSON(t, "--kind", "decision", "Billing keeps PostgreSQL 16.")
	if code != output.OK || res.Project != "billing" || !strings.Contains(retainedTags[len(retainedTags)-1], "project:billing") {
		t.Errorf("exit %d %+v\n%s", code, res, out)
	}
}

// The home directory belongs to the transverse pool, shown in every
// project: a note goes there only when the caller names it.
func TestStoreInTheTransverseDirectoryNeedsAnExplicitProject(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	t.Chdir(home)
	before := retained.Load()
	code, _, out := storeJSON(t, "--kind", "convention", "Answer in French.")
	if code != output.Usage || !strings.Contains(out, "--project transverse") || retained.Load() != before {
		t.Errorf("implicit transverse: exit %d, %d sent\n%s", code, retained.Load()-before, out)
	}
	if n, _ := notesOutbox(pathsOf(home)).Pending(); n != 0 {
		t.Errorf("a refused note was queued: %d", n)
	}
	code, res, out := storeJSON(t, "--kind", "convention", "--project", "transverse", "Answer in French.")
	if code != output.OK || res.Project != ".claude" || !strings.Contains(retainedTags[len(retainedTags)-1], "project:.claude") {
		t.Errorf("explicit transverse: exit %d %+v\n%s", code, res, out)
	}
	if code, res, _ := storeJSON(t, "--kind", "decision", "--project", "billing", "Billing keeps PostgreSQL 16."); code != output.OK || res.Project != "billing" {
		t.Errorf("explicit project from home: exit %d %+v", code, res)
	}
}

func TestMCPStoreScopeIsTheCurrentProjectAndTransverseOnlyWhenNamed(t *testing.T) {
	mcpParentPID = 4747
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	sessions := pathsOf(home).SessionsDir()
	_ = session.WriteLive(sessions, 4747, session.Live{SessionID: "s-billing", Cwd: filepath.Join(home, "billing"), Project: "billing"})
	if out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps PostgreSQL 16."}); isErr || !strings.Contains(out, "project billing)") {
		t.Errorf("default scope: %v %s", isErr, out)
	}
	// a session in the home directory: its project is the transverse pool
	_ = session.WriteLive(sessions, 4747, session.Live{SessionID: "s-home", Cwd: home, Project: ".claude"})
	before := retained.Load()
	recallTags = nil
	out, isErr := call(t, cs, "memory_store", map[string]any{"kind": "convention", "text": "Always run the installer script first."})
	if !isErr || !strings.Contains(out, `project "transverse"`) || retained.Load() != before || len(recallTags) != 0 {
		t.Errorf("implicit transverse must be refused before anything is sent: %v %s (%d sent, %d recalls)", isErr, out, retained.Load()-before, len(recallTags))
	}
	out, isErr = call(t, cs, "memory_store", map[string]any{"kind": "convention", "text": "Answer in French.", "project": "transverse"})
	if isErr || !strings.Contains(out, "project .claude)") || !strings.Contains(retainedTags[len(retainedTags)-1], "project:.claude") {
		t.Errorf("explicit transverse: %v %s", isErr, out)
	}
}

// The tool tells Claude the default and that transverse must be named.
func TestMCPStoreSchemaDocumentsTheScope(t *testing.T) {
	cs := mcpSession(t)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "memory_store" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(tool.Description, "current project") || !strings.Contains(string(raw), `only for a user-wide rule`) {
			t.Errorf("description %q\nschema %s", tool.Description, raw)
		}
		return
	}
	t.Fatal("no memory_store tool")
}

type notesOut struct {
	Data struct {
		Since string     `json:"since"`
		Notes []noteLine `json:"notes"`
	} `json:"data"`
}

func notesJSON(t *testing.T, args ...string) (int, notesOut, string) {
	t.Helper()
	code, out, errOut := run(t, append(append([]string{"memory", "notes"}, args...), "--json")...)
	var n notesOut
	_ = json.Unmarshal([]byte(out), &n)
	return code, n, out + errOut
}

// memory notes lists the recent notes, newest first, with project,
// session, kind, time and text; not the conversations, not older notes.
func TestMemoryNotesListsRecentNotesNewestFirst(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	t.Setenv("BACKSIGHT_SESSION", "s-one")
	_, a, _ := storeJSON(t, "--kind", "decision", "--project", "billing", "Billing keeps PostgreSQL 16.")
	t.Setenv("BACKSIGHT_SESSION", "")
	_, b, _ := storeJSON(t, "--kind", "convention", "--project", "transverse", "Answer in French.")
	_, old, _ := storeJSON(t, "--kind", "lesson", "--project", "billing", "An old lesson.")
	now := time.Now().UTC()
	fakeDocCreated[a.Document] = now.Add(-2 * time.Hour)
	fakeDocCreated[b.Document] = now.Add(-1 * time.Hour)
	fakeDocCreated[old.Document] = now.AddDate(0, 0, -30)
	// a conversation document is not a note
	cfg, sec, _ := loadServerConfig()
	cl, _ := hindsight.New(cfg.Server.URL, sec.APIKey, 5*time.Second)
	if err := cl.RetainNow(t.Context(), cfg.Server.Bank, []hindsight.RetainItem{{Content: "chat", DocumentID: "conversation:s9", Tags: []string{"project:billing", "source:chat"}}}); err != nil {
		t.Fatal(err)
	}

	code, n, out := notesJSON(t)
	if code != output.OK || len(n.Data.Notes) != 2 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	first, second := n.Data.Notes[0], n.Data.Notes[1]
	if first.Document != b.Document || first.Project != ".claude" || first.Kind != "convention" || first.Text != "Answer in French." || first.Session != "" {
		t.Errorf("newest %+v", first)
	}
	if second.Document != a.Document || second.Project != "billing" || second.Session != "s-one" || !strings.Contains(second.Text, "PostgreSQL 16") {
		t.Errorf("second %+v", second)
	}
	if !second.Created.Before(first.Created) || second.Created.IsZero() {
		t.Errorf("times %v %v", first.Created, second.Created)
	}
	if _, n, _ := notesJSON(t, "--days", "60"); len(n.Data.Notes) != 3 {
		t.Errorf("--days 60: %d notes", len(n.Data.Notes))
	}
	if _, n, _ := notesJSON(t, "--limit", "1"); len(n.Data.Notes) != 1 || n.Data.Notes[0].Document != b.Document {
		t.Errorf("--limit 1: %+v", n.Data.Notes)
	}
	if _, n, _ := notesJSON(t, "--project", "transverse"); len(n.Data.Notes) != 1 || n.Data.Notes[0].Project != ".claude" {
		t.Errorf("--project transverse: %+v", n.Data.Notes)
	}
	// the text output names the session, the project and how to remove one
	_, text, _ := run(t, "memory", "notes")
	for _, want := range []string{"project billing", "session s-one", "Answer in French.", "backsight forget"} {
		if !strings.Contains(text, want) {
			t.Errorf("text output lacks %q:\n%s", want, text)
		}
	}
}

func TestMemoryNotesWithoutNotes(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	code, out, _ := run(t, "memory", "notes")
	if code != output.OK || !strings.Contains(out, "no note stored in the last 7 days") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "memory", "notes", "--limit", "0"); code != output.Usage {
		t.Errorf("--limit 0: exit %d", code)
	}
}
