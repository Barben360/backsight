package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
)

// fakeGitHub answers ghIssue from items ("owner/repo#n" → item or error;
// absent: not found) and records every call.
func fakeGitHub(t *testing.T, items map[string]any) *[]string {
	t.Helper()
	var calls []string
	oldIssue, oldLook := ghIssue, ghLookPath
	ghLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	ghIssue = func(_ context.Context, owner, repo string, n int) (ghItem, error) {
		key := owner + "/" + repo + "#" + itoa(n)
		calls = append(calls, key)
		switch v := items[key].(type) {
		case ghItem:
			return v, nil
		case error:
			return ghItem{}, v
		}
		return ghItem{}, errGHNotFound
	}
	t.Cleanup(func() { ghIssue, ghLookPath = oldIssue, oldLook })
	return &calls
}

func merged(at string) ghItem {
	ts, _ := time.Parse(time.RFC3339, at)
	it := ghItem{State: "closed", ClosedAt: ts}
	it.PullRequest = &struct {
		MergedAt *time.Time `json:"merged_at"`
	}{&ts}
	return it
}

// stateFacts puts facts on the fake server, listed as world facts and
// known to it for curation: id → text, tags.
func stateFacts(t *testing.T, facts ...[3]string) {
	t.Helper()
	var items []string
	for _, f := range facts {
		tags, _ := json.Marshal(strings.Split(f[2], ","))
		text, _ := json.Marshal(f[1])
		item := `{"id":"` + f[0] + `","text":` + string(text) + `,"fact_type":"world","type":"world","tags":` + string(tags) + `}`
		items = append(items, item)
		memories[f[0]] = item
		for _, tg := range strings.Split(f[2], ",") {
			if p, ok := strings.CutPrefix(tg, "project:"); ok {
				projectEntities[p] = "[]"
			}
		}
	}
	listAnswer["world"] = "[" + strings.Join(items, ",") + "]"
	t.Cleanup(func() { listAnswer["world"] = "" })
}

