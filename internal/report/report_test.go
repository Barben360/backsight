package report

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/stats"
)

func TestScrubHomeOnlyWhereItStartsAPath(t *testing.T) {
	s := NewScrubber("/home/ann")
	for in, want := range map[string]string{
		"/home/ann/.config/x":         "~/.config/x",
		"open /home/ann: denied":      "open ~: denied",
		"/home/ann.bak/x":             "~.bak/x",
		"/home/anna/x and /home/ann/": "/home/anna/x and ~/",
		"/home/ann_2/x":               "/home/ann_2/x",
	} {
		if got := s.String(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestScrubResolvedHome(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	s := NewScrubber(link)
	if got := s.String("in " + real + "/x and " + link + "/y"); got != "in ~/x and ~/y" {
		t.Errorf("got %q", got)
	}
}

func TestScrubDropsServerAnswersAndSecrets(t *testing.T) {
	s := NewScrubber("")
	secret := "sk-ant-" + strings.Repeat("x9Y", 10)
	got := s.String("POST /v1/default/banks/b/memories: HTTP 422 (bad_request): {\"input\":\"my private note\"}; key " + secret)
	if strings.Contains(got, "private note") || strings.Contains(got, secret) {
		t.Errorf("got %q", got)
	}
	if want := "POST /v1/default/banks/b/memories: HTTP 422 (bad_request): [server answer omitted]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := s.String("token: " + secret); !strings.Contains(got, "[redacted:anthropic_key]") {
		t.Errorf("got %q", got)
	}
}

func TestScrubNamesInsideCompoundWords(t *testing.T) {
	s := NewScrubber("")
	s.Name("annsbox", "<host>")
	s.Name("annuser", "<user>")
	s.Name("ab", "<x>") // too short to replace outside a path segment
	got := s.String("annsbox annsbox,annsbox-2 backsight-annsbox annsbox_x annsbox.lan myannsbox annsboxer annuser-laptop ab")
	if want := "<host> <host>,<host>-2 backsight-<host> <host>_x <host>.lan myannsbox annsboxer <user>-laptop ab"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A name is never cut out of a longer word: anns stays in annsboxer.
	near := NewScrubber("")
	near.Name("anns", "<host>")
	if got, want := near.String("annsboxer anns-2 xanns"), "annsboxer <host>-2 xanns"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrubCapsEachString(t *testing.T) {
	got := NewScrubber("").String(strings.Repeat("é", 1000))
	if r := []rune(got); len(r) != MaxField || r[len(r)-1] != '…' {
		t.Errorf("%d runes, ends %q", len(r), string(r[len(r)-1]))
	}
}

func TestDiagnosticScrubsEveryStringAndKey(t *testing.T) {
	s := NewScrubber("/home/ann")
	d := Diagnostic{
		Versions:     Versions{ClaudeCode: "/home/ann/bin/claude"},
		Doctor:       Doctor{Checks: []doctor.Check{{ID: "x", Status: doctor.Error, Summary: "s", Detail: "/home/ann/d", Fix: "rm /home/ann/f"}}},
		Counts:       Counts{ErrorClasses: map[string]int{"/home/ann/k": 2}},
		RecentErrors: []ErrorEvent{{TS: time.Unix(0, 0).UTC(), Class: "c", Message: "/home/ann/m"}},
	}
	out, err := s.Diagnostic(d)
	if err != nil {
		t.Fatal(err)
	}
	c := out.Doctor.Checks[0]
	if out.Versions.ClaudeCode != "~/bin/claude" || c.Detail != "~/d" || c.Fix != "rm ~/f" || out.Counts.ErrorClasses["~/k"] != 2 || out.RecentErrors[0].Message != "~/m" {
		t.Errorf("not scrubbed: %+v", out)
	}
	if !out.RecentErrors[0].TS.Equal(time.Unix(0, 0)) {
		t.Errorf("timestamp changed: %v", out.RecentErrors[0].TS)
	}
}

func TestClassOrder(t *testing.T) {
	warn := doctor.Check{ID: "disk", Status: doctor.Warn}
	errc := doctor.Check{ID: "server", Status: doctor.Error}
	telemetry := map[string]int{"job/tick/partial": 1, "hook_failure/prompt/timeout": 3}
	for want, d := range map[string]Diagnostic{
		"doctor/server/error":         {Doctor: Doctor{Checks: []doctor.Check{warn, errc}}, Counts: Counts{ErrorClasses: telemetry}},
		"hook_failure/prompt/timeout": {Doctor: Doctor{Checks: []doctor.Check{warn}}, Counts: Counts{ErrorClasses: telemetry}},
		"doctor/disk/warn":            {Doctor: Doctor{Checks: []doctor.Check{warn}}},
		"":                            {Doctor: Doctor{Checks: []doctor.Check{{ID: "ok", Status: doctor.OK}}}},
	} {
		if got := Class(d); got != want {
			t.Errorf("class %q, want %q", got, want)
		}
	}
}

func TestIssueURLStaysShortAndTheRestIsPasted(t *testing.T) {
	d := Diagnostic{Versions: Versions{Binary: "v0.5.0", OS: "linux", Arch: "amd64"}, Counts: Counts{ErrorClasses: map[string]int{}, Days: 7}}
	for i := range 60 {
		id := "check" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + strconv.Itoa(i)
		d.Doctor.Checks = append(d.Doctor.Checks, doctor.Check{ID: id, Status: doctor.Warn, Summary: strings.Repeat("a long summary ", 12)})
		d.Doctor.Warnings++
	}
	for i := range 40 {
		d.RecentErrors = append(d.RecentErrors, ErrorEvent{TS: time.Now(), Class: "session_state_error", Message: strings.Repeat("why ", 50) + strconv.Itoa(i)})
	}
	r, err := Build(d, NewScrubber(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.IssueURL) > MaxURL {
		t.Errorf("issue link is %d bytes, over %d", len(r.IssueURL), MaxURL)
	}
	if r.Omitted == 0 {
		t.Error("lines must have been left out")
	}
	u, _ := url.Parse(r.IssueURL)
	body := u.Query().Get("body")
	for _, want := range []string{"left out to keep this link short", pastePlaceholder, "**Versions**: backsight v0.5.0", "**Doctor**: 0 error(s), 60 warning(s)"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	last := d.Doctor.Checks[len(d.Doctor.Checks)-1].ID
	if strings.Contains(body, last) || !strings.Contains(r.Paste, `"`+last+`"`) {
		t.Errorf("the last check should be in the paste block only")
	}
	if !strings.Contains(r.Paste, "why why") || r.Sent {
		t.Errorf("paste block lacks the recent errors, or sent is true")
	}
}

func TestIssueURLKeepsEverythingWhenShort(t *testing.T) {
	d := Diagnostic{Versions: Versions{Binary: "v0.5.0"}, Doctor: Doctor{Checks: []doctor.Check{{ID: "server", Status: doctor.Error, Summary: "Hindsight unreachable"}}, Errors: 1},
		Counts: Counts{ErrorClasses: map[string]int{"job/tick/partial": 2}, Days: 7}}
	r, err := Build(d, NewScrubber(""))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(r.IssueURL)
	body := u.Query().Get("body")
	if r.Omitted != 0 || strings.Contains(body, "left out") || !strings.Contains(body, "- error `server`: Hindsight unreachable") || !strings.Contains(body, "`job/tick/partial` × 2") {
		t.Errorf("omitted %d, body:\n%s", r.Omitted, body)
	}
	if got := u.Query().Get("title"); got != "report: doctor/server/error: Hindsight unreachable" {
		t.Errorf("title %q", got)
	}
	if u.Host != "github.com" || u.Path != "/"+Repo+"/issues/new" {
		t.Errorf("link %s", r.IssueURL)
	}
}

func TestErrorsClassifiesOnlyFailures(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 10, 25, 10, m, 0, 0, time.UTC) }
	ev := []stats.Event{
		{TS: at(1), Event: "job", Name: "tick", Status: "done"},
		{TS: at(2), Event: "job", Name: "tick", Status: "skipped", Details: map[string]any{"reason": "another tick is running"}},
		{TS: at(3), Event: "job", Name: "maintain", Status: "partial", Details: map[string]any{"reason": "backup failed"}},
		{TS: at(4), Event: "tool", Name: "memory_store", OK: false},
		{TS: at(5), Event: "tool", Name: "memory_search", OK: false},
		{TS: at(6), Event: "tool", Name: "memory_search", OK: true},
		{TS: at(7), Event: "retain", Status: "ok"},
		{TS: at(8), Event: "retain", Status: "quarantined"},
		{TS: at(9), Event: "hook_failure", Hook: "session-start", Kind: "panic"},
		{TS: at(10), Event: "spawn_failed", Error: "fork: resource busy"},
		{TS: at(11), Event: "prompt", Decision: "skip"},
		{TS: at(12), Event: "transcript_unknown"},
		{TS: at(13), Event: "hook_failure", Hook: "prompt\nfree text \"x\"", Kind: "timeout"},
	}
	counts, recent := Errors(ev)
	want := map[string]int{"job/maintain/partial": 1, "tool/memory_search/failed": 1, "retain/quarantined": 1, "hook_failure/session-start/panic": 1,
		"spawn_failed": 1, "transcript/unknown_records": 1, "hook_failure/promptfreetextx/timeout": 1}
	if len(counts) != len(want) {
		t.Errorf("counts %v, want %v", counts, want)
	}
	for k, n := range want {
		if counts[k] != n {
			t.Errorf("%s: %d, want %d (all: %v)", k, counts[k], n, counts)
		}
	}
	if len(recent) != 7 || recent[0].Class != "hook_failure/promptfreetextx/timeout" || recent[len(recent)-1].Class != "job/maintain/partial" {
		t.Errorf("recent, newest first: %+v", recent)
	}
	for _, e := range recent {
		if e.Class == "spawn_failed" && e.Message != "fork: resource busy" {
			t.Errorf("message %q", e.Message)
		}
	}
}

func TestErrorsKeepsTheNewest(t *testing.T) {
	var ev []stats.Event
	for i := range 25 {
		ev = append(ev, stats.Event{TS: time.Unix(int64(i), 0), Event: "hook_failure", Hook: "stop", Kind: "timeout"})
	}
	counts, recent := Errors(ev)
	if counts["hook_failure/stop/timeout"] != 25 || len(recent) != MaxRecent || !recent[0].TS.Equal(time.Unix(24, 0)) {
		t.Errorf("counts %v, %d recent, newest %v", counts, len(recent), recent[0].TS)
	}
}

func TestScrubDropsAServerAnswerThatSpansLines(t *testing.T) {
	s := NewScrubber("")
	got := s.String("GET /v1/default/banks/b/stats: HTTP 500 (server_error): {\"detail\":\"boom\nquokka trellis memory\r\nmore\"}")
	if want := "GET /v1/default/banks/b/stats: HTTP 500 (server_error): [server answer omitted]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrubNamesInTheirPercentEncodedForms(t *testing.T) {
	s := NewScrubber("")
	s.Name("coding-agent::memoire", "<bank>")
	s.Name("ann notes", "<bank>")
	for in, want := range map[string]string{
		// hindsight's bankPath escapes ':' too.
		"GET /v1/default/banks/coding-agent%3A%3Amemoire/stats: refused": "GET /v1/default/banks/<bank>/stats: refused",
		"bank coding-agent::memoire":                                     "bank <bank>",
		"?bank=coding-agent%3A%3Amemoire&x=1":                            "?bank=<bank>&x=1",
		"/banks/ann%20notes/stats and ?q=ann+notes":                      "/banks/<bank>/stats and ?q=<bank>",
	} {
		if got := s.String(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestScrubShortNamesOnlyAsPathSegments(t *testing.T) {
	s := NewScrubber("")
	s.Name("jd", "<user>")
	s.Name("ann", "<user>")
	if got, want := s.String(`open /run/media/jd/usb and C:\Users\jd\x: jd is short`), `open /run/media/<user>/usb and C:\Users\<user>\x: jd is short`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := s.String("/mnt/ann/x: ann-2 ann.lan ann"), "/mnt/<user>/x: ann-2 ann.lan ann"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScrubRootsOutsideHome(t *testing.T) {
	s := NewScrubber("/home/ann")
	s.Root("/srv/bs", "<x>")
	s.Root("/srv/bs/data", "<data>")
	s.Root("/home/ann/.local/state/backsight", "<state>") // under home: shown as ~
	s.Root(`D:\bs\cfg`, "<config>")
	for in, want := range map[string]string{
		"open /srv/bs/data/server/.env: denied": "open <data>/server/.env: denied",
		"/srv/bs/database and /srv/bs/data":     "<x>/database and <data>",
		"/home/ann/.local/state/backsight/x":    "~/.local/state/backsight/x",
		`D:\bs\cfg\config.toml`:                 `<config>\config.toml`,
	} {
		if got := s.String(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestScrubSecretValuesAnywhere(t *testing.T) {
	s := NewScrubber("")
	s.Secret("plainwordsecretquokka")
	s.Secret("pass phrase/with slash")
	s.Secret("short") // too short to replace safely
	for in, want := range map[string]string{
		"dial plainwordsecretquokka.lan: refused":                   "dial [redacted:secret].lan: refused",
		"xplainwordsecretquokkay":                                   "x[redacted:secret]y",
		"a pass phrase/with slash b":                                "a [redacted:secret] b",
		"q=pass+phrase%2Fwith+slash&p=pass%20phrase%2Fwith%20slash": "q=[redacted:secret]&p=[redacted:secret]",
		"a short word": "a short word",
	} {
		if got := s.String(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestIssueURLFallsBackToTheHeadWhenTheVersionsAreHuge(t *testing.T) {
	huge := strings.Repeat("é", 400) // 6 bytes each once escaped
	d := Diagnostic{
		Versions: Versions{Binary: "v0.5.0", Commit: huge, Plugin: huge, HindsightPin: huge, HindsightDeployed: huge, ClaudeCode: huge, OSRelease: huge},
		Doctor:   Doctor{Checks: []doctor.Check{{ID: "server", Status: doctor.Error, Summary: "Hindsight unreachable"}}, Errors: 1},
		Counts:   Counts{ErrorClasses: map[string]int{"job/tick/partial": 2}, Days: 7},
	}
	r, err := Build(d, NewScrubber(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.IssueURL) > MaxURL {
		t.Fatalf("issue link is %d bytes, over %d", len(r.IssueURL), MaxURL)
	}
	u, _ := url.Parse(r.IssueURL)
	body := u.Query().Get("body")
	for _, want := range []string{"**What happened**", pastePlaceholder, "left out to keep this link short"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "**Versions**") || strings.Contains(body, "Hindsight unreachable") {
		t.Errorf("the fallback body keeps lines that do not fit:\n%s", body)
	}
	if got := u.Query().Get("title"); got != "report: doctor/server/error: Hindsight unreachable" {
		t.Errorf("title %q", got)
	}
	// Left out: the class, versions and doctor lines, the check, the counts
	// and the error class line; blank lines and headings are not counted.
	if r.Omitted != 6 {
		t.Errorf("omitted %d, want 6", r.Omitted)
	}
	if !strings.Contains(r.Paste, `"commit": "éé`) {
		t.Errorf("the paste block lacks the versions")
	}
}

// Names go longest first, so a project named after the host goes whole;
// a value registered twice keeps its first placeholder.
func TestScrubLongestNameFirstAndFirstPlaceholderWins(t *testing.T) {
	s := NewScrubber("/home/ann")
	s.Name("annsbox", "<host>")
	s.Name("orchard", "<user>")
	s.Project("annsbox-api")
	s.Project("orchard")
	if got, want := s.String("annsbox-api on annsbox for orchard"), "<project> on <host> for <user>"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A project name under 4 characters is a project name wherever a project:
// tag or a --project flag names it, in its encoded forms too; elsewhere
// only a whole path segment.
func TestScrubShortProjectAfterItsPrefix(t *testing.T) {
	s := NewScrubber("/home/ann")
	s.Project("api")
	s.Project("x.y")
	for in, want := range map[string]string{
		"two repositories share project:api":        "two repositories share project:<project>",
		"project:api: github.com/acme/<remote>":     "project:<project>: github.com/acme/<remote>",
		"backsight forget --project api --plan":     "backsight forget --project <project> --plan",
		"forget --project=api.":                     "forget --project=<project>.",
		"GET /tags?q=project%3Aapi: refused":        "GET /tags?q=project%3A<project>: refused",
		"project:api-gateway and project:apis stay": "project:api-gateway and project:apis stay",
		"the api answers in /srv/api/x":             "the api answers in /srv/<project>/x",
		"project:x.y, project:xxy":                  "project:<project>, project:xxy",
	} {
		if got := s.String(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// Names everyone's install shares, and the placeholders' own words, are
// not a user's: hiding them would only blank backsight's own text.
func TestScrubKeepsPublicNames(t *testing.T) {
	s := NewScrubber("/home/ann")
	for _, p := range []string{".claude", "claude", "backsight", "hindsight", "project", "bank"} {
		s.Project(p)
	}
	s.Name("ann-notes", "<bank>")
	in := "~/.claude/settings.json: claude plugin list; backsight forget --project .claude; hindsight bank ann-notes"
	if got, want := s.String(in), "~/.claude/settings.json: claude plugin list; backsight forget --project .claude; hindsight bank <bank>"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Check ids and error classes are the same for every user: no name
// changes them (the search link finds duplicates by class), while the
// text around them is scrubbed. The project names a check lists are
// scrubbed in that check, the short ones as whole words too.
func TestDiagnosticKeepsIdentifiersAndScrubsCheckProjects(t *testing.T) {
	s := NewScrubber("/home/ann")
	s.Project("tick")
	s.Project("disk")
	d := Diagnostic{
		Doctor: Doctor{Checks: []doctor.Check{
			{ID: "disk", Status: doctor.Warn, Summary: "disk is low", Detail: "headless: web (2), orchard (1)", Fix: "backsight forget --project web --plan", Projects: []string{"web", "orchard"}},
			{ID: "server", Status: doctor.OK, Summary: "the web ui answers"},
		}},
		Counts:       Counts{ErrorClasses: map[string]int{"job/tick/failed": 2}},
		RecentErrors: []ErrorEvent{{Class: "job/tick/failed", Message: "tick failed for orchard"}},
	}
	out, err := s.Diagnostic(d)
	if err != nil {
		t.Fatal(err)
	}
	c := out.Doctor.Checks[0]
	if c.ID != "disk" || c.Summary != "<project> is low" || c.Detail != "headless: <project> (2), <project> (1)" || c.Fix != "backsight forget --project <project> --plan" {
		t.Errorf("check %+v", c)
	}
	if got := out.Doctor.Checks[1].Summary; got != "the web ui answers" {
		t.Errorf("a short name is a word outside the check that lists it: %q", got)
	}
	if out.Counts.ErrorClasses["job/tick/failed"] != 2 || out.RecentErrors[0].Class != "job/tick/failed" || out.RecentErrors[0].Message != "<project> failed for <project>" {
		t.Errorf("classes %+v, recent %+v", out.Counts.ErrorClasses, out.RecentErrors)
	}
	if Class(out) != "job/tick/failed" {
		t.Errorf("class %q", Class(out))
	}
}

// A class is built from backsight's own vocabulary: the project every
// telemetry line carries, and a failed scope, never reach it.
func TestErrorClassesNeverHoldAProject(t *testing.T) {
	ev := []stats.Event{
		{Event: "prompt", Scopes: map[string]struct {
			OK bool `json:"ok"`
		}{"project": {OK: false}, "related": {OK: false}}},
		{Event: "job", Name: "tick", Status: "partial", Details: map[string]any{"reason": "orchard: backup failed"}},
		{Event: "retain_failed", Error: "orchard: refused"},
	}
	counts, recent := Errors(ev)
	for k := range counts {
		if strings.Contains(k, "orchard") {
			t.Errorf("class %q", k)
		}
	}
	for _, e := range recent {
		if strings.Contains(e.Class, "orchard") {
			t.Errorf("class %q", e.Class)
		}
	}
	if counts["recall/scope_failed"] != 1 || counts["job/tick/partial"] != 1 || counts["retain_failed"] != 1 {
		t.Errorf("counts %v", counts)
	}
}

// A recorded remote hides in every form a message can quote it: whole, as
// scp (host:path), without its host, and its owner (namespace) alone, in
// any case; a local path remote whole. The repository's last segment
// alone is a project's business, and a public owner is kept.
func TestScrubRemoteForms(t *testing.T) {
	s := NewScrubber("")
	s.Remote("github.com/annsco/skunkworks")
	s.Remote("gitlab.com/rivalgrp/sub/stealth")
	s.Remote("/srv/git/vaultrepo")
	s.Remote("github.com/backsight/tools")
	for in, want := range map[string]string{
		"fetch git@github.com:Annsco/Skunkworks.git failed":          "fetch git@<remote>.git failed",
		"https://github.com:443/annsco/skunkworks.git":               "https://github.com:443/<remote>.git",
		"repository annsco/skunkworks not found":                     "repository <remote> not found",
		"owner Annsco has no access":                                 "owner <remote> has no access",
		"remote github.com/annsco/skunkworks":                        "remote <remote>",
		"git@gitlab.com:rivalgrp/sub/stealth.git and rivalgrp/sub/x": "git@<remote>.git and <remote>/x",
		"group rivalgrp":                "group <remote>",
		"clone /srv/git/vaultrepo.git":  "clone <remote>.git",
		"skunkworks builds":             "skunkworks builds",
		"backsight/tools and backsight": "<remote> and backsight",
	} {
		if got := s.String(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
