package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The browse views. Clock fxNow (2026-10-30T12:00+01:00, Paris), window the
// 7 days before it (from 2026-10-23T13:00+02:00) unless a test says otherwise.

// paris is 2026-10-<day> hh:mm in Paris.
func paris(day, hh, mm int) time.Time { return time.Date(2026, 10, day, hh, mm, 0, 0, fxParis) }

const memorySearch = "mcp__plugin_backsight_memory__memory_search"

// runErr runs view for q on x's source and returns its error.
func runErr(x *fx, view string, q Query) (Result, error) {
	return Run(context.Background(), view, x.source(), q)
}

// notFound is err as a NotFoundError, failing the test when it is not one.
func notFound(t *testing.T, err error) NotFoundError {
	t.Helper()
	var nf NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want a NotFoundError", err)
	}
	return nf
}

// ids are the ids of rows, in order.
func sessionIDs(rows []SessionRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func memoryIDs(rows []MemoryRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func projectNames(rows []ProjectRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Project)
	}
	return out
}

// TestPooledRow: alpha has 10 prompts (4 with memory) and beta 30 (6); the
// rows count them, the pooled row totals 40 and 10 and gives the extremes
// of every column; the rows are the latest activity first; the server's
// tags give each project's memories (0 for a project with no tag); the
// series of a project sums to its prompts; --limit pages the rows but not
// the pooled row; the project filter keeps its rows.
func TestPooledRow(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "a", fxNow.Add(-2*time.Hour), 10, func(i int, p *telemetry.Prompt) {
		if i < 4 {
			p.Injected, p.Chars = 1, 100
		}
	})
	x.prompts("beta", "b", fxNow.Add(-30*time.Hour), 30, func(i int, p *telemetry.Prompt) {
		if i < 6 {
			p.Injected, p.Chars = 1, 100
		}
	})
	x.server.set(func(d *fakeData) {
		d.Tags = []hindsight.TagCount{{Tag: "project:alpha", Count: 120}, {Tag: "kind:decision", Count: 9}}
	})
	r := x.run("projects", x.query(7))
	rows := r.Tables.Projects
	if got := projectNames(rows); !slices.Equal(got, []string{"alpha", "beta"}) { // alpha's last prompt is 28 h later than beta's
		t.Fatalf("projects = %v, want alpha then beta (latest activity first)", got)
	}
	a, b := rows[0], rows[1]
	if a.Sessions != 10 || a.Prompts != 10 || a.PromptsWithMemory != 4 || b.Sessions != 30 || b.Prompts != 30 || b.PromptsWithMemory != 6 {
		t.Errorf("alpha %+v, beta %+v, want 10/10/4 and 30/30/6", a, b)
	}
	if a.MemoriesOnServer == nil || *a.MemoriesOnServer != 120 || b.MemoriesOnServer == nil || *b.MemoriesOnServer != 0 {
		t.Errorf("memories on server: alpha %v, beta %v, want 120 and 0", a.MemoriesOnServer, b.MemoriesOnServer)
	}
	if want := fxNow.Add(-3 * time.Hour); a.LastActivity == nil || !a.LastActivity.Equal(want) {
		t.Errorf("alpha last activity %v, want %v", a.LastActivity, want)
	}
	for _, row := range rows {
		var sum float64
		for _, p := range row.Series {
			sum += *p.Value
		}
		if sum != float64(row.Prompts) || len(row.Series) != 8 {
			t.Errorf("%s: series sums to %v over %d points, want %d prompts over 8 days", row.Project, sum, len(row.Series), row.Prompts)
		}
	}
	p := r.Tables.Pooled
	if p == nil || p.Label != "all projects pooled" || p.Totals.Sessions != 40 || p.Totals.Prompts != 40 || p.Totals.PromptsWithMemory != 10 {
		t.Fatalf("pooled = %+v, want 40 sessions, 40 prompts, 10 with memory", p)
	}
	if p.Min["prompts_with_memory"] != 4 || p.Max["prompts_with_memory"] != 6 || p.Min["prompts"] != 10 || p.Max["prompts"] != 30 || p.Max["used_turns"] != 0 {
		t.Errorf("min %v max %v, want prompts_with_memory 4..6 and prompts 10..30", p.Min, p.Max)
	}
	if p.Totals.LastActivity == nil || !p.Totals.LastActivity.Equal(*a.LastActivity) {
		t.Errorf("pooled last activity %v, want alpha's %v", p.Totals.LastActivity, a.LastActivity)
	}
	if pg := r.Tables.Page; pg == nil || *pg != (Page{Limit: 50, Offset: 0, Total: 2}) {
		t.Errorf("page = %+v, want 50/0/2", pg)
	}

	q := x.query(7)
	q.Limit, q.Offset = 1, 1
	r = x.run("projects", q)
	if got := projectNames(r.Tables.Projects); !slices.Equal(got, []string{"beta"}) || r.Tables.Pooled.Totals.Prompts != 40 || *r.Tables.Page != (Page{Limit: 1, Offset: 1, Total: 2}) {
		t.Errorf("page 2 of size 1: %v, pooled prompts %d, page %+v, want beta, 40, 1/1/2", got, r.Tables.Pooled.Totals.Prompts, *r.Tables.Page)
	}
	q = x.query(7)
	q.Project = "beta"
	if got := projectNames(x.run("projects", q).Tables.Projects); !slices.Equal(got, []string{"beta"}) {
		t.Errorf("--project beta lists %v", got)
	}
}

// TestProjectsSessionsWithoutProjectAreLeftOut: a session whose telemetry
// names no project is in no row, nor in the pooled totals.
func TestProjectsSessionsWithoutProjectAreLeftOut(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "a", fxNow.Add(-2*time.Hour), 2, nil)
	x.prompts("", "n", fxNow.Add(-2*time.Hour), 3, nil)
	r := x.run("projects", x.query(7))
	if got := projectNames(r.Tables.Projects); !slices.Equal(got, []string{"alpha"}) || r.Tables.Pooled.Totals.Sessions != 2 {
		t.Errorf("projects %v, pooled sessions %d, want only alpha, 2", got, r.Tables.Pooled.Totals.Sessions)
	}
}

// TestProjectUsedTurnsAndWrong: used_turns counts the main-agent turns
// whose final stop declared a used credit, reported_wrong the wrong and
// outdated credits, both by the session's project (alpha's turns are not
// beta's even when the memory belongs to beta).
func TestProjectUsedTurnsAndWrong(t *testing.T) {
	x := newFx(t, fxNow)
	x.declaredTurn(hoursAgo(1), "s1", "declared", "used", "m1")
	x.declaredTurn(hoursAgo(2), "s2", "declared", "used", "m2")
	x.declaredTurn(hoursAgo(3), "s3", "declared", "wrong", "m3")
	x.declaredTurn(hoursAgo(4), "s4", "declared", "outdated", "m4")
	x.declaredTurn(hoursAgo(5), "s5", "undeclared", "used", "m5") // a used credit, but the turn ended undeclared
	x.turnEvent(hoursAgo(6), "b1", "beta")
	x.shows(hoursAgo(6), "b1", fxTurn, "", "beta", "prompt", "alpha", "m6") // beta's turn, an alpha memory
	x.credit(hoursAgo(6).Add(time.Second), "b1", "", "beta", "used", "m6")
	x.stop(hoursAgo(6).Add(time.Minute), "b1", "", "declared")
	rows := x.run("projects", x.query(7)).Tables.Projects
	by := map[string]ProjectRow{}
	for _, r := range rows {
		by[r.Project] = r
	}
	if a := by["alpha"]; a.UsedTurns != 2 || a.ReportedWrong != 2 || a.Sessions != 5 {
		t.Errorf("alpha = %+v, want 2 used turns, 2 reported (wrong and outdated), 5 sessions", a)
	}
	if b := by["beta"]; b.UsedTurns != 1 || b.ReportedWrong != 0 || b.Sessions != 1 {
		t.Errorf("beta = %+v, want 1 used turn, none reported", b)
	}
}

// TestAttributionDisagreements (M7): five main-agent attribution rows of
// alpha in the window, two whose path rule would file them under beta; a
// subagent row, a row applied to beta, a row before the window and one
// whose rule agrees do not count.
func TestAttributionDisagreements(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "a", fxNow.Add(-2*time.Hour), 1, nil)
	attr := func(h time.Duration, subagent bool, applied, rule string) {
		x.tel(fxNow.Add(-h), telemetry.Attribution{Turn: "t1", Subagent: subagent, Base: "alpha", Applied: applied, PathRule: rule}, "a-0", "alpha")
	}
	for i := range 5 {
		rule := "alpha"
		if i < 2 {
			rule = "beta"
		}
		attr(time.Duration(i+1)*time.Hour, false, "alpha", rule)
	}
	attr(7*time.Hour, true, "alpha", "beta")  // a subagent
	attr(8*time.Hour, false, "beta", "alpha") // applied to beta
	attr(9*day, false, "alpha", "beta")       // before the window
	r := x.run("projects", projectQuery(x, "alpha"))
	if got := r.Tables.Project.AttributionDisagreements; got != 2 {
		t.Errorf("attribution_disagreements = %d, want 2 (of 5 main-agent rows)", got)
	}
	if r.Scope.Project != "alpha" {
		t.Errorf("scope.project = %q", r.Scope.Project)
	}
}

// projectQuery is a 7-day query for the page of project name.
func projectQuery(x *fx, name string) Query {
	q := x.query(7)
	q.ID = name
	return q
}

// TestEmptyProject: gamma has sessions but no project:gamma tag on the
// server: no_memories and the command that gives it some. A project with a
// tag, a server that cannot be read, and an unknown project say otherwise.
func TestEmptyProject(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("gamma", "g", fxNow.Add(-2*time.Hour), 2, nil)
	x.prompts("alpha", "a", fxNow.Add(-2*time.Hour), 2, nil)
	x.server.set(func(d *fakeData) { d.Tags = []hindsight.TagCount{{Tag: "project:alpha", Count: 5}} })
	p := x.run("projects", projectQuery(x, "gamma")).Tables.Project
	if !p.NoMemories || p.Command != "backsight retain backfill --project gamma --plan" {
		t.Errorf("gamma: no_memories %v, command %q", p.NoMemories, p.Command)
	}
	if p := x.run("projects", projectQuery(x, "alpha")).Tables.Project; p.NoMemories || p.Command != "" {
		t.Errorf("alpha has a tag: no_memories %v, command %q", p.NoMemories, p.Command)
	}
	x.server.fail("ProjectTags", hindsight.ErrUnreachable)
	if p := x.run("projects", projectQuery(x, "gamma")).Tables.Project; p.NoMemories || p.Command != "" {
		t.Errorf("tags unreadable: no_memories %v, command %q, want neither claimed", p.NoMemories, p.Command)
	}
	_, err := runErr(x, "projects", projectQuery(x, "nowhere"))
	if nf := notFound(t, err); nf.Kind != "project" || nf.ID != "nowhere" {
		t.Errorf("unknown project: %+v", nf)
	}
}

