package cli

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

func TestForgetIsReversible(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	if code, out, _ := run(t, "forget", "m1"); code != output.OK || !strings.Contains(out, "--restore") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	run(t, "forget", "--restore", "m1")
	got := strings.Join(curation, "\n")
	if !strings.Contains(got, `PATCH /v1/default/banks/claude-memory/memories/m1 {"reason":"forgotten by the user (backsight forget)","state":"invalidated"}`) ||
		!strings.Contains(got, `"state":"valid"`) {
		t.Errorf("calls:\n%s", got)
	}
}

func TestForgetPurgeShowsItsPlanThenNeedsYes(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["o1"] = `{"id":"o1","text":"token was ghp_x","type":"observation","tags":["project:billing"],
		"source_memories":[{"id":"f1","text":"src","type":"world","document_id":"conversation:s1"},{"id":"f2","text":"src2","type":"world","document_id":"conversation:s1"}]}`
	code, out, _ := run(t, "forget", "o1", "--purge", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "conversation:s1") || !strings.Contains(out, "--purge --yes") || len(curation) != 0 {
		t.Fatalf("plan: exit %d, %d calls\n%s", code, len(curation), out)
	}
	if code, out, _ := run(t, "forget", "o1", "--purge", "--yes"); code != output.OK || !strings.Contains(out, "backups") {
		t.Fatalf("purge: exit %d\n%s", code, out)
	}
	got := strings.Join(curation, "\n")
	for _, want := range []string{"PATCH /v1/default/banks/claude-memory/memories/o1", "DELETE /v1/default/banks/claude-memory/memories/f1/observations",
		"DELETE /v1/default/banks/claude-memory/memories/f2/observations", "DELETE /v1/default/banks/claude-memory/documents/conversation%3As1"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if strings.Count(got, "documents/conversation%3As1") != 1 {
		t.Error("a document shared by two facts must be deleted once")
	}
	if code, _, _ := run(t, "forget", "nope", "--purge", "--yes"); code != output.Usage {
		t.Errorf("unknown id: exit %d", code)
	}
}

func TestMemoryScanFindsSecretShapes(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	listAnswer["world"] = `[{"id":"f9","text":"deploy with GITHUB_TOKEN=ghp_` + strings.Repeat("a1B2c3D4e5F6", 3) + `","fact_type":"world","tags":["project:billing"]}]`
	listAnswer["observation"] = `[{"id":"o2","text":"Billing keeps PostgreSQL 16.","fact_type":"observation","tags":["project:billing"]}]`
	code, out, _ := run(t, "memory", "scan")
	if code != output.OK || !strings.Contains(out, "f9") || !strings.Contains(out, "backsight forget f9 --purge") || strings.Contains(out, "o2") || strings.Contains(out, "a1B2c3D4e5F6") {
		t.Errorf("exit %d\n%s", code, out)
	}
	listAnswer["world"] = ""
	if _, out, _ := run(t, "memory", "scan"); !strings.Contains(out, "no secret") {
		t.Errorf("clean bank:\n%s", out)
	}
}

func TestAskReflectsOnTheProjectAndShowsTokens(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	t.Setenv("BACKSIGHT_PROJECT", "billing")
	code, out, _ := run(t, "ask", "Which", "PostgreSQL", "version?")
	if code != output.OK || !strings.Contains(out, "Billing keeps PostgreSQL 16.") || !strings.Contains(out, "1200") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(strings.Join(recallTags, "|"), `"project:billing"`) {
		t.Errorf("not scoped to the project: %q", recallTags)
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
	projectEntities["billing"] = "[]"
	projectEntities["infra"] = "[]"
	recentAnswer[""] = `[{"id":"x","text":"t","fact_type":"world","updated_at":"2026-10-20T09:00:00Z"}]`
	code, out, _ := run(t, "projects")
	if code != output.OK || !strings.Contains(out, "billing") || !strings.Contains(out, "infra") || !strings.Contains(out, "2026-10-20") {
		t.Errorf("exit %d\n%s", code, out)
	}
}