// learntOwners writes the owners this machine learnt from its sessions.
func learntOwners(t *testing.T, home string, owners ...string) {
	t.Helper()
	dir := pathsOf(home).AttributionDir()
	_ = os.MkdirAll(dir, 0o700)
	raw, _ := json.Marshal(owners)
	if err := os.WriteFile(filepath.Join(dir, "owners.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func statesJSON(t *testing.T, args ...string) (int, statesResult, string) {
	t.Helper()
	code, out, errOut := run(t, append(append([]string{"memory", "states"}, args...), "--json")...)
	var env struct {
		Data statesResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	return code, env.Data, out + errOut
}

func outdatedIDs(r statesResult) []string {
	var ids []string
	for _, f := range r.Outdated {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestMemoryStatesListsWhatGitHubContradictsAndChangesNothing(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/Acme")
	stateFacts(t,
		[3]string{"url1", "Tool PR #187 (https://github.com/acme/tool/pull/187) is open and not merged.", "project:tool,kind:state"},
		[3]string{"bare1", "The branch for tool PR #189 is kept open and nothing has been merged.", "project:tool,kind:state"},
		[3]string{"open1", "Tool PR #190 is still open, awaiting the user's review.", "project:tool,kind:state"},
		[3]string{"done1", "Tool PR #176 was not yet merged as of 2026-11-03. It was later squash-merged as 719f94d.", "project:tool,kind:state"},
		[3]string{"dec1", "Tool PR #191 is open and not merged.", "project:tool,kind:decision"},
		[3]string{"plain1", "Tool PR #192 was merged into main.", "project:tool,kind:state"},
	)
	calls := fakeGitHub(t, map[string]any{
		"acme/tool#187": merged("2026-11-04T17:16:40Z"),
		"acme/tool#189": merged("2026-11-04T17:38:49Z"),
		"acme/tool#190": ghItem{State: "open"},
		"acme/tool#191": merged("2026-11-04T17:38:49Z"),
	})
	code, res, out := statesJSON(t)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := outdatedIDs(res); !slices.Equal(got, []string{"url1", "bare1"}) {
		t.Fatalf("outdated %v, want url1 and bare1\n%s", got, out)
	}
	if res.Read != 5 || res.Checked != 4 || res.StillOpen != 1 || res.Skipped[skipSaysDone] != 1 {
		t.Errorf("counts: %+v", res)
	}
	if f := res.Outdated[1]; f.Refs[0].Ref != "acme/tool#189" || f.Refs[0].State != "merged" || f.Command != "backsight forget bare1" {
		t.Errorf("bare ref: %+v", f)
	}
	if slices.Contains(*calls, "acme/tool#191") || slices.Contains(*calls, "acme/tool#176") {
		t.Errorf("checked a fact it must not: %v", *calls)
	}
	if len(curation) != 0 {
		t.Errorf("without --retire nothing is written: %q", curation)
	}
	// the text names each one with its command and the way to retire them
	_, text, _ := run(t, "memory", "states")
	if !strings.Contains(text, "acme/tool#189 merged 2026-11-04") || !strings.Contains(text, "backsight forget bare1") ||
		!strings.Contains(text, "backsight memory states --retire") {
		t.Errorf("text:\n%s", text)
	}
}

func TestMemoryStatesRetireInvalidatesWithGitHubsAnswerAsEvidence(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/acme")
	stateFacts(t,
		[3]string{"url1", "PR https://github.com/acme/tool/pull/187 is open and not merged.", "project:tool,kind:state"},
		[3]string{"open1", "Tool PR #190 is still open.", "project:tool,kind:state"},
	)
	fakeGitHub(t, map[string]any{"acme/tool#187": merged("2026-11-04T17:16:40Z"), "acme/tool#190": ghItem{State: "open"}})
	code, res, out := statesJSON(t, "--retire")
	if code != output.OK || !slices.Equal(res.Retired, []string{"url1"}) {
		t.Fatalf("exit %d %+v\n%s", code, res, out)
	}
	got := strings.Join(curation, "\n")
	want := `PATCH /v1/default/banks/claude-memory/memories/url1 {"reason":"outdated (backsight memory states): GitHub says acme/tool#187 merged 2026-11-04T17:16:40Z","state":"invalidated"}`
	if !strings.Contains(got, want) || strings.Contains(got, "open1") {
		t.Errorf("calls:\n%s\nwant:\n%s", got, want)
	}
	if code, out, _ := run(t, "forget", "--restore", "url1"); code != output.OK || !strings.Contains(strings.Join(curation, "\n"), `"state":"valid"`) {
		t.Errorf("restore: exit %d\n%s", code, out)
	}
}

// A fact whose item GitHub could not read, or that names an item still
// open beside a merged one, is never retired.
func TestMemoryStatesNeverRetiresOnDoubt(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/acme")
	stateFacts(t,
		[3]string{"err1", "Tool PR #1 is open and not merged.", "project:tool,kind:state"},
		[3]string{"half1", "Tool PR #2 fixes https://github.com/acme/tool/issues/3; the PR is not merged yet.", "project:tool,kind:state"},
		[3]string{"gone1", "Tool PR #4 is open and not merged.", "project:tool,kind:state"},
	)
	fakeGitHub(t, map[string]any{
		"acme/tool#1": errors.New("gh api: HTTP 502"),
		"acme/tool#2": merged("2026-11-04T17:16:40Z"),
		"acme/tool#3": ghItem{State: "open"},
	})
	code, res, out := statesJSON(t, "--retire")
	if code != output.OK || len(res.Outdated) != 0 || len(curation) != 0 {
		t.Fatalf("exit %d, retired %q\n%s", code, curation, out)
	}
	if res.Skipped[skipGitHubError] != 1 || res.Skipped[skipNotFound] != 1 || res.StillOpen != 1 {
		t.Errorf("counts: %+v", res)
	}
}

// A bare "PR #N" is looked up in the project the text names before it,
// not in the fact's own project; with no owner known, it is skipped.
func TestMemoryStatesResolvesABareRefByTheProjectItNames(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	stateFacts(t,
		[3]string{"x1", "Clinic-App PR 578 is not merged yet.", "project:my-garage,kind:state"},
		[3]string{"y1", "Other PR #5 is open and not merged.", "project:my-garage,kind:state"},
		[3]string{"z1", "Clinic-app PR #6 and my-garage PR #7 are open.", "project:clinic-app,kind:state"},
	)
	projectEntities["clinic-app"] = "[]"
	calls := fakeGitHub(t, map[string]any{"acme/clinic-app#578": merged("2026-11-08T10:00:00Z"), "acme/my-garage#578": ghItem{State: "open"}})

	// no owner learnt or configured: nothing to look a bare ref up under
	// (y1's name is no known project, whatever the owners)
	if code, res, out := statesJSON(t); code != output.OK || len(*calls) != 0 || res.Skipped[skipNoRepository] != 2 || res.Skipped[skipUnknownName] != 1 {
		t.Fatalf("no owner: exit %d %+v %v\n%s", code, res, *calls, out)
	}
	learntOwners(t, home, "github.com/acme")
	code, res, out := statesJSON(t)
	if code != output.OK || !slices.Equal(outdatedIDs(res), []string{"x1"}) || slices.Contains(*calls, "acme/my-garage#578") {
		t.Fatalf("exit %d %+v %v\n%s", code, res, *calls, out)
	}
	if res.Skipped[skipUnknownName] != 1 || res.Skipped[skipNotFound] != 1 {
		t.Errorf("skipped %v (y1: an unknown name; z1: #6 not found)", res.Skipped)
	}
}

// The same item under two owners is ambiguous: skipped.
func TestMemoryStatesSkipsAnItemFoundUnderSeveralOwners(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/acme", "github.com/other")
	stateFacts(t, [3]string{"a1", "Tool PR #9 is open and not merged.", "project:tool,kind:state"})
	fakeGitHub(t, map[string]any{"acme/tool#9": merged("2026-11-04T17:16:40Z"), "other/tool#9": merged("2026-11-04T17:16:40Z")})
	if code, res, out := statesJSON(t, "--retire"); code != output.OK || len(curation) != 0 || res.Skipped[skipAmbiguous] != 1 {
		t.Errorf("exit %d %+v %q\n%s", code, res, curation, out)
	}
}

func TestMemoryStatesNeedsALoggedInGh(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/acme")
	stateFacts(t, [3]string{"a1", "Tool PR #9 is open and not merged.", "project:tool,kind:state"})
	fakeGitHub(t, map[string]any{"acme/tool#9": errGHAuth})
	if code, _, out := statesJSON(t, "--retire"); code != output.NeedsUser || !strings.Contains(out, "gh auth login") || len(curation) != 0 {
		t.Errorf("not logged in: exit %d %q\n%s", code, curation, out)
	}
	ghLookPath = func(string) (string, error) { return "", errors.New("not found") }
	before := len(bankCalls)
	if code, _, out := statesJSON(t); code != output.NeedsUser || !strings.Contains(out, "memory states checks GitHub with the gh CLI, which is not installed") || len(bankCalls) != before {
		t.Errorf("no gh: exit %d, %d server calls\n%s", code, len(bankCalls)-before, out)
	}
}

// A pull request closed without a merge may be what a "not merged" fact
// says: skipped. An issue closed is outdated, dated by its closing.
func TestMemoryStatesClosedItems(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/acme")
	stateFacts(t,
		[3]string{"pr1", "Tool PR #1 was not merged; superseded by PR #2.", "project:tool,kind:state"},
		[3]string{"is1", "Tool issue #3 is still open.", "project:tool,kind:state"},
	)
	closedAt, _ := time.Parse(time.RFC3339, "2026-11-05T09:00:00Z")
	unmerged := ghItem{State: "closed", ClosedAt: closedAt}
	unmerged.PullRequest = &struct {
		MergedAt *time.Time `json:"merged_at"`
	}{}
	fakeGitHub(t, map[string]any{"acme/tool#1": unmerged, "acme/tool#2": merged("2026-11-04T17:16:40Z"), "acme/tool#3": ghItem{State: "closed", ClosedAt: closedAt}})
	code, res, out := statesJSON(t)
	if code != output.OK || !slices.Equal(outdatedIDs(res), []string{"is1"}) || res.Skipped[skipClosedUnmerged] != 1 {
		t.Fatalf("exit %d %+v\n%s", code, res, out)
	}
	if r := res.Outdated[0].Refs[0]; r.State != "closed" || !r.At.Equal(closedAt) {
		t.Errorf("issue: %+v", r)
	}
}

// A project named in passing does not send a bare ref to its repository.
func TestMemoryStatesSkipsABareRefNamingAnotherProject(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/acme")
	stateFacts(t, [3]string{"a1", "The fix in PR #330 is not merged until the engine release.", "project:tool,kind:state"})
	projectEntities["engine"] = "[]"
	calls := fakeGitHub(t, map[string]any{"acme/engine#330": merged("2026-11-04T17:16:40Z")})
	if code, res, out := statesJSON(t, "--retire"); code != output.OK || len(*calls) != 0 || len(curation) != 0 || res.Skipped[skipOtherProject] != 1 {
		t.Errorf("exit %d %+v %v\n%s", code, res, *calls, out)
	}
}

// --project keeps one project's facts; experience facts are read too.
func TestMemoryStatesProjectFilterAndExperienceFacts(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/acme")
	stateFacts(t, [3]string{"w1", "Tool PR #1 is open and not merged.", "project:tool,kind:state"})
	listAnswer["experience"] = `[{"id":"e1","text":"Other PR #2 is open and not merged.","fact_type":"experience","tags":["project:other","kind:state"]}]`
	t.Cleanup(func() { listAnswer["experience"] = "" })
	projectEntities["other"] = "[]"
	fakeGitHub(t, map[string]any{"acme/tool#1": merged("2026-11-04T17:16:40Z"), "acme/other#2": merged("2026-11-04T17:16:40Z")})
	if code, res, out := statesJSON(t); code != output.OK || !slices.Equal(outdatedIDs(res), []string{"w1", "e1"}) {
		t.Errorf("all: exit %d %+v\n%s", code, res, out)
	}
	if code, res, out := statesJSON(t, "--project", "other"); code != output.OK || res.Read != 1 || !slices.Equal(outdatedIDs(res), []string{"e1"}) {
		t.Errorf("--project other: exit %d %+v\n%s", code, res, out)
	}
}

// A retirement the server refuses stops the run: exit Partial, only the
// ones done reported retired, the rest left.
func TestMemoryStatesRetireStopsAtARefusedWrite(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/acme")
	stateFacts(t,
		[3]string{"a1", "Tool PR #1 is open and not merged.", "project:tool,kind:state"},
		[3]string{"a2", "Tool PR #2 is open and not merged.", "project:tool,kind:state"},
		[3]string{"a3", "Tool PR #3 is open and not merged.", "project:tool,kind:state"},
	)
	memoryStatus["PATCH a2"] = 500
	fakeGitHub(t, map[string]any{"acme/tool#1": merged("2026-11-04T17:16:40Z"), "acme/tool#2": merged("2026-11-04T17:16:40Z"), "acme/tool#3": merged("2026-11-04T17:16:40Z")})
	code, res, out := statesJSON(t, "--retire")
	if code != output.Partial || !slices.Equal(res.Retired, []string{"a1"}) || strings.Contains(strings.Join(curation, "\n"), "a3") {
		t.Errorf("exit %d %+v %q\n%s", code, res, curation, out)
	}
}

// ghIssue reads gh's answers: the item, a 404, a missing login, a reply
// that is no item.
func TestGhIssueReadsGhsAnswers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for gh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	body := `#!/bin/sh
case "$2" in
repos/a/b/issues/1) echo '{"state":"closed","closed_at":"2026-11-05T09:00:00Z","html_url":"https://github.com/a/b/pull/1","pull_request":{"merged_at":"2026-11-05T09:00:00Z"}}' ;;
repos/a/b/issues/2) echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;
repos/a/b/issues/3) echo 'To get started with GitHub CLI, please run:  gh auth login' >&2; exit 4 ;;
repos/a/b/issues/4) echo 'not json' ;;
repos/a/b/issues/5) echo 'gh: Server Error (HTTP 502)' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	old := ghLookPath
	ghLookPath = func(string) (string, error) { return script, nil }
	t.Cleanup(func() { ghLookPath = old })
	ctx := context.Background()
	if it, err := ghIssue(ctx, "a", "b", 1); err != nil || (ghAnswer{item: it}).stateRef().State != "merged" {
		t.Errorf("item: %+v %v", it, err)
	}
	if _, err := ghIssue(ctx, "a", "b", 2); !errors.Is(err, errGHNotFound) {
		t.Errorf("404: %v", err)
	}
	if _, err := ghIssue(ctx, "a", "b", 3); !errors.Is(err, errGHAuth) {
		t.Errorf("login: %v", err)
	}
	for _, n := range []int{4, 5} {
		if _, err := ghIssue(ctx, "a", "b", n); err == nil || errors.Is(err, errGHNotFound) || errors.Is(err, errGHAuth) {
			t.Errorf("#%d: %v", n, err)
		}
	}
}

// A fact older than GitHub's item under its number is about another
// item (the repository was recreated and the number reused): skipped,
// whereas the same item created before the fact is outdated.
func TestMemoryStatesSkipsAnItemNewerThanTheFact(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	listAnswer["world"] = `[{"id":"old1","text":"PR https://github.com/acme/tool/pull/258 is not yet merged.","fact_type":"world","tags":["project:tool","kind:state"],"mentioned_at":"2026-10-25T10:00:00Z"},
		{"id":"new1","text":"PR https://github.com/acme/tool/pull/259 is not yet merged.","fact_type":"world","tags":["project:tool","kind:state"],"mentioned_at":"2026-11-09T12:00:00Z"}]`
	t.Cleanup(func() { listAnswer["world"] = "" })
	created, _ := time.Parse(time.RFC3339, "2026-11-09T09:00:00Z")
	recent := merged("2026-11-09T10:00:00Z")
	recent.CreatedAt = created
	fakeGitHub(t, map[string]any{"acme/tool#258": recent, "acme/tool#259": recent})
	code, res, out := statesJSON(t)
	if code != output.OK || !slices.Equal(outdatedIDs(res), []string{"new1"}) || res.Skipped[skipNewerItem] != 1 {
		t.Errorf("exit %d %+v\n%s", code, res, out)
	}
}

// A state fact that is also a decision, convention, pitfall or correction
// is skipped: retiring it would lose its durable part. A ref named twice
// is shown once.
func TestMemoryStatesSkipsMixedKindsAndShowsARefOnce(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	learntOwners(t, home, "github.com/acme")
	stateFacts(t,
		[3]string{"mix1", "Tool issue #130 remains open: litellm caps filelock below 4.0.", "project:tool,kind:state,kind:pitfall"},
		[3]string{"two1", "Tool issue #131 is still open; the image is tracked in issue #131.", "project:tool,kind:state"},
	)
	calls := fakeGitHub(t, map[string]any{"acme/tool#130": ghItem{State: "closed"}, "acme/tool#131": ghItem{State: "closed"}})
	code, res, out := statesJSON(t)
	if code != output.OK || !slices.Equal(outdatedIDs(res), []string{"two1"}) || res.Skipped[skipMixedKind] != 1 || slices.Contains(*calls, "acme/tool#130") {
		t.Fatalf("exit %d %+v %v\n%s", code, res, *calls, out)
	}
	if n := len(res.Outdated[0].Refs); n != 1 {
		t.Errorf("refs %+v, want #131 once", res.Outdated[0].Refs)
	}
}