// TestProjectPageMemories: the project's most used memories (recent-
// weighted credits) and its never used ones, by the memory's project, with
// their commands; another project's memories and a session-project mismatch
// stay out.
func TestProjectPageMemories(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "a", fxNow.Add(-2*time.Hour), 1, nil)
	// the session is alpha's; the memory's project is the tag of the latest show
	x.shows(hoursAgo(1), "s1", "t1", "", "alpha", "prompt", "alpha", "u-big", "u-small", "never-a", "other-b", "u-hid")
	x.shows(hoursAgo(2), "s2", "t1", "", "alpha", "prompt", "beta", "beta-used")
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{
		"u-big":     {Shown: 8, Credits: 5, CreditsAt: fxNow, LastShown: ago(time.Hour)},
		"u-small":   {Shown: 20, Credits: 1, CreditsAt: fxNow, LastShown: ago(2 * time.Hour)},
		"never-a":   {Shown: 12, LastShown: ago(3 * time.Hour)},
		"other-b":   {Shown: 30, Credits: 9, CreditsAt: fxNow}, // alpha shown it, but its latest show names alpha too: see below
		"beta-used": {Shown: 4, Credits: 7, CreditsAt: fxNow},
		"u-hid":     {Shown: 9, Credits: 50, CreditsAt: fxNow, Hidden: &feedback.Hidden{Since: ago(day)}}, // hidden: not listed
	}})
	x.shows(hoursAgo(0), "s3", "t1", "", "alpha", "prompt", "beta", "other-b") // other-b's latest show: beta
	p := x.run("projects", projectQuery(x, "alpha")).Tables.Project
	var top, never []string
	for _, r := range p.TopUsed {
		top = append(top, r.ID+" "+r.Command)
	}
	for _, r := range p.NeverUsed {
		never = append(never, r.ID+" "+r.Command)
	}
	if want := []string{"u-big backsight read u-big", "u-small backsight read u-small"}; !slices.Equal(top, want) {
		t.Errorf("top_used = %v, want %v", top, want)
	}
	if want := []string{"never-a backsight forget never-a"}; !slices.Equal(never, want) {
		t.Errorf("never_used = %v, want %v", never, want)
	}
	if r := p.TopUsed[0]; r.Project != "alpha" || r.Shown != 8 || !r.LastShown.Equal(ago(time.Hour)) {
		t.Errorf("top_used[0] = %+v", r)
	}
}

// TestSessionsHas: four sessions in the window, one with a used credit, one
// with a wrong credit, one whose turn never ended (more than 2 h ago), one
// with a briefing only: --has used 1, wrong 1, undeclared 1, memory 3. A
// turn still inside the 2 hours is pending, not undeclared.
func TestSessionsHas(t *testing.T) {
	x := newFx(t, fxNow)
	x.declaredTurn(hoursAgo(1), "s-used", "declared", "used", "m1")
	x.declaredTurn(hoursAgo(2), "s-wrong", "declared", "wrong", "m2")
	x.declaredTurn(hoursAgo(3), "s-undeclared", "", "", "m3") // no stop, 6 h ago
	x.turnEvent(hoursAgo(4), "s-brief", "alpha")
	x.shows(hoursAgo(4), "s-brief", fxTurn, "", "alpha", "briefing", "", "b1")
	x.tel(hoursAgo(4), telemetry.Briefing{IDs: []string{"b1"}, Chars: 300}, "s-brief", "alpha")
	q := x.query(7)
	if got := len(x.run("sessions", q).Tables.Sessions); got != 4 {
		t.Fatalf("%d sessions, want 4", got)
	}
	for has, want := range map[string][]string{
		"used":       {"s-used"},
		"wrong":      {"s-wrong"},
		"undeclared": {"s-undeclared"},
		"memory":     {"s-used", "s-wrong", "s-undeclared"},
	} {
		q.Has = has
		r := x.run("sessions", q)
		got := sessionIDs(r.Tables.Sessions)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) || r.Tables.Page.Total != len(want) {
			t.Errorf("--has %s = %v (total %d), want %v", has, got, r.Tables.Page.Total, want)
		}
	}
	y := newFx(t, fxNow)
	y.declaredTurn(fxNow.Add(-30*time.Minute), "s-open", "", "", "m1")          // 30 min: pending
	y.declaredTurn(fxNow.Add(-2*time.Hour-time.Minute), "s-late", "", "", "m2") // 2 h 1 min: undeclared
	y.declaredTurn(fxNow.Add(-3*time.Hour), "s-blocked", "blocked", "", "m3")   // a last stop of blocked reads undeclared
	q = y.query(7)
	q.Has = "undeclared"
	if got := sessionIDs(y.run("sessions", q).Tables.Sessions); !slices.Equal(got, []string{"s-late", "s-blocked"}) {
		t.Errorf("--has undeclared = %v, want the late and the blocked one (latest first), not the open one", got)
	}
	q.Has = "nope"
	if _, err := runErr(y, "sessions", q); err == nil {
		t.Error("--has nope: no error")
	}
}

// TestSessionsSent: a session's sent state is the last status of its
// documents (conversation:<sid>, @<project>, #n successors): ok only when
// all are ok, else the worst; a session with none says nothing; other
// sessions' documents and notes are not its.
func TestSessionsSent(t *testing.T) {
	x := newFx(t, fxNow)
	for _, s := range []string{"s-ok", "s-mixed", "s-none", "s-other"} {
		x.prompts("alpha", s, fxNow.Add(-2*time.Hour), 1, nil)
	}
	send := func(h int, doc, status string) {
		x.tel(fxNow.Add(-time.Duration(h)*time.Hour), telemetry.Retain{Document: doc, Items: 1, Status: status, MS: 5}, "x", "alpha")
	}
	send(10, "conversation:s-ok-0", "ok")
	send(9, "conversation:s-ok-0@beta", "ok")
	send(8, "conversation:s-ok-0#2", "ok")
	send(10, "conversation:s-mixed-0", "ok")
	send(9, "conversation:s-mixed-0#2", "queued")
	send(8, "conversation:s-mixed-0#3", "failed")
	send(10, "conversation:s-other-0X", "failed") // a different session whose id starts the same
	send(10, "note:s-none-0", "failed")           // not a conversation document
	send(7, "conversation:s-mixed-0", "ok")       // the first document is ok again; the others are as they were
	sent := map[string]string{}
	for _, r := range x.run("sessions", x.query(7)).Tables.Sessions {
		sent[r.ID] = r.Sent
	}
	want := map[string]string{"s-ok-0": "ok", "s-mixed-0": "failed", "s-none-0": "", "s-other-0": ""}
	for id, w := range want {
		if sent[id] != w {
			t.Errorf("sent[%s] = %q, want %q", id, sent[id], w)
		}
	}
}

// TestSessionIDPrefixes (RF 4, view side): a unique prefix of 8 characters
// finds its session; 7 characters are too short; a prefix of two sessions
// lists both; an unknown id is not found; the full id of a short session
// resolves; a memory id resolves exactly and by a unique prefix.
func TestSessionIDPrefixes(t *testing.T) {
	x := newFx(t, fxNow)
	for _, s := range []string{"11111111-aaaa", "22222222-bbbb", "33333333-cc01", "33333333-cc02", "short"} {
		x.tel(fxNow.Add(-2*time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, s, "alpha")
	}
	x.shows(hoursAgo(1), "11111111-aaaa", fxTurn, "", "alpha", "prompt", "", "world:abc:12345678")
	q := x.query(7)
	page := func(id string) (Result, error) {
		q.ID = id
		return runErr(x, "sessions", q)
	}
	if r, err := page("11111111"); err != nil || r.Tables.Session.Totals.ID != "11111111-aaaa" {
		t.Errorf("unique prefix: %v, %+v", err, r.Tables.Session)
	}
	if r, err := page("11111111-aaaa"); err != nil || r.Tables.Session.Totals.ID != "11111111-aaaa" {
		t.Errorf("exact: %v", err)
	}
	if r, err := page("short"); err != nil || r.Tables.Session.Totals.ID != "short" {
		t.Errorf("a short id that exists resolves exactly: %v", err)
	}
	_, err := page("1111111")
	if nf := notFound(t, err); nf.Kind != "session" || !nf.TooShort || len(nf.Matches) != 0 {
		t.Errorf("7 characters: %+v, want TooShort", nf)
	}
	_, err = page("33333333")
	if nf := notFound(t, err); nf.TooShort || !slices.Equal(nf.Matches, []string{"33333333-cc01", "33333333-cc02"}) {
		t.Errorf("two matches: %+v", nf)
	}
	_, err = page("99999999")
	if nf := notFound(t, err); nf.TooShort || len(nf.Matches) != 0 || nf.Kind != "session" || nf.ID != "99999999" {
		t.Errorf("unknown: %+v", nf)
	}
	q.ID = "world:abc:12345678"
	if r, err := runErr(x, "memories", q); err != nil || r.Tables.Memory.Row.ID != "world:abc:12345678" {
		t.Errorf("memory id: %v", err)
	}
	q.ID = "world:ab" // 8 characters
	if r, err := runErr(x, "memories", q); err != nil || r.Tables.Memory.Row.ID != "world:abc:12345678" {
		t.Errorf("memory prefix: %v", err)
	}
	q.ID = "world:a"
	_, err = runErr(x, "memories", q)
	if nf := notFound(t, err); !nf.TooShort || nf.Kind != "memory" {
		t.Errorf("short memory prefix: %+v", nf)
	}
	q.ID = "world:zzzzzzzz"
	_, err = runErr(x, "memories", q)
	if nf := notFound(t, err); nf.TooShort || nf.Kind != "memory" {
		t.Errorf("unknown memory: %+v", nf)
	}
}

// TestSessionPageIgnoresTheWindowAndTheProject: a session page shows all of
// the session, the window and the project filter notwithstanding.
func TestSessionPageIgnoresTheWindowAndTheProject(t *testing.T) {
	x := newFx(t, fxNow)
	x.tel(fxNow.Add(-20*day), telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Chars: 100}, "old-session", "alpha")
	x.tel(fxNow.Add(-2*time.Hour), telemetry.Prompt{N: 2, Decision: "recall", Plen: 80}, "old-session", "alpha")
	q := x.query(7)
	q.ID, q.Project = "old-session", "beta"
	r := x.run("sessions", q)
	if tot := r.Tables.Session.Totals; tot.Prompts != 2 || tot.PromptsWithMemory != 1 || tot.ContextChars != 100 || tot.Project != "alpha" {
		t.Errorf("totals = %+v, want both prompts of alpha", tot)
	}
	if n := len(r.Tables.Session.Turns); n != 2 {
		t.Errorf("%d turns, want 2", n)
	}
}

