package staleref

import (
	"reflect"
	"testing"
)

// The facts below are the ones measured stale on 2026-11-12 (issue 331),
// as the server wrote them, and their look-alikes that must not match.

func TestClaimsOpen(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"Backsight PR #187 (https://github.com/Barben360/backsight/pull/187) is open and not merged, and its branch has been kept.", true},
		{"The branch for backsight PR #189 is kept open and nothing has been merged.", true},
		{"Backsight PR 318 implements the seven fixes. It was ready to merge but not merged, pending the user's review.", true},
		{"Clinic-App PR 578 failed its checks twice; it is not merged yet and a background watch re-runs the CI.", true},
		{"PR #265 was open and not merged as of 2026-11-10.", true},
		{"Issue #143 is still open for an opt-in daily price refresh.", true},
		{"PR #12 has not yet been merged.", true},
		{"PR #12 is unmerged.", true},
		{"PR #12 awaits the user's review.", true},
		// no claim that something is still open
		{"PR #251 was merged into main as commit aeb629b7 on 2026-11-09.", false},
		{"PR #12 was opened on 2026-10-25.", false},
		{"The open items list holds PR #12.", false},
		{"Release v1.0.1 shipped PR #318.", false},
		// a claim about something else, or not about now
		{"PR #12 fixed the parser; the caching question is still open.", false},
		{"PR #12 fixed the parser. The caching question is still open.", false},
		{"The fix from PR #12 awaits the v1.4 release.", false},
		{"backsight is open source; PR #12 adds X.", false},
		{"PR #12 is open-source tooling.", false},
		{"PR #12 was open for two weeks before the merge.", false},
		// the claim in the next sentence, about the same item
		{"PR #265 holds phase 3. The PR is open and not merged.", true},
		{"Clinic-App PR 578 failed its checks; it is not merged yet.", true},
	} {
		if got := ClaimsOpen(tc.text); got != tc.want {
			t.Errorf("ClaimsOpen(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestSaysDone(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"PR #176 was not yet merged as of 2026-11-03: its CI job was re-run. It was later squash-merged as 719f94d.", true},
		{"PR #103 was open and not merged before the cutover, and is merged during the cutover.", true},
		{"PR #258 was opened on 2026-10-25 and was merged to main on 2026-10-26.", true},
		{"PR #12 has been merged.", true},
		{"PR #12 merged into main on 2026-11-02.", true},
		{"Issue #143 is no longer open.", true},
		{"Issue #9 was closed by PR #10.", true},
		{"Issue #143 was delivered by PR #216.", true},
		// negated or not a completion
		{"The branch for backsight PR #189 is kept open and nothing has been merged.", false},
		{"PR #12 has not been merged.", false},
		{"PR #12 was never merged.", false},
		{"PR #265 was open and not merged as of 2026-11-10.", false},
		{"It was ready to merge but not merged, pending the user's review.", false},
	} {
		if got := SaysDone(tc.text); got != tc.want {
			t.Errorf("SaysDone(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestRefs(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []Ref
	}{
		{"Backsight PR #187 (https://github.com/Barben360/backsight/pull/187) is open.",
			[]Ref{{Owner: "barben360", Repo: "backsight", Number: 187}}},
		{"Phase 3 is in PR #265 (https://github.com/Barben360/backsight/pull/265), which contains `Closes #261`.",
			[]Ref{{Owner: "barben360", Repo: "backsight", Number: 265}}},
		{"See https://github.com/acme/tool/issues/9 and acme/other#4.",
			[]Ref{{Owner: "acme", Repo: "tool", Number: 9}, {Owner: "acme", Repo: "other", Number: 4}}},
		{"The branch for backsight PR #189 is kept open.", []Ref{{Qualifier: "backsight", Number: 189}}},
		{"Backsight PR 318 implements the fixes.", []Ref{{Qualifier: "backsight", Number: 318}}},
		{"Clinic-App PR 578 is not merged yet.", []Ref{{Qualifier: "clinic-app", Number: 578}}},
		{"A merge of PR #255 was queued; pull request #256 and issue 12 too.",
			[]Ref{{Number: 255}, {Number: 256}, {Number: 12}}},
		{"The ledger has 36 alerts (#130 aside); commit 5d41f21f.", nil},
		// a number that counts something is no ref
		{"Merged the PR 3 days ago; issue 2 of the plan.", nil},
		// another repository's #12 does not hide this one
		{"acme/foo#12 merged; backsight PR #12 still open.",
			[]Ref{{Owner: "acme", Repo: "foo", Number: 12}, {Qualifier: "backsight", Number: 12}}},
		{"acme/foo#12 is merged and backsight PR #12 is open.",
			[]Ref{{Owner: "acme", Repo: "foo", Number: 12}, {Qualifier: "backsight", Number: 12}}},
	} {
		if got := Refs(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Refs(%q) =\n%+v, want\n%+v", tc.text, got, tc.want)
		}
	}
}

// The same item named twice (a bare ref and its URL) is checked once.
func TestRefsDropTheBareRefOfAURLAlreadyNamed(t *testing.T) {
	got := Refs("backsight PR #187 (https://github.com/Barben360/backsight/pull/187), PR #187 again")
	want := []Ref{{Owner: "barben360", Repo: "backsight", Number: 187}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestNames(t *testing.T) {
	projects := []string{"backsight", "clinic-app", "my-garage", "app", ".claude"}
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"Clinic-App PR 578 is not merged yet.", []string{"clinic-app"}},
		{"The my-garage PR #103 and backsight PR #12.", []string{"backsight", "my-garage"}},
		{"backsight-archive PRs are merged.", nil}, // a longer name is not the project
		{"The application is ready.", nil},
		{"Settings in .claude stay.", nil}, // names starting with a dot never count
	} {
		if got := Names(tc.text, projects); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Names(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
