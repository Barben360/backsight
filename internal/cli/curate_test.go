package cli

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

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