// TestTurnRail: prompt 1 (recall, two shows ranked 1 and 3, one credited,
// a memory search, a subagent show and search that are not the main
// agent's), prompt 2 (skip, gap), prompt 3 (recall that injected nothing,
// below_floor) and a journal turn no prompt names: the rail has the four
// rows in time order with the shows' outcomes, the searches, the verdicts
// and the final check.
func TestTurnRail(t *testing.T) {
	x := newFx(t, fxNow)
	const s = "rail-session"
	t1, t2, t3, t9 := hoursAgo(5), hoursAgo(4), hoursAgo(3), fxNow.Add(-10*time.Minute)
	x.tel(t1, telemetry.Prompt{N: 1, Decision: "recall", Injected: 2, Plen: 80}, s, "alpha")
	x.tel(t2, telemetry.Prompt{N: 2, Decision: "skip", Reason: "gap", Plen: 5}, s, "alpha")
	x.tel(t3, telemetry.Prompt{N: 3, Decision: "recall", Empty: "below_floor", Plen: 80}, s, "alpha")
	x.turnEvent(t1.Add(-time.Second), s, "alpha") // turn t1, n 1
	x.journal(feedback.Event{TS: t3.Add(-time.Second), Kind: "turn", Session: s, Turn: "t3", Project: "alpha", N: 3})
	x.journal(feedback.Event{TS: t9.Add(-time.Second), Kind: "turn", Session: s, Turn: "t9", Project: "alpha", N: 9})
	ranked := func(at time.Time, id string, rank int) string {
		e := feedback.Event{TS: at, Kind: "shows", Session: s, Turn: "t1", Project: "alpha", Path: "prompt", Items: []feedback.Item{{ID: id, Rank: rank}}}
		x.journal(e)
		return feedback.ShowsOf(e)[0].Key
	}
	ka, kb := ranked(t1, "m-a", 1), ranked(t1.Add(time.Second), "m-b", 3)
	x.shows(t1.Add(2*time.Second), s, "t1", "sub", "alpha", "prompt", "", "m-sub") // a subagent's show
	x.credit(t1.Add(30*time.Second), s, "", "alpha", "used", "m-a")
	x.credit(t1.Add(31*time.Second), s, "", "alpha", "wrong", "m-b")
	x.credit(t1.Add(32*time.Second), s, "", "alpha", "used", "m-a")          // a verdict is listed once
	x.credit(t1.Add(33*time.Second), s, "sub", "alpha", "outdated", "m-sub") // a subagent's credit
	x.stop(t1.Add(time.Minute), s, "", "declared")
	x.settle(t1.Add(2*time.Hour), map[string]string{ka: feedback.OutcomeCredit, kb: feedback.OutcomeMiss})
	x.shows(t9, s, "t9", "", "alpha", "prompt", "", "m-open") // turn t9: shown, no stop, 10 minutes ago
	x.toolUse(t1.Add(5*time.Second), s, "alpha", 1, memorySearch, false)
	x.toolUse(t1.Add(6*time.Second), s, "alpha", 1, memorySearch, true)
	x.toolUse(t1.Add(7*time.Second), s, "alpha", 1, "Bash", false)
	x.toolUse(t1.Add(8*time.Second), s, "alpha", 1, "mcp__plugin_backsight_memory__memory_read", false)

	q := x.query(7)
	q.ID = s
	r := x.run("sessions", q)
	turns := r.Tables.Session.Turns
	if len(turns) != 4 {
		t.Fatalf("%d rows, want 4: %+v", len(turns), turns)
	}
	n := func(v int64) *int64 { return &v }
	rk := func(v int64) *int64 { return &v }
	want := []TurnRow{
		{N: n(1), Turn: "t1", At: t1, Decision: "recall", Shows: []ShowChip{
			{MemoryID: "m-a", Path: "prompt", Outcome: "credit", Rank: rk(1)},
			{MemoryID: "m-b", Path: "prompt", Outcome: "confident_miss", Rank: rk(3)}},
			Searches: 2, Declared: []string{"used", "wrong"}, Check: "declared"},
		{N: n(2), At: t2, Decision: "skip", Reason: "gap", Shows: []ShowChip{}, Declared: []string{}},
		{N: n(3), Turn: "t3", At: t3, Decision: "recall", Empty: "below_floor", Shows: []ShowChip{}, Declared: []string{}},
		{N: n(9), Turn: "t9", At: t9.Add(-time.Second), Shows: []ShowChip{{MemoryID: "m-open", Path: "prompt", Outcome: "pending", Rank: rk(1)}},
			Declared: []string{}, Check: "pending"},
	}
	for i, w := range want {
		g := turns[i]
		if !g.At.Equal(w.At) {
			t.Errorf("row %d at %v, want %v", i, g.At, w.At)
		}
		g.At, w.At = time.Time{}, time.Time{}
		if got, want := string(mustJSON(t, g)), string(mustJSON(t, w)); got != want {
			t.Errorf("row %d = %s, want %s", i, got, want)
		}
	}
	// the totals are the sessions view's: every show and credit of the session, any agent
	if tot := r.Tables.Session.Totals; tot.Prompts != 3 || tot.PromptsWithMemory != 1 || tot.Shown != 4 || tot.Used != 2 || tot.Wrong != 2 {
		t.Errorf("totals = %+v, want 3 prompts, 1 with memory, 4 shown, 2 used credits, 2 wrong or outdated", tot)
	}
}

// memParams are the feedback parameters the default configuration gives
// recall: a memory's boost is read with them.
func memParams() feedback.Params {
	const d = 24 * time.Hour
	return feedback.Params{ConfidentRank: 2, MissHalfLife: 90 * d, CreditHalfLife: 365 * d, TurnTimeout: feedback.TurnTimeout,
		Weight: 0.1, PriorCredits: 3, PriorMisses: 9}
}

// browseLedger is a ledger of five memories at fxNow: m-a used 4 and missed
// 1 (rate 0.8), m-b shown 12 times and never used, m-c credited once a year
// ago (half-life 365 days: 0.5 now) and missed 3 times 90 days ago (half-life
// 90 days: 1.5 now), m-d reported wrong,
// m-e hidden for 10 days; m-restore holds only a restore.
func browseLedger() feedback.Ledger {
	return feedback.Ledger{Memories: map[string]feedback.Entry{
		"m-a":       {Shown: 20, Credits: 4, CreditsAt: fxNow, Misses: 1, MissesAt: fxNow, LastShown: ago(time.Hour), LastCredited: ago(2 * time.Hour)},
		"m-b":       {Shown: 12, LastShown: ago(5 * time.Hour)},
		"m-c":       {Shown: 5, Credits: 1, CreditsAt: ago(365 * day), Misses: 3, MissesAt: ago(90 * day), LastShown: ago(3 * time.Hour)},
		"m-d":       {Shown: 3, Wrong: 1, LastShown: ago(30 * time.Minute)},
		"m-e":       {Shown: 3, Hidden: &feedback.Hidden{Since: ago(10 * day)}},
		"m-restore": {UnhiddenAt: ago(day)},
	}}
}

// TestMemoriesSortAndState: --sort orders largest (or latest) first with
// the id as tie-breaker and a missing rate or show last; --state keeps one
// state as `memory stale` sorts them; used and ignored are the credits and
// misses decayed to now, rate their ratio (null with neither), boost the
// ledger's own Entry.Boost; a restore-only entry is no memory; paging
// slices the ordered list; a bad sort or state is refused.
func TestMemoriesSortAndState(t *testing.T) {
	x := newFx(t, fxNow)
	x.ledger(browseLedger())
	q := x.query(7)
	ids := func(q Query) []string { return memoryIDs(x.run("memories", q).Tables.Memories) }
	for sort, want := range map[string][]string{
		"":           {"m-a", "m-b", "m-c", "m-d", "m-e"}, // shown: 20 12 5 3 3
		"shown":      {"m-a", "m-b", "m-c", "m-d", "m-e"},
		"used":       {"m-a", "m-c", "m-b", "m-d", "m-e"}, // 4 0.5 0 0 0
		"rate":       {"m-a", "m-c", "m-b", "m-d", "m-e"}, // 0.8 0.25 then no rate, by id
		"last_shown": {"m-d", "m-a", "m-c", "m-b", "m-e"}, // 30 min, 1 h, 3 h, 5 h, never
	} {
		q.Sort = sort
		if got := ids(q); !slices.Equal(got, want) {
			t.Errorf("--sort %q = %v, want %v", sort, got, want)
		}
	}
	q.Sort = ""
	for state, want := range map[string]string{"never_used": "m-b", "wrong": "m-d", "hidden": "m-e"} {
		q.State = state
		if got := ids(q); !slices.Equal(got, []string{want}) {
			t.Errorf("--state %s = %v, want [%s]", state, got, want)
		}
	}
	q.State = ""
	rows := map[string]MemoryRow{}
	r := x.run("memories", q)
	for _, m := range r.Tables.Memories {
		rows[m.ID] = m
	}
	a, c := rows["m-a"], rows["m-c"]
	wantBoost := browseLedger().Memories["m-a"].Boost(memParams(), fxNow)
	if a.Shown != 20 || a.Used != 4 || a.Ignored != 1 || a.Rate == nil || math.Abs(*a.Rate-0.8) > 1e-9 || a.State != "" ||
		a.Boost != wantBoost || math.Abs(a.Boost-0.1*((4.0+3)/(4+1+12.0)-0.25)) > 1e-9 || a.Boost == 0 {
		t.Errorf("m-a = %+v, want 20 shown, 4 used, 1 ignored, rate 0.8, boost %v", a, wantBoost)
	}
	if a.LastShown == nil || !a.LastShown.Equal(ago(time.Hour)) || a.LastUsed == nil || !a.LastUsed.Equal(ago(2*time.Hour)) {
		t.Errorf("m-a last shown %v, last used %v", a.LastShown, a.LastUsed)
	}
	if math.Abs(c.Used-0.5) > 1e-9 || math.Abs(c.Ignored-1.5) > 1e-9 || c.Rate == nil || math.Abs(*c.Rate-0.25) > 1e-9 {
		t.Errorf("m-c = %+v, want used 0.5 (a year of decay), ignored 1.5 (one miss half-life), rate 0.25", c)
	}
	if b := rows["m-b"]; b.Rate != nil || b.State != "never_used" || b.LastUsed != nil {
		t.Errorf("m-b = %+v, want no rate, never_used, never used", b)
	}
	if _, ok := rows["m-restore"]; ok {
		t.Error("a restore-only entry is listed")
	}
	if m := r.Metrics["stale.never_used"]; m.Aggregation != "recent_weighted" || m.Value == nil || *m.Value != 1 {
		t.Errorf("stale.never_used = %+v, want recent_weighted, 1", m)
	}
	q.Limit, q.Offset = 2, 1
	r = x.run("memories", q)
	if got := memoryIDs(r.Tables.Memories); !slices.Equal(got, []string{"m-b", "m-c"}) || *r.Tables.Page != (Page{Limit: 2, Offset: 1, Total: 5}) {
		t.Errorf("page = %v %+v, want m-b m-c of 5", got, *r.Tables.Page)
	}
	q = x.query(7)
	q.Sort = "nope"
	if _, err := runErr(x, "memories", q); err == nil {
		t.Error("--sort nope: no error")
	}
	q = x.query(7)
	q.State = "nope"
	if _, err := runErr(x, "memories", q); err == nil {
		t.Error("--state nope: no error")
	}
}

// TestMemoriesProjectIsTheMemorys: a memory's project is that of its latest
// show (the tag it carries), not its session's; --project keeps the
// memories of that project, whatever sessions showed them.
func TestMemoriesProjectIsTheMemorys(t *testing.T) {
	x := newFx(t, fxNow)
	x.ledger(browseLedger())
	x.shows(hoursAgo(5), "s1", "t1", "", "alpha", "prompt", "beta", "m-a")  // an earlier show: beta
	x.shows(hoursAgo(4), "s1", "t2", "", "alpha", "prompt", "alpha", "m-a") // the latest: alpha
	x.shows(hoursAgo(3), "s2", "t1", "", "alpha", "prompt", "beta", "m-c")  // an alpha session showing a beta memory
	q := x.query(7)
	project := func(p string) []string {
		q.Project = p
		return memoryIDs(x.run("memories", q).Tables.Memories)
	}
	if got := project("alpha"); !slices.Equal(got, []string{"m-a"}) {
		t.Errorf("--project alpha = %v, want [m-a]", got)
	}
	if got := project("beta"); !slices.Equal(got, []string{"m-c"}) {
		t.Errorf("--project beta = %v, want [m-c]", got)
	}
	q.Project = ""
	for _, m := range x.run("memories", q).Tables.Memories {
		if want := map[string]string{"m-a": "alpha", "m-c": "beta"}[m.ID]; m.Project != want {
			t.Errorf("%s project %q, want %q", m.ID, m.Project, want)
		}
	}
}

// TestMemoryPage: a memory's kind, state and creation time from the server
// (a zone-less time is UTC), its shows latest first with their settle
// outcome (pending until settled; the sources answers are not shows), its
// rank histogram over all the shows (the page slices only the list), its
// hides and restores, and the three commands.
func TestMemoryPage(t *testing.T) {
	x := newFx(t, fxNow)
	const id = "mem-page-0001"
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{id: {Shown: 5, Credits: 1, CreditsAt: fxNow}}})
	show := func(at time.Time, session, path string, rank, withoutBoost int, score float64) string {
		e := feedback.Event{TS: at, Kind: "shows", Session: session, Turn: "t1", Project: "alpha", Path: path,
			Items: []feedback.Item{{ID: id, Rank: rank, RankWithoutBoost: withoutBoost, ScoreWithoutBoost: &score, Project: "alpha"}}}
		x.journal(e)
		return feedback.ShowsOf(e)[0].Key
	}
	k1 := show(hoursAgo(1), "s1", "prompt", 1, 2, 0.9)
	k2 := show(hoursAgo(2), "s2", "search", 3, 3, 0.5)
	show(hoursAgo(3), "s3", "prompt", 7, 7, 0.1)
	show(hoursAgo(4), "s4", "sources", 1, 1, 0.9)
	k5 := show(hoursAgo(5), "s5", "prompt", 0, 0, 0.0)
	x.settle(hoursAgo(0), map[string]string{k1: feedback.OutcomeCredit, k2: feedback.OutcomeMiss, k5: feedback.OutcomeUnknown})
	x.journal(feedback.Event{TS: ago(10 * time.Hour), Kind: "hide", Session: "s1", IDs: []string{id}})
	x.journal(feedback.Event{TS: ago(9 * time.Hour), Kind: "unhide", Session: "s1", IDs: []string{id}})
	x.server.set(func(d *fakeData) {
		d.Memories = map[string]hindsight.MemoryDetail{id: {ID: id, Type: "observation", State: "active", MentionedAt: "2026-10-01T10:00:00"}}
	})
	q := x.query(7)
	q.ID = id
	r := x.run("memories", q)
	m := r.Tables.Memory
	if m.Kind != "observation" || m.ServerState != "active" || !m.Created.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("server facts: %q %q %v", m.Kind, m.ServerState, m.Created)
	}
	if m.Row.ID != id || m.Row.Shown != 5 || m.Row.Project != "alpha" || m.Row.Used != 1 {
		t.Errorf("row = %+v", m.Row)
	}
	var got []string
	for _, s := range m.Shows {
		got = append(got, fmt.Sprintf("%s %s %s %s", s.At.Format("15:04"), s.Path, s.Outcome, s.Turn))
	}
	want := []string{"08:00 prompt credit t1", "07:00 search confident_miss t1", "06:00 prompt pending t1", "04:00 prompt unknown t1"}
	if !slices.Equal(got, want) {
		t.Errorf("shows = %v, want %v", got, want)
	}
	s0 := m.Shows[0]
	if s0.Rank == nil || *s0.Rank != 1 || s0.RankWithoutBoost == nil || *s0.RankWithoutBoost != 2 || s0.ScoreWithoutBoost == nil || *s0.ScoreWithoutBoost != 0.9 {
		t.Errorf("first show rank/without boost/score = %v/%v/%v, want 1/2/0.9", s0.Rank, s0.RankWithoutBoost, s0.ScoreWithoutBoost)
	}
	if last := m.Shows[3]; last.Rank != nil {
		t.Errorf("an unranked show has rank %v", *last.Rank)
	}
	wantHist := map[string]int64{"1": 1, "2": 0, "3": 1, "4": 0, "5": 0, "6+": 1, "unranked": 1}
	if fmt.Sprint(m.RankHistogram) != fmt.Sprint(wantHist) {
		t.Errorf("rank_histogram = %v, want %v", m.RankHistogram, wantHist)
	}
	if len(m.Retirements) != 2 || m.Retirements[0].Kind != "hide" || !m.Retirements[0].At.Equal(ago(10*time.Hour)) || m.Retirements[1].Kind != "unhide" {
		t.Errorf("retirements = %+v, want hide then unhide", m.Retirements)
	}
	if want := []string{"backsight read " + id, "backsight forget " + id, "backsight forget --restore " + id}; !slices.Equal(m.Commands, want) {
		t.Errorf("commands = %v", m.Commands)
	}
	if *r.Tables.Page != (Page{Limit: 50, Offset: 0, Total: 4}) {
		t.Errorf("page = %+v", *r.Tables.Page)
	}
	q.Limit, q.Offset = 2, 1
	r = x.run("memories", q)
	m = r.Tables.Memory
	if len(m.Shows) != 2 || m.Shows[0].Outcome != "confident_miss" || m.Shows[1].Outcome != "pending" || *r.Tables.Page != (Page{Limit: 2, Offset: 1, Total: 4}) {
		t.Errorf("page 2: %+v %+v", m.Shows, *r.Tables.Page)
	}
	if m.RankHistogram["6+"] != 1 || m.RankHistogram["1"] != 1 {
		t.Errorf("the histogram covers all shows, got %v", m.RankHistogram)
	}
}

// TestMemoryTextOnlyWithText: `memories m1` never carries the memory's text
// (it is nowhere in the JSON); with Text the result holds {id, text} from
// the server; the server is asked once either way; the text is not kept
// (the next plain run has none) and the index does not change.
func TestMemoryTextOnlyWithText(t *testing.T) {
	x := newFx(t, fxNow)
	const secret = "CANARY memory text 7f3a"
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{"m1": {Shown: 1}}})
	x.shows(hoursAgo(1), "s1", "t1", "", "alpha", "prompt", "", "m1")
	x.server.set(func(d *fakeData) {
		d.Memories = map[string]hindsight.MemoryDetail{"m1": {ID: "m1", Text: secret, Type: "world", State: "active"}}
	})
	count := func() map[string]int {
		out := map[string]int{}
		x.source()
		if err := x.ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
			for _, tb := range []string{"shows", "ledger", "turns", "settles", "tel_prompt", "events", "meta"} {
				var n int
				if err := tx.QueryRow("SELECT COUNT(*) FROM " + tb).Scan(&n); err != nil {
					return err
				}
				out[tb] = n
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	memoryCalls := func() int {
		n := 0
		for _, c := range x.server.called() {
			if strings.HasPrefix(c, "Memory?") {
				n++
			}
		}
		return n
	}
	before := count()
	q := x.query(7)
	q.ID = "m1"
	r := x.run("memories", q)
	if r.Text != nil || strings.Contains(string(mustJSON(t, r)), "CANARY") {
		t.Errorf("without Text: text %+v, json holds the canary: %v", r.Text, strings.Contains(string(mustJSON(t, r)), "CANARY"))
	}
	if r.Tables.Memory.Kind != "world" || memoryCalls() != 1 {
		t.Errorf("kind %q after %d memory reads, want world, 1", r.Tables.Memory.Kind, memoryCalls())
	}
	q.Text = true
	r = x.run("memories", q)
	if r.Text == nil || *r.Text != (MemoryText{ID: "m1", Text: secret}) || memoryCalls() != 2 {
		t.Errorf("with Text: %+v after %d memory reads, want {m1 text} and 1 more", r.Text, memoryCalls())
	}
	q.Text = false
	if r = x.run("memories", q); r.Text != nil || strings.Contains(string(mustJSON(t, r)), "CANARY") {
		t.Error("the text came back without being asked for")
	}
	if after := count(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("index counts %v, were %v", after, before)
	}
	q.ID, q.Text = "", true
	if _, err := runErr(x, "memories", q); err == nil {
		t.Error("--text without an id: no error")
	}
}

// TestMemoryPageServerFailure: a server that fails the memory read leaves
// the server facts empty and the page whole; the error text of the server
// is nowhere in the result.
func TestMemoryPageServerFailure(t *testing.T) {
	x := newFx(t, fxNow)
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{"m1": {Shown: 1}}})
	x.server.fail("Memory", &hindsight.APIError{Kind: hindsight.ServerError})
	q := x.query(7)
	q.ID, q.Text = "m1", true
	r := x.run("memories", q)
	if r.Text != nil || r.Tables.Memory.Kind != "" || r.Tables.Memory.Row.ID != "m1" || r.Server == nil || r.Server.OK || r.Server.Error != "server_error" {
		t.Errorf("text %v, kind %q, row %q, server %+v", r.Text, r.Tables.Memory.Kind, r.Tables.Memory.Row.ID, r.Server)
	}
	if strings.Contains(string(mustJSON(t, r)), "fake") { // the fake's body is {"detail":"fake"}
		t.Error("a server error message reached the result")
	}
}

// TestDaysView: a window of the three days 28-30 October has three rows,
// latest day first, each counted by hand; the 29th's page has its six tile
// metrics over that day and its sessions, latest first; a day with no data
// is zeros and a window with no data has zero rows, not an error; the day
// the clocks went back lasts 25 hours.
func TestDaysView(t *testing.T) {
	x := newFx(t, fxNow)
	p := func(ts time.Time, session string, pr telemetry.Prompt) { x.tel(ts, pr, session, "alpha") }
	p(paris(28, 10, 0), "s1", telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Chars: 100, Plen: 80})
	p(paris(28, 10, 5), "s1", telemetry.Prompt{N: 2, Decision: "recall", Empty: "below_floor", Plen: 80})
	x.tel(paris(28, 14, 0), telemetry.Retain{Document: "conversation:s1", Items: 1, Status: "ok", MS: 5}, "s1", "alpha")
	x.tel(paris(28, 15, 0), telemetry.Retain{Document: "conversation:s1#2", Items: 1, Status: "failed", MS: 5}, "s1", "alpha")
	x.declaredTurn(paris(28, 12, 0), "s6", "undeclared", "used", "m3") // a used credit on a turn that ended undeclared: not a used turn
	p(paris(29, 9, 0), "s2", telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Chars: 200, Plen: 80})
	p(paris(29, 9, 30), "s3", telemetry.Prompt{N: 1, Decision: "recall", Reason: "machine", Injected: 1, Chars: 300, Plen: 80})
	x.declaredTurn(paris(29, 10, 0), "s2", "declared", "used", "m1")
	x.declaredTurn(paris(29, 11, 0), "s4", "declared", "wrong", "m2")
	p(paris(30, 8, 0), "s5", telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Chars: 50, Plen: 80})
	p(paris(30, 8, 5), "s5", telemetry.Prompt{N: 2, Decision: "skip", Reason: "too_short", Plen: 3}) // typed, but not a recall
	w, err := NewWindow(fxNow, 0, paris(28, 0, 0), paris(31, 0, 0), fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Window: w, Machines: "this"}
	rows := x.run("days", q).Tables.Days
	want := []DayRow{
		{Day: "2026-10-30", Prompts: 2, Recalls: 1, PromptsWithMemory: 1, ContextChars: 50, Sessions: 1},
		{Day: "2026-10-29", Prompts: 1, Recalls: 2, PromptsWithMemory: 1, UsedTurns: 1, Wrong: 1, ContextChars: 200, Sessions: 3},
		{Day: "2026-10-28", Prompts: 2, Recalls: 2, PromptsWithMemory: 1, ContextChars: 100, Sessions: 2, SentOK: 1, SentFailed: 1},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("days = %+v\nwant   %+v", rows, want)
	}

	q.ID = "2026-10-29"
	r := x.run("days", q)
	if !r.Window.Since.Equal(paris(29, 0, 0)) || !r.Window.Until.Equal(paris(30, 0, 0)) || r.Tables.Day.Day != "2026-10-29" {
		t.Errorf("day window %v..%v, page %q", r.Window.Since, r.Window.Until, r.Tables.Day.Day)
	}
	for _, id := range overviewTiles {
		if _, ok := r.Metrics[id]; !ok {
			t.Errorf("no metric %s on the day page", id)
		}
	}
	if m := r.Metrics["recall.hit_rate"]; num(m) != 2 || den(m) != 2 { // two recalls that day, both injected (one is a machine prompt)
		t.Errorf("recall.hit_rate %v/%v, want 2/2", num(m), den(m))
	}
	if m := r.Metrics["used.turn_rate"]; num(m) != 1 || den(m) != 2 {
		t.Errorf("used.turn_rate %v/%v, want 1/2", num(m), den(m))
	}
	if got := sessionIDs(r.Tables.Day.Sessions); !slices.Equal(got, []string{"s4", "s2", "s3"}) {
		t.Errorf("day sessions = %v, want s4 s2 s3", got)
	}

	q.ID = "2026-01-01"
	r = x.run("days", q)
	if len(r.Tables.Day.Sessions) != 0 || r.Tables.Day.Sessions == nil || den(r.Metrics["recall.hit_rate"]) != 0 {
		t.Errorf("an empty day: %+v", r.Tables.Day)
	}
	q.ID = "2026-10-25" // the clocks went back
	if r = x.run("days", q); r.Window.Until.Sub(r.Window.Since) != 25*time.Hour {
		t.Errorf("2026-10-25 lasts %v, want 25h", r.Window.Until.Sub(r.Window.Since))
	}
	q.ID = "29/10/2026"
	if _, err := runErr(x, "days", q); err == nil {
		t.Error("a bad date: no error")
	}
	q.ID = ""
	q.Window, _ = NewWindow(fxNow, 0, time.Date(2026, 1, 1, 0, 0, 0, 0, fxParis), time.Date(2026, 1, 3, 0, 0, 0, 0, fxParis), fxParis, "Europe/Paris")
	empty := x.run("days", q).Tables.Days
	if len(empty) != 2 || empty[0] != (DayRow{Day: "2026-01-02"}) || empty[1] != (DayRow{Day: "2026-01-01"}) {
		t.Errorf("a window with no data: %+v, want two rows of zeros", empty)
	}
	q.Limit, q.Offset = 1, 1
	if r := x.run("days", q); len(r.Tables.Days) != 1 || r.Tables.Days[0].Day != "2026-01-01" || *r.Tables.Page != (Page{Limit: 1, Offset: 1, Total: 2}) {
		t.Errorf("page of days: %+v %+v", r.Tables.Days, r.Tables.Page)
	}
}

// TestDaysShortWindow: a window of a few hours still lists whole local days
// (the days view is never drawn by the hour), the cut ones counted over
// the hours they cover.
func TestDaysShortWindow(t *testing.T) {
	x := newFx(t, fxNow)
	x.tel(fxNow.Add(-time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Chars: 10, Plen: 80}, "s1", "alpha")
	x.tel(fxNow.Add(-5*time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "s2", "alpha") // before the window
	w, err := NewWindow(fxNow, 0, fxNow.Add(-2*time.Hour), fxNow, fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	rows := x.run("days", Query{Window: w}).Tables.Days
	if len(rows) != 1 || rows[0].Day != "2026-10-30" || rows[0].Prompts != 1 {
		t.Errorf("days = %+v, want the one day with 1 prompt", rows)
	}
}

// relPrompts writes count typed recalls of version ver, one per hour going
// back from start (the first one an hour before it): every injectEvery-th
// one injects (0: all but every fifth), in project alpha (beta for every
// fourth when split), taking ms.
func relPrompts(x *fx, ver string, start time.Time, count int, inject func(i int) bool, split bool, ms int) {
	x.binary = ver
	for i := range count {
		project := "alpha"
		if split && i%4 == 0 {
			project = "beta"
		}
		p := telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, MS: ms}
		if inject(i) {
			p.Injected, p.Chars = 1, 100
		}
		x.tel(start.Add(-time.Duration(i+1)*time.Hour), p, fmt.Sprintf("%s-%d", ver, i), project)
	}
}

// releaseFx has three versions over the same days: 0.1.16 (40 prompts, a
// quarter get memory, 20 declared turns of which 5 use memory and 15 are
// reported wrong), 0.1.17 (50 prompts, four fifths get memory, 25 turns:
// 20 use memory, 5 are reported wrong) and 0.1.15 (3 prompts on 28 October).
// The first turn of 0.1.16 was credited and ended by the 0.1.17 binary.
func releaseFx(t *testing.T) *fx {
	x := newFx(t, fxNow)
	relPrompts(x, "0.1.16", fxNow.Add(-2*time.Hour), 40, func(i int) bool { return i%4 == 0 }, true, 100)
	relPrompts(x, "0.1.17", fxNow.Add(-150*time.Minute), 50, func(i int) bool { return i%5 != 0 }, false, 300)
	x.binary = "0.1.15"
	for i := range 3 {
		x.tel(paris(28, 10+i, 0), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, fmt.Sprintf("old-%d", i), "alpha")
	}
	x.tel(paris(28, 13, 0), telemetry.Prompt{N: 1, Decision: "recall", Reason: "machine", Plen: 80}, "old-machine", "alpha") // not typed: no n
	for i := range 20 {
		s, mem := fmt.Sprintf("s16-%d", i), fmt.Sprintf("m16-%d", i)
		verdict := "wrong"
		if i < 5 {
			verdict = "used"
		}
		x.binary = "0.1.16"
		x.turnEvent(hoursAgo(i).Add(-time.Second), s, "alpha")
		x.shows(hoursAgo(i), s, fxTurn, "", "alpha", "prompt", "", mem)
		if i == 0 {
			x.binary = "0.1.17" // the binary was upgraded mid-turn
		}
		x.credit(hoursAgo(i).Add(30*time.Second), s, "", "alpha", verdict, mem)
		x.stop(hoursAgo(i).Add(time.Minute), s, "", "declared")
	}
	x.binary = "0.1.17"
	for i := range 25 {
		verdict := "used"
		if i >= 20 {
			verdict = "wrong"
		}
		x.declaredTurn(hoursAgo(i), fmt.Sprintf("s17-%d", i), "declared", verdict, fmt.Sprintf("m17-%d", i))
	}
	x.server.set(func(d *fakeData) {
		d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": 900, "experience": 80, "observation": 20}}
		d.Series.Buckets = []hindsight.SeriesBucket{{Time: utcDay(28), World: 5}, {Time: utcDay(29), World: 15, Experience: 5}, {Time: utcDay(30), World: 10}}
	})
	return x
}

func releaseByVersion(r Result) map[string]ReleaseRow {
	out := map[string]ReleaseRow{}
	for _, row := range r.Tables.Releases {
		out[row.Version] = row
	}
	return out
}

// TestReleasesAttributePerEvent: prompts of 0.1.16 and 0.1.17 in the same
// days give each version its own n, hit rate (10 of 40, 40 of 50), latency
// and context counts; the journal's turns, credits and verdicts belong to
// the version of their turn (the turn started by 0.1.16 and credited by
// 0.1.17 is 0.1.16's); the rows are the newest first by first prompt; the
// shares, prompts per active day, span and bank size are as counted by hand.
func TestReleasesAttributePerEvent(t *testing.T) {
	x := releaseFx(t)
	r := x.run("releases", x.query(7))
	if got := []string{r.Tables.Releases[0].Version, r.Tables.Releases[1].Version, r.Tables.Releases[2].Version}; !slices.Equal(got, []string{"0.1.16", "0.1.15", "0.1.17"}) {
		t.Fatalf("order = %v, want by first prompt, newest first", got)
	}
	by := releaseByVersion(r)
	v16, v17, v15 := by["0.1.16"], by["0.1.17"], by["0.1.15"]
	if v16.N != 40 || v17.N != 50 || v15.N != 3 {
		t.Errorf("n = %d %d %d, want 40 50 3", v16.N, v17.N, v15.N)
	}
	type fig struct{ num, den int64 }
	for _, c := range []struct {
		row  ReleaseRow
		want map[string]fig
	}{
		{v16, map[string]fig{"recall.hit_rate": {10, 40}, "used.turn_rate": {5, 20}, "declare.coverage": {20, 20}, "verdict.wrong_rate": {15, 20},
			"context.per_session": {0, 40}, "recall.latency": {0, 40}}},
		{v17, map[string]fig{"recall.hit_rate": {40, 50}, "used.turn_rate": {20, 25}, "declare.coverage": {25, 25}, "verdict.wrong_rate": {5, 25},
			"context.per_session": {0, 50}, "recall.latency": {0, 50}}},
	} {
		if len(c.row.Flagship) != 6 {
			t.Errorf("%s: %d flagship metrics, want 6", c.row.Version, len(c.row.Flagship))
		}
		for id, w := range c.want {
			m := c.row.Flagship[id]
			if den(m) != w.den || (w.num > 0 && num(m) != w.num) {
				t.Errorf("%s %s = %v/%v, want %v/%v", c.row.Version, id, num(m), den(m), w.num, w.den)
			}
			if m.Status == "" || m.Unit == "" {
				t.Errorf("%s %s is not judged: %+v", c.row.Version, id, m)
			}
		}
	}
	if m := v16.Flagship["recall.hit_rate"]; val(m) != 0.25 || m.Status != StatusOK {
		t.Errorf("0.1.16 hit rate = %v %s, want 0.25 ok", val(m), m.Status)
	}
	if m := v17.Flagship["recall.hit_rate"]; val(m) != 0.8 {
		t.Errorf("0.1.17 hit rate = %v", val(m))
	}
	if m := v16.Flagship["verdict.wrong_rate"]; val(m) != 0.75 {
		t.Errorf("0.1.16 wrong rate = %v, want 0.75", val(m))
	}
	if m := v16.Flagship["used.turn_rate"]; m.Previous != nil || m.Delta != nil {
		t.Errorf("a version has no previous window: %+v", m)
	}
	if v16.ProjectShares["alpha"] != 0.75 || v16.ProjectShares["beta"] != 0.25 || len(v16.ProjectShares) != 2 || v17.ProjectShares["alpha"] != 1 || len(v17.ProjectShares) != 1 {
		t.Errorf("shares: %v and %v", v16.ProjectShares, v17.ProjectShares)
	}
	for _, c := range []struct {
		row  ReleaseRow
		want float64
	}{{v16, 40.0 / 3}, {v17, 50.0 / 3}, {v15, 3}} {
		if math.Abs(c.row.PromptsPerDay-c.want) > 1e-9 {
			t.Errorf("%s prompts per day %v, want %v", c.row.Version, c.row.PromptsPerDay, c.want)
		}
	}
	if !v16.First.Equal(fxNow.Add(-42*time.Hour)) || !v16.Last.Equal(fxNow.Add(-3*time.Hour)) {
		t.Errorf("0.1.16 span %v..%v", v16.First, v16.Last)
	}
	for v, want := range map[string]int64{"0.1.16": 1000, "0.1.17": 1000, "0.1.15": 970} { // 970 = 1000 less what was added on the 29th and 30th
		if b := by[v].BankSize; b == nil || *b != want {
			t.Errorf("%s bank size %v, want %d", v, b, want)
		}
	}
	if r.Tables.Note != "observational: weeks, projects and memory differ" {
		t.Errorf("note = %q", r.Tables.Note)
	}
	s := r.Tables.Settings
	if s["recall.session_budget_chars"] != 6000 || s["feedback.boost_weight"] != 0.1 || s["feedback.enabled"] != 1 || s["recall.families"] != 0 {
		t.Errorf("settings = %v", s)
	}
	for _, k := range []string{"recall.briefing", "feedback.stop_check"} {
		if _, ok := s[k]; ok {
			t.Errorf("settings has the string key %s", k)
		}
	}

	q := x.query(7)
	q.Project = "beta"
	by = releaseByVersion(x.run("releases", q))
	if v := by["0.1.16"]; v.N != 10 || den(v.Flagship["recall.hit_rate"]) != 10 || v.BankSize != nil {
		t.Errorf("0.1.16 under --project beta: n %d, den %d, bank %v, want 10, 10, null", v.N, den(v.Flagship["recall.hit_rate"]), v.BankSize)
	}
	if _, ok := by["0.1.17"]; ok {
		t.Error("0.1.17 has no prompt of beta and is listed")
	}
}

// TestReleasesBankSizeReachesNinetyDays: a version whose last prompt is more
// than 90 days old has no bank size; a recent one has.
func TestReleasesBankSizeReachesNinetyDays(t *testing.T) {
	x := newFx(t, fxNow)
	x.binary = "0.1.10"
	x.tel(fxNow.Add(-100*day), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "old", "alpha")
	x.binary = "0.1.18"
	x.tel(fxNow.Add(-2*time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "new", "alpha")
	x.server.set(func(d *fakeData) {
		d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": 50}}
		d.Series.Buckets = []hindsight.SeriesBucket{{Time: utcDay(30), World: 10}}
	})
	by := releaseByVersion(x.run("releases", x.query(120)))
	if b := by["0.1.10"].BankSize; b != nil {
		t.Errorf("0.1.10 bank size %d, want null (100 days ago)", *b)
	}
	if b := by["0.1.18"].BankSize; b == nil || *b != 50 {
		t.Errorf("0.1.18 bank size %v, want 50", b)
	}
	x.server.fail("BankStats", hindsight.ErrUnreachable)
	if b := releaseByVersion(x.run("releases", x.query(120)))["0.1.18"].BankSize; b != nil {
		t.Errorf("bank size %d with the stats unreadable, want null", *b)
	}
}

// TestDefsCarryNAndMDC: a rate's n is the denominator over the last 7 days
// and its mdc the smallest detectable change once n reaches the metric's
// minimum (33 recalls: 0.2413); a rate under its minimum n and every other
// row have no mdc; the server is never asked; an unknown id is not found.
func TestDefsCarryNAndMDC(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "r", fxNow.Add(-2*time.Hour), 33, nil)
	x.tel(fxNow.Add(-10*day), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "older", "alpha") // before the window
	one := func(id string) DefRow {
		q := x.query(7)
		q.ID = id
		r := x.run("defs", q)
		if len(r.Tables.Defs) != 1 {
			t.Fatalf("defs %s: %d rows", id, len(r.Tables.Defs))
		}
		return r.Tables.Defs[0]
	}
	if d := one("recall.hit_rate"); d.N != 33 || d.MDC == nil || math.Abs(*d.MDC-0.2413) > 5e-5 || d.Label == "" || d.Question == "" {
		t.Errorf("recall.hit_rate = n %d mdc %v", d.N, d.MDC)
	}
	if d := one("search.rate"); d.N != 33 || d.MDC == nil {
		t.Errorf("search.rate = n %d mdc %v (33 typed prompts of 40+ characters)", d.N, d.MDC)
	}
	if d := one("used.turn_rate"); d.N != 0 || d.MDC != nil {
		t.Errorf("used.turn_rate = n %d mdc %v, want 0, null", d.N, d.MDC)
	}
	if d := one("bank.size"); d.N != 0 || d.MDC != nil {
		t.Errorf("bank.size = n %d mdc %v, want 0, null", d.N, d.MDC)
	}
	q := x.query(7)
	r := x.run("defs", q)
	if len(r.Tables.Defs) != len(Catalog) || *r.Tables.Page != (Page{Limit: 50, Offset: 0, Total: len(Catalog)}) {
		t.Fatalf("%d rows, page %+v, want the %d catalog rows", len(r.Tables.Defs), r.Tables.Page, len(Catalog))
	}
	for i, d := range r.Tables.Defs {
		if d.ID != Catalog[i].ID {
			t.Errorf("row %d is %s, want the catalog's %s", i, d.ID, Catalog[i].ID)
		}
		if d.Unit != "ratio" && (d.N != 0 || d.MDC != nil) {
			t.Errorf("%s (%s) = n %d mdc %v, want 0 and null", d.ID, d.Unit, d.N, d.MDC)
		}
	}
	if !strings.Contains(string(mustJSON(t, r.Tables.Defs[len(r.Tables.Defs)-1])), `"mdc":null`) {
		t.Error("mdc is not in the JSON of a row without one")
	}
	if !r.Window.Until.Equal(fxNow) || r.Window.Days != 7 {
		t.Errorf("window %v..%v (%d days), want the last 7 days", r.Window.Since, r.Window.Until, r.Window.Days)
	}
	if calls := x.server.called(); len(calls) != 0 {
		t.Errorf("defs called the server: %v", calls)
	}
	q.ID = "nope"
	_, err := runErr(x, "defs", q)
	if nf := notFound(t, err); nf.Kind != "metric" || nf.ID != "nope" {
		t.Errorf("unknown metric: %+v", nf)
	}

	y := newFx(t, fxNow)
	y.prompts("alpha", "r", fxNow.Add(-2*time.Hour), 10, nil)
	q = y.query(7)
	q.ID = "recall.hit_rate"
	if d := y.run("defs", q).Tables.Defs[0]; d.N != 10 || d.MDC != nil {
		t.Errorf("10 recalls: n %d mdc %v, want 10, null (under the minimum n of 20)", d.N, d.MDC)
	}
}

// TestDefsIgnoreTheQueryWindowAndProject: n is for the last 7 days of every
// project whatever window or project the query names.
func TestDefsIgnoreTheQueryWindowAndProject(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "a", fxNow.Add(-2*time.Hour), 25, nil)
	x.prompts("beta", "b", fxNow.Add(-2*time.Hour), 5, nil)
	q := x.query(30)
	q.Project, q.ID = "beta", "recall.hit_rate"
	r := x.run("defs", q)
	if d := r.Tables.Defs[0]; d.N != 30 || r.Window.Days != 7 || r.Scope.Project != "" {
		t.Errorf("n %d over %d days (project %q), want 30 over 7 days, no project", d.N, r.Window.Days, r.Scope.Project)
	}
}

// TestBrowseTiesAreBrokenByName: rows equal on the sort key come in a
// fixed order (the name or id, ascending), whatever order they were written
// in, so a page is the same page twice.
func TestBrowseTiesAreBrokenByName(t *testing.T) {
	x := newFx(t, fxNow)
	same := fxNow.Add(-2 * time.Hour)
	for _, s := range []string{"s-c", "s-a", "s-b"} { // three sessions of three projects, the same instant
		x.tel(same, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, s, "p-"+s)
	}
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{"m-z": {Shown: 4}, "m-x": {Shown: 4}, "m-y": {Shown: 4}}})
	q := x.query(7)
	if got := sessionIDs(x.run("sessions", q).Tables.Sessions); !slices.Equal(got, []string{"s-a", "s-b", "s-c"}) {
		t.Errorf("sessions = %v", got)
	}
	for _, sort := range []string{"last_activity", "sessions", "prompts", "project"} { // all tie
		q.Sort = sort
		if got := projectNames(x.run("projects", q).Tables.Projects); !slices.Equal(got, []string{"p-s-a", "p-s-b", "p-s-c"}) {
			t.Errorf("projects --sort %s = %v", sort, got)
		}
	}
	q.Sort = ""
	for _, sort := range []string{"shown", "used", "rate", "last_shown"} {
		q.Sort = sort
		if got := memoryIDs(x.run("memories", q).Tables.Memories); !slices.Equal(got, []string{"m-x", "m-y", "m-z"}) {
			t.Errorf("memories --sort %s = %v", sort, got)
		}
	}
	q.Sort, q.Limit, q.Offset = "", 2, 2
	r := x.run("sessions", q)
	if got := sessionIDs(r.Tables.Sessions); !slices.Equal(got, []string{"s-c"}) || *r.Tables.Page != (Page{Limit: 2, Offset: 2, Total: 3}) {
		t.Errorf("last page = %v %+v", got, *r.Tables.Page)
	}
	q.Offset = 9 // past the end: no rows, the total still said
	r = x.run("sessions", q)
	if r.Tables.Sessions == nil || len(r.Tables.Sessions) != 0 || r.Tables.Page.Total != 3 {
		t.Errorf("past the end: %v %+v", r.Tables.Sessions, *r.Tables.Page)
	}
	q.Sort = "nope"
	for _, v := range []string{"sessions", "projects", "memories", "days"} {
		if _, err := runErr(x, v, q); err == nil {
			t.Errorf("%s --sort nope: no error", v)
		}
	}
}

// TestSessionsFilterBySessionProject: activity follows the session's
// project: a beta session that showed an alpha memory is beta's.
func TestSessionsFilterBySessionProject(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "a", fxNow.Add(-2*time.Hour), 2, nil)
	x.turnEvent(hoursAgo(1), "b1", "beta")
	x.shows(hoursAgo(1), "b1", fxTurn, "", "beta", "prompt", "alpha", "m1") // an alpha memory shown to a beta session
	q := x.query(7)
	q.Project = "beta"
	r := x.run("sessions", q)
	if got := sessionIDs(r.Tables.Sessions); !slices.Equal(got, []string{"b1"}) || r.Tables.Sessions[0].Project != "beta" {
		t.Errorf("--project beta = %v", got)
	}
	q.Project = "alpha"
	if got := sessionIDs(x.run("sessions", q).Tables.Sessions); !slices.Equal(got, []string{"a-0", "a-1"}) {
		t.Errorf("--project alpha = %v", got)
	}
}

// browseFx is the full fixture plus what the browse pages list: a session
// with a prompt, a turn, shows, a credit, a stop and a search; an alpha
// memory used and a never used one in the ledger, shown with ranks and
// scores, hidden once; a second binary version; attribution rows; and the
// server's detail of the used memory.
func browseFx(t *testing.T) *fx {
	x := fullFx(t)
	const s = "b-sess"
	x.tel(ago(2*time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Injected: 2, Chars: 500, Plen: 80, MS: 100}, s, "alpha")
	x.journal(feedback.Event{TS: ago(2*time.Hour - time.Second), Kind: "turn", Session: s, Turn: "tb", Project: "alpha", N: 1})
	var keys []string
	for i, id := range []string{"pm-used", "pm-never"} {
		sc := 0.9 - 0.1*float64(i)
		e := feedback.Event{TS: ago(2*time.Hour + time.Duration(i)*time.Second), Kind: "shows", Session: s, Turn: "tb", Project: "alpha", Path: "prompt",
			Items: []feedback.Item{{ID: id, Rank: i + 1, RankWithoutBoost: i + 2, ScoreWithoutBoost: &sc, Project: "alpha"}}}
		x.journal(e)
		keys = append(keys, feedback.ShowsOf(e)[0].Key)
	}
	x.journal(feedback.Event{TS: ago(2*time.Hour - 30*time.Second), Kind: "credit", Session: s, Turn: "tb", Project: "alpha", Verdict: "used", IDs: []string{"pm-used"}})
	x.journal(feedback.Event{TS: ago(2*time.Hour - time.Minute), Kind: "stop", Session: s, Turn: "tb", Outcome: "declared"})
	x.settle(ago(time.Hour), map[string]string{keys[0]: feedback.OutcomeCredit, keys[1]: feedback.OutcomeMiss})
	x.toolUse(ago(2*time.Hour-10*time.Second), s, "alpha", 1, memorySearch, false)
	x.journal(feedback.Event{TS: ago(30 * time.Hour), Kind: "hide", Session: s, IDs: []string{"pm-used"}})
	x.tel(ago(5*time.Hour), telemetry.Attribution{Turn: "tb", Base: "alpha", Applied: "alpha", PathRule: "beta"}, s, "alpha")
	x.binary = "0.1.17"
	x.tel(ago(90*time.Minute), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, MS: 100}, "v17-s", "alpha")
	x.binary = fxBinary
	l := staleLedger()
	l.Memories["pm-used"] = feedback.Entry{Shown: 9, Credits: 3, CreditsAt: fxNow, Misses: 1, MissesAt: fxNow, LastShown: ago(time.Hour), LastCredited: ago(2 * time.Hour)}
	l.Memories["pm-never"] = feedback.Entry{Shown: 14, LastShown: ago(3 * time.Hour)}
	x.ledger(l)
	x.server.set(func(d *fakeData) {
		d.Memories = map[string]hindsight.MemoryDetail{"pm-used": {ID: "pm-used", Type: "world", State: "active", MentionedAt: "2026-10-01T10:00:00Z"}}
	})
	return x
}

// browseWidgets is one row per field of the browse pages (spec §8), as
// pageWidgets is for the six pages: view names the page's JSON.
func browseWidgets() []widget {
	var ws []widget
	add := func(r ...widget) { ws = append(ws, r...) }
	const set, present = "set", "present"
	add(rows("projects", "list", "projects", "tables/projects[]/", "project,sessions,prompts,prompts_with_memory,used_turns,reported_wrong,memories_on_server,last_activity,series")...)
	add(rows("projects", "pooled", "projects", "tables/pooled/", "label,totals,min,max")...)
	add(rows("project", "page", "project", "tables/project/", "top_used,never_used,sessions,attribution_disagreements,no_memories,command")...)
	for _, id := range overviewTiles {
		add(widget{"project", "tile " + id, "project", "metrics/" + id + "/status", set, ""})
		add(widget{"day", "tile " + id, "day", "metrics/" + id + "/status", set, ""})
	}
	add(rows("sessions", "list", "sessions", "tables/sessions[]/", "start,project,prompts,prompts_with_memory,shown,used,wrong,context_chars,sent")...)
	add(rows("session", "rail", "session", "tables/session/turns[]/", "n,at,decision,reason,empty,searches,declared,check")...)
	add(rows("session", "chips", "session", "tables/session/turns[]/shows[]/", "memory_id,rank,path,outcome")...)
	add(rows("memories", "list", "memories", "tables/memories[]/", "id,project,shown,used,ignored,rate,boost,last_shown,last_used,state")...)
	add(rows("memory", "page", "memory", "tables/memory/", "kind,server_state,created,retirements,rank_histogram,commands")...)
	add(rows("memory", "strip", "memory", "tables/memory/shows[]/", "at,path,outcome,turn,rank,rank_without_boost,score_without_boost")...)
	add(widget{"day", "sessions", "day", "tables/day/sessions", set, ""})
	add(rows("releases", "list", "releases", "tables/releases[]/", "version,first,last,n,flagship,project_shares,prompts_per_day,bank_size")...)
	add(rows("releases", "settings", "releases", "tables/", "settings,note")...)
	add(rows("defs", "list", "defs", "tables/defs[]/", "id,label,question,definition,unit,min_n,absolute,n")...)
	add(widget{"defs", "list", "defs", "tables/defs[]/mdc", present, "null for a row without a rate or without enough n"})
	for _, v := range []string{"projects", "project", "sessions", "session", "memories", "memory", "day", "releases", "defs"} {
		add(rows(v, "scope", v, "scope/", "machines,server,server_ok")...)
		add(rows(v, "window", v, "window/", "since,until,days,tz")...)
		add(rows(v, "freshness", v, "index/", "ingested_at,lag_seconds,partial")...)
		add(widget{v, "freshness", v, "generated_at", set, ""})
	}
	return ws
}

// TestBrowseWidgets (acceptance): each widget of the browse pages reads a
// field of its view's JSON on the full fixture.
func TestBrowseWidgets(t *testing.T) {
	x := browseFx(t)
	q := func(id string) Query {
		qq := x.query(7)
		qq.ID = id
		return qq
	}
	day := q("2026-10-29")
	pages := map[string]Result{
		"projects": x.run("projects", q("")),
		"project":  x.run("projects", q("alpha")),
		"sessions": x.run("sessions", q("")),
		"session":  x.run("sessions", q("b-sess")),
		"memories": x.run("memories", q("")),
		"memory":   x.run("memories", q("pm-used")),
		"day":      x.run("days", day),
		"releases": x.run("releases", q("")),
		"defs":     x.run("defs", q("")),
	}
	views := map[string]any{}
	for name, r := range pages {
		views[name] = asJSON(t, r)
	}
	for _, w := range browseWidgets() {
		ok := false
		for _, l := range lookup(views[w.view], strings.Split(w.path, "/")) {
			if w.want == "present" || isSet(l) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s / %s: %s %s is not %s", w.page, w.widget, w.view, w.path, w.want)
		}
	}
	// the fields the generic check cannot tell from a default
	pg := pages["project"].Tables.Project
	if pg.AttributionDisagreements != 1 || len(pg.TopUsed) == 0 || pg.TopUsed[0].ID != "pm-used" || len(pg.NeverUsed) == 0 || pg.NeverUsed[0].ID != "pm-never" {
		t.Errorf("project page: %+v", pg)
	}
	if ts := pages["session"].Tables.Session.Turns; len(ts) != 1 || ts[0].Check != "declared" || ts[0].Searches != 1 || len(ts[0].Declared) != 1 || len(ts[0].Shows) != 2 {
		t.Errorf("session page: %+v", ts)
	}
	if m := pages["memory"].Tables.Memory; len(m.Shows) != 1 || m.Shows[0].ScoreWithoutBoost == nil || len(m.Retirements) != 1 || m.Kind != "world" {
		t.Errorf("memory page: %+v", m)
	}
	if len(pages["releases"].Tables.Releases) != 2 {
		t.Errorf("releases: %d versions, want 0.1.18 and 0.1.17", len(pages["releases"].Tables.Releases))
	}
}

// TestBrowseListsAreNeverNull: a view over an empty index lists [] for
// every table it fills, and {} for the maps; nothing it fills is null.
func TestBrowseListsAreNeverNull(t *testing.T) {
	x := newFx(t, fxNow)
	for _, c := range []struct{ view, id string }{{"projects", ""}, {"sessions", ""}, {"memories", ""}, {"days", ""}, {"releases", ""}, {"defs", ""}, {"days", "2026-10-29"}} {
		q := x.query(7)
		q.ID = c.id
		r := x.run(c.view, q)
		for k, v := range asJSON(t, r.Tables).(map[string]any) {
			if v == nil {
				t.Errorf("%s %s: tables.%s is null", c.view, c.id, k)
			}
		}
		if l, ok := asJSON(t, r.Tables).(map[string]any)[c.view]; ok && l == nil {
			t.Errorf("%s: the list is null", c.view)
		}
	}
	// the project, session and memory pages of an index that has them but nothing in them
	x.prompts("alpha", "a", fxNow.Add(-2*time.Hour), 1, nil)
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{"m1": {}}})
	for _, c := range []struct{ view, id string }{{"projects", "alpha"}, {"sessions", "a-0"}, {"memories", "m1"}} {
		q := x.query(7)
		q.ID = c.id
		raw := string(mustJSON(t, x.run(c.view, q).Tables))
		// a time that was never set is the zero time, a pointer that was never set is null by design
		for _, bad := range []string{`":null`} {
			for _, field := range []string{"top_used", "never_used", "sessions", "turns", "shows", "retirements", "commands", "rank_histogram", "declared"} {
				if strings.Contains(raw, `"`+field+`"`+bad) {
					t.Errorf("%s %s: %s is null in %s", c.view, c.id, field, raw)
				}
			}
		}
	}
}

// TestBrowseServerReads: each browse view asks the server only for what it
// shows, and never runs the doctor.
func TestBrowseServerReads(t *testing.T) {
	methods := func(x *fx) []string {
		var out []string
		for _, c := range x.server.called() {
			m, _, _ := strings.Cut(c, "?")
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
		slices.Sort(out)
		return out
	}
	for _, c := range []struct {
		view, id, project string
		want              []string
	}{
		{"projects", "", "", []string{"ProjectTags"}},
		{"projects", "alpha", "", []string{"ProjectTags"}},
		{"sessions", "", "", nil},
		{"sessions", "a-0", "", nil},
		{"memories", "", "", nil},
		{"memories", "m1", "", []string{"Memory"}},
		{"days", "", "", nil},
		{"days", "2026-10-29", "", nil},
		{"releases", "", "", []string{"BankStats", "MemoriesTimeseries"}},
		{"releases", "", "alpha", nil},
		{"defs", "", "", nil},
	} {
		x := newFx(t, fxNow)
		x.prompts("alpha", "a", fxNow.Add(-2*time.Hour), 1, nil)
		x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{"m1": {}}})
		src := x.source()
		src.Doctor = func(context.Context) ([]DoctorCheck, error) {
			t.Errorf("%s ran the doctor", c.view)
			return nil, nil
		}
		q := x.query(7)
		q.ID, q.Project = c.id, c.project
		if _, err := Run(context.Background(), c.view, src, q); err != nil {
			t.Fatalf("%s %s: %v", c.view, c.id, err)
		}
		if got := methods(x); !slices.Equal(got, c.want) {
			t.Errorf("%s %q (project %q) read %v, want %v", c.view, c.id, c.project, got, c.want)
		}
	}
}

// TestBrowseOneTransaction: a browse view reads the index in one
// transaction; a memory page, whose id is resolved before its one server
// read, in two.
func TestBrowseOneTransaction(t *testing.T) {
	x := browseFx(t)
	for _, c := range []struct {
		view, id string
		want     int32
	}{
		{"projects", "", 1}, {"projects", "alpha", 1}, {"sessions", "", 1}, {"sessions", "b-sess", 1}, {"memories", "", 1},
		{"memories", "pm-used", 2}, {"days", "", 1}, {"days", "2026-10-29", 1}, {"releases", "", 1}, {"defs", "", 1},
	} {
		src := x.source()
		cr := &countingReader{Reader: src.Index}
		src.Index = cr
		q := x.query(7)
		q.ID = c.id
		if _, err := Run(context.Background(), c.view, src, q); err != nil {
			t.Fatalf("%s %s: %v", c.view, c.id, err)
		}
		if n := cr.n.Load(); n != c.want {
			t.Errorf("%s %q: %d read transactions, want %d", c.view, c.id, n, c.want)
		}
	}
}

// TestBrowseNeverPrintsFreeText: memory text, server error messages and the
// prompts' content are nowhere in the JSON of any browse view, with or
// without a failing server.
func TestBrowseNeverPrintsFreeText(t *testing.T) {
	x := browseFx(t)
	const canary = "CANARY-FREE-TEXT"
	x.server.set(func(d *fakeData) {
		d.Memories = map[string]hindsight.MemoryDetail{"pm-used": {ID: "pm-used", Text: canary, Type: "world", State: "active", Tags: []string{canary}}}
		d.Tags = append(d.Tags, hindsight.TagCount{Tag: "project:" + canary, Count: 1})
	})
	for _, failing := range []bool{false, true} {
		if failing {
			x.server.failAll(&hindsight.APIError{Kind: hindsight.ServerError}) // its body, {"detail":"fake"}, is the server's message
		}
		for _, c := range []struct{ view, id string }{{"projects", ""}, {"projects", "alpha"}, {"sessions", ""}, {"sessions", "b-sess"},
			{"memories", ""}, {"memories", "pm-used"}, {"days", ""}, {"days", "2026-10-29"}, {"releases", ""}, {"defs", ""}} {
			q := x.query(7)
			q.ID = c.id
			r := x.run(c.view, q)
			raw := string(mustJSON(t, r))
			if strings.Contains(raw, canary) || strings.Contains(raw, "fake") {
				t.Errorf("%s %q (failing server %v) holds the memory text or the server's message", c.view, c.id, failing)
			}
		}
	}
}

// TestTurnRailDuplicateN: two prompts that carry the same n share one
// journal turn: its shows are listed once, on the first.
func TestTurnRailDuplicateN(t *testing.T) {
	x := newFx(t, fxNow)
	const s = "dup"
	x.tel(hoursAgo(5), telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Plen: 80}, s, "alpha")
	x.tel(hoursAgo(4), telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Plen: 80}, s, "alpha")
	x.turnEvent(hoursAgo(5).Add(-time.Second), s, "alpha")
	x.shows(hoursAgo(5), s, fxTurn, "", "alpha", "prompt", "", "m1")
	q := x.query(7)
	q.ID = s
	turns := x.run("sessions", q).Tables.Session.Turns
	if len(turns) != 2 || len(turns[0].Shows) != 1 || turns[0].Turn != "t1" || len(turns[1].Shows) != 0 || turns[1].Turn != "" {
		t.Errorf("turns = %+v, want the shows and the turn on the first prompt only", turns)
	}
}

// TestNoZeroTimes: a time that was never set is null, never year 1: a memory
// never used or shown, a page whose server answered nothing, a review row,
// and the pooled row of no project.
func TestNoZeroTimes(t *testing.T) {
	x := newFx(t, fxNow)
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{"m-never": {Shown: 12}, "imp": {Imported: feedback.Imported{Shown: 3}, Shown: 3}}})
	x.server.fail("Memory", hindsight.ErrUnreachable)
	q := x.query(7)
	rows := map[string]MemoryRow{}
	for _, m := range x.run("memories", q).Tables.Memories {
		rows[m.ID] = m
	}
	if m := rows["m-never"]; m.LastUsed != nil || m.LastShown != nil {
		t.Errorf("m-never = %+v, want null times", m)
	}
	q.ID = "m-never"
	r := x.run("memories", q)
	if r.Tables.Memory.Created != nil {
		t.Errorf("created = %v with the server down, want null", r.Tables.Memory.Created)
	}
	rv := x.run("bank", x.query(7)).Tables.Review
	if len(rv.NeverUsed) == 0 || rv.NeverUsed[0].LastShown != nil {
		t.Errorf("review = %+v, want a never-used row with a null last_shown", rv.NeverUsed)
	}
	pooled := x.run("projects", x.query(7)).Tables.Pooled
	if pooled.Totals.LastActivity != nil {
		t.Errorf("pooled last activity %v with no project", pooled.Totals.LastActivity)
	}
	for _, v := range []Result{r, x.run("memories", x.query(7)), x.run("bank", x.query(7)), x.run("projects", x.query(7))} {
		if strings.Contains(string(mustJSON(t, v)), "0001-01-01") {
			t.Errorf("%s holds a zero time: %s", v.View, mustJSON(t, v))
		}
	}
}

// TestHiddenFromTheMomentOfHiding: a memory hidden a day ago is hidden for
// --state, ahead of its other states; it is not yet in the review queue
// (memory stale waits 7 days), which keeps its own counts.
func TestHiddenFromTheMomentOfHiding(t *testing.T) {
	x := newFx(t, fxNow)
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{
		"m-new":  {Shown: 20, Hidden: &feedback.Hidden{Since: ago(day)}}, // would also be never used
		"m-old":  {Shown: 3, Hidden: &feedback.Hidden{Since: ago(10 * day)}},
		"m-free": {Shown: 3},
	}})
	q := x.query(7)
	q.State = "hidden"
	r := x.run("memories", q)
	if got := memoryIDs(r.Tables.Memories); !slices.Equal(got, []string{"m-new", "m-old"}) {
		t.Errorf("--state hidden = %v", got)
	}
	if h := r.Metrics["stale.hidden"]; h.Value == nil || *h.Value != 1 || r.Metrics["stale.never_used"].Value == nil || *r.Metrics["stale.never_used"].Value != 0 {
		t.Errorf("stale counts hidden %v never_used %v, want 1 and 0 (the queue waits 7 days)", h.Value, r.Metrics["stale.never_used"].Value)
	}
	q.State, q.ID = "", "m-new"
	if st := x.run("memories", q).Tables.Memory.Row.State; st != "hidden" {
		t.Errorf("page state %q, want hidden", st)
	}
}

// TestSentIsOneDefinition: the overview's lately and the sessions view read
// the same sent for the same session, from its documents.
func TestSentIsOneDefinition(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "s", fxNow.Add(-2*time.Hour), 1, nil)
	x.tel(fxNow.Add(-5*time.Hour), telemetry.Retain{Document: "conversation:s-0", Items: 1, Status: "ok", MS: 1}, "other", "alpha")
	x.tel(fxNow.Add(-4*time.Hour), telemetry.Retain{Document: "conversation:s-0#2", Items: 1, Status: "queued", MS: 1}, "other", "alpha")
	x.tel(fxNow.Add(-3*time.Hour), telemetry.Retain{Document: "note:x", Items: 1, Status: "ok", MS: 1}, "s-0", "alpha") // the session's event, not its document
	lately := x.run("overview", x.query(7)).Tables.Lately
	sessions := x.run("sessions", x.query(7)).Tables.Sessions
	if len(lately) != 1 || len(sessions) != 1 || lately[0].Sent != "queued" || sessions[0].Sent != "queued" {
		t.Errorf("lately %+v, sessions %+v, want queued in both", lately, sessions)
	}
}

// TestSortSessionsTies: equal activity times come ordered by id whatever the
// input order.
func TestSortSessionsTies(t *testing.T) {
	same := fxNow
	rows := []SessionRow{{ID: "c", Last: same}, {ID: "a", Last: same}, {ID: "z", Last: same.Add(time.Hour)}, {ID: "b", Last: same}}
	sortSessions(rows)
	if got := sessionIDs(rows); !slices.Equal(got, []string{"z", "a", "b", "c"}) {
		t.Errorf("sorted = %v", got)
	}
}

// TestRunKeyHoldsTheVersion: the same family and window asked under two
// binary versions in one env are two runs, not one served twice.
func TestRunKeyHoldsTheVersion(t *testing.T) {
	x := releaseFx(t)
	var a, b int64
	err := withTx(context.Background(), x.source(), x.query(7), nil, func(e env) error {
		fam := families.byID["recall.hit_rate"]
		for v, into := range map[string]*int64{"0.1.16": &a, "0.1.17": &b} {
			ve := e
			ve.q.version = v
			obs, _, err := ve.run(fam, e.q.Window) // the same memo
			if err != nil {
				return err
			}
			*into = obs["recall.hit_rate"].Den
		}
		return nil
	})
	if err != nil || a != 40 || b != 50 {
		t.Errorf("dens %d %d (%v), want 40 and 50", a, b, err)
	}
}

// TestQualityStepsServeSubsets: a quality id asked alone runs only its step;
// a subset is served from an earlier full run (one run, not two); an id of a
// skipped step is never invented.
func TestQualityStepsServeSubsets(t *testing.T) {
	x := newFx(t, fxNow)
	x.declaredTurn(hoursAgo(1), "s1", "declared", "used", "m1")
	err := withTx(context.Background(), x.source(), x.query(7), nil, func(e env) error {
		fam := families.byID["used.turn_rate"]
		obs, _, err := e.runIDs(fam, e.q.Window, []string{"used.turn_rate"})
		if err != nil {
			return err
		}
		if _, ok := obs["used.rank"]; ok {
			t.Error("a step nobody asked for was run")
		}
		if obs["used.turn_rate"].Den != 1 {
			t.Errorf("used.turn_rate den %d", obs["used.turn_rate"].Den)
		}
		full, _, err := e.run(fam, e.q.Window)
		if err != nil {
			return err
		}
		if _, ok := full["used.rank"]; !ok {
			t.Error("a full run lacks used.rank")
		}
		sub, _, err := e.runIDs(fam, e.q.Window, []string{"verdict.wrong_rate"})
		if err != nil {
			return err
		}
		if _, ok := sub["used.rank"]; !ok { // served from the full run
			t.Error("the subset was recomputed instead of served from the full run")
		}
		if !reflect.DeepEqual(obs["used.turn_rate"], full["used.turn_rate"]) {
			t.Errorf("alone %+v, in the full run %+v", obs["used.turn_rate"], full["used.turn_rate"])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestWorstSentNamesAnEmptyStatus: a document with no status is unknown,
// never an empty string that reads as "nothing to report".
func TestWorstSentNamesAnEmptyStatus(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{{[]string{"ok", ""}, "unknown"}, {[]string{""}, "unknown"}, {[]string{"ok", "", "failed"}, "failed"}, {[]string{"ok"}, "ok"}} {
		if got := worstSent(slices.Clone(c.in)); got != c.want {
			t.Errorf("worstSent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReleaseSharesNameTheMissingProject: prompts with no project are
// counted under the key "none", never under an empty JSON key.
func TestReleaseSharesNameTheMissingProject(t *testing.T) {
	x := newFx(t, fxNow)
	x.binary = "0.1.16"
	x.tel(fxNow.Add(-time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "a-1", "alpha")
	x.tel(fxNow.Add(-2*time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "n-1", "")
	r := x.run("releases", x.query(7))
	if len(r.Tables.Releases) != 1 {
		t.Fatalf("rows: %+v", r.Tables.Releases)
	}
	got := r.Tables.Releases[0].ProjectShares
	if _, bad := got[""]; bad || got[NoProject] != 0.5 || got["alpha"] != 0.5 || len(got) != 2 {
		t.Errorf("shares = %v, want alpha and none at 0.5", got)
	}
}
