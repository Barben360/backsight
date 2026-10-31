package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
	"github.com/Barben360/backsight/internal/views"
)

// viewsFixture is fixedStatsFixture (session s1, memory m1, the fake server
// up) plus three more sessions (s2, sess-long-1 and sess-long-2, two ids
// sharing a prefix of 8 characters) and two memories with long ids.
func viewsFixture(t *testing.T) {
	t.Helper()
	fixedStatsFixture(t)
	statsDown.Store(false)
	d, _, err := machine()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i, s := range []string{"s2", "sess-long-1", "sess-long-2"} {
		at := goldenNow.Add(-time.Duration(10+i) * time.Hour)
		b.WriteString(telLineFor(t, at, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, MS: 500, Injected: 1}, s, "p1"))
	}
	f, err := os.OpenFile(filepath.Join(d.TelemetryDir(), "events-2026-10.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	home, _ := os.UserHomeDir()
	at := func(h int) time.Time { return goldenNow.Add(-time.Duration(h) * time.Hour) }
	journalIn(t, home,
		feedback.Event{Kind: "shows", Path: "prompt", TS: at(9), Items: []feedback.Item{{ID: "mem-long-1", Rank: 1}, {ID: "mem-long-2", Rank: 2}}},
	)
}

// viewOut runs args with --json and decodes the envelope.
type viewOut struct {
	code        int
	raw, stderr string
	env         struct {
		OK      bool           `json:"ok"`
		Command string         `json:"command"`
		Data    map[string]any `json:"data"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
}

func runView(t *testing.T, args ...string) viewOut {
	t.Helper()
	var v viewOut
	v.code, v.raw, v.stderr = run(t, append(args, "--json")...)
	if err := json.Unmarshal([]byte(v.raw), &v.env); err != nil {
		t.Fatalf("%v: not an envelope (exit %d): %q %q", args, v.code, v.raw, v.stderr)
	}
	return v
}

// at reads a path of keys in decoded JSON ("" when absent).
func at(m any, keys ...string) any {
	for _, k := range keys {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}

func TestStatsViewsRun(t *testing.T) {
	viewsFixture(t)
	for _, view := range metrics.Views {
		t.Run(view, func(t *testing.T) {
			v := runView(t, "stats", view)
			if v.code != output.OK || !v.env.OK || v.env.Command != "stats "+view {
				t.Fatalf("exit %d command %q: %s %s", v.code, v.env.Command, v.raw, v.stderr)
			}
			if got := at(v.env.Data, "view"); got != view {
				t.Errorf("data.view = %v", got)
			}
			if got := at(v.env.Data, "view_schema"); got != float64(1) {
				t.Errorf("data.view_schema = %v", got)
			}
		})
	}
	for _, args := range [][]string{
		{"stats", "sessions", "s1"}, {"stats", "memories", "m1"}, {"stats", "days", "2026-10-08"},
		{"stats", "defs", "recall.hit_rate"}, {"stats", "projects", "p1"},
	} {
		v := runView(t, args...)
		if v.code != output.OK {
			t.Errorf("%v: exit %d: %s", args, v.code, v.raw)
		}
	}
	// the page of one session names it
	v := runView(t, "stats", "sessions", "s1")
	if at(v.env.Data, "tables", "session") == nil {
		t.Errorf("no session page: %s", v.raw)
	}
}

func TestStatsLegacyUnchanged(t *testing.T) {
	fixedStatsFixture(t)
	for name, args := range map[string][]string{
		"stats-14-json.golden": {"stats", "--days", "14", "--json"},
		"stats-14.golden":      {"stats", "--days", "14"},
	} {
		code, out, errOut := run(t, args...)
		if code != output.OK {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
		cliGolden(t, name, []byte(goldenPort.ReplaceAllString(out, "127.0.0.1:PORT")))
	}
}

func TestStatsUnknownView(t *testing.T) {
	fixedStatsFixture(t)
	code, out, errOut := run(t, "stats", "foo")
	want := `unknown view "foo": one of overview, quality, cost, bank, pipeline, system, releases, projects, sessions, memories, days, defs, index`
	if code != output.Usage || !strings.Contains(errOut, want+"\n") || out != "" {
		t.Errorf("stats foo: exit %d %q %q", code, out, errOut)
	}
	code, _, errOut = run(t, "stats", "0")
	if code != output.Usage || !strings.Contains(errOut, "days: a whole number from 1 to 365") {
		t.Errorf("stats 0: exit %d %q", code, errOut)
	}
	// --days alone is `stats N`, with the same range
	if code, _, errOut := run(t, "stats", "--days", "0"); code != output.Usage || !strings.Contains(errOut, "days: a whole number from 1 to 365") {
		t.Errorf("stats --days 0: exit %d %q", code, errOut)
	}
	if code, _, _ := run(t, "stats", "7", "--days", "7"); code != output.Usage {
		t.Errorf("stats 7 --days 7: exit %d", code)
	}
}

func TestPersistentFlags(t *testing.T) {
	viewsFixture(t)
	v := runView(t, "stats", "overview", "--tz", "Europe/Paris", "--since", "2026-10-01", "--until", "2026-10-08")
	if v.code != output.OK || at(v.env.Data, "window", "since") != "2026-10-01T00:00:00+02:00" || at(v.env.Data, "window", "tz") != "Europe/Paris" {
		t.Errorf("--since: exit %d window %v", v.code, at(v.env.Data, "window"))
	}
	// without --tz the window is read in this machine's zone and names it
	v = runView(t, "stats", "overview", "--since", "2026-10-01")
	local := metrics.ZoneName(time.Local, os.Getenv, os.Readlink)
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local).Format(time.RFC3339); v.code != output.OK || at(v.env.Data, "window", "since") != want || at(v.env.Data, "window", "tz") != local {
		t.Errorf("local zone: exit %d window %v, want %s in %s", v.code, at(v.env.Data, "window"), want, local)
	}
	v = runView(t, "stats", "overview", "--tz", "UTC", "--since", "2026-10-01")
	if at(v.env.Data, "window", "tz") != "UTC" || at(v.env.Data, "window", "since") != "2026-10-01T00:00:00Z" {
		t.Errorf("--tz UTC: %v", at(v.env.Data, "window"))
	}
	v = runView(t, "stats", "sessions", "--limit", "2")
	rows, _ := at(v.env.Data, "tables", "sessions").([]any)
	if len(rows) != 2 || at(v.env.Data, "tables", "page", "total") != float64(4) {
		t.Errorf("--limit 2: %d rows, page %v", len(rows), at(v.env.Data, "tables", "page"))
	}
	v = runView(t, "stats", "sessions", "--limit", "2", "--offset", "3")
	if rows, _ := at(v.env.Data, "tables", "sessions").([]any); len(rows) != 1 {
		t.Errorf("--offset 3: %d rows, want the last one", len(rows))
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"stats", "--project", "x"}, "--project applies to a view, e.g. backsight stats overview --project …"},
		{[]string{"stats", "14", "--tz", "UTC"}, "--tz applies to a view"},
		{[]string{"stats", "--dev"}, "--dev applies to a view"},
		{[]string{"stats", "--path", "prompt"}, "--path applies to a view"},
		{[]string{"stats", "--limit", "3"}, "--limit applies to a view"},
		{[]string{"stats", "--offset", "3"}, "--offset applies to a view"},
		{[]string{"stats", "index", "--days", "3"}, "--days applies to a view"},
		{[]string{"stats", "index", "rebuild", "--plan", "--project", "x"}, "--project applies to a view"},
		{[]string{"stats", "index", "migrate", "--since", "2026-10-01"}, "--since applies to a view"},
		{[]string{"stats", "overview", "--limit", "501"}, "--limit: a whole number from 1 to 500"},
		{[]string{"stats", "overview", "--limit", "0"}, "--limit: a whole number from 1 to 500"},
		{[]string{"stats", "overview", "--offset", "-1"}, "--offset: a whole number, 0 or more"},
		{[]string{"stats", "overview", "--days", "0"}, "--days: a whole number from 1 to 365"},
		{[]string{"stats", "overview", "--days", "366"}, "--days: a whole number from 1 to 365"},
		{[]string{"stats", "overview", "--days", "3", "--until", "2026-10-08"}, "give either --days or --since/--until"},
		{[]string{"stats", "overview", "--days", "3", "--since", "2026-10-01"}, "give either --days or --since/--until"},
		{[]string{"stats", "overview", "--until", "2026-10-08"}, "--until needs --since"},
		{[]string{"stats", "overview", "--since", "2026-10-08", "--until", "2026-10-01"}, "--since must be before --until"},
		{[]string{"stats", "overview", "--since", "soon"}, "--since:"},
		{[]string{"stats", "overview", "--tz", "Mars/Base"}, "--tz: unknown zone"},
		{[]string{"stats", "sessions", "--has", "nope"}, "--has: one of used, wrong, undeclared, memory"},
		{[]string{"stats", "memories", "--sort", "nope"}, "--sort: one of shown, used, rate, last_shown"},
		{[]string{"stats", "memories", "--state", "nope"}, "--state: one of hidden, wrong, never_used"},
		{[]string{"stats", "memories", "--text"}, "--text needs a memory id"},
		{[]string{"stats", "days", "yesterday"}, "a day is written YYYY-MM-DD"},
		{[]string{"stats", "overview", "extra"}, ""},
		{[]string{"stats", "overview", "--has", "used"}, ""},
	} {
		code, out, errOut := run(t, c.args...)
		if code != output.Usage || !strings.Contains(errOut, c.want) || out != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q, want exit 2 with %q", c.args, code, out, errOut, c.want)
		}
	}
	// the error as JSON follows the envelope
	v = runView(t, "stats", "overview", "--limit", "501")
	if v.code != output.Usage || v.env.OK || v.env.Command != "stats overview" || v.env.Error.Code != output.Usage {
		t.Errorf("error envelope: exit %d %+v", v.code, v.env)
	}
}

// The mapping of flags to the Query a view runs.
func TestViewQuery(t *testing.T) {
	nowT := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	base := viewFlags{limit: 50, tz: "Europe/Paris"}
	with := func(edit func(*viewFlags)) viewFlags {
		f := base
		edit(&f)
		return f
	}
	cases := []struct {
		name  string
		f     viewFlags
		id    string
		check func(t *testing.T, q metrics.Query)
	}{
		{"defaults: 7 days, page of 50", base, "", func(t *testing.T, q metrics.Query) {
			if q.Window.Days != 7 || !q.Window.Until.Equal(nowT) || q.Window.TZ != "Europe/Paris" || q.Limit != 50 || q.Offset != 0 {
				t.Errorf("%+v", q)
			}
		}},
		{"days", with(func(f *viewFlags) { f.days, f.daysSet = 30, true }), "", func(t *testing.T, q metrics.Query) {
			if q.Window.Days != 30 {
				t.Errorf("%+v", q.Window)
			}
		}},
		{"since alone ends now", with(func(f *viewFlags) { f.since = "2026-10-05" }), "", func(t *testing.T, q metrics.Query) {
			if q.Window.Since.Format(time.RFC3339) != "2026-10-05T00:00:00+02:00" || !q.Window.Until.Equal(nowT) {
				t.Errorf("%+v", q.Window)
			}
		}},
		{"a date is midnight in --tz", with(func(f *viewFlags) { f.since, f.until, f.tz = "2026-10-05", "2026-10-06", "UTC" }), "", func(t *testing.T, q metrics.Query) {
			if q.Window.Since.Format(time.RFC3339) != "2026-10-05T00:00:00Z" || q.Window.Days != 1 || q.Window.TZ != "UTC" {
				t.Errorf("%+v", q.Window)
			}
		}},
		{"an explicit zone in the time wins", with(func(f *viewFlags) { f.since, f.until = "2026-10-05T10:00:00Z", "2026-10-06T10:00:00Z" }), "", func(t *testing.T, q metrics.Query) {
			if !q.Window.Since.Equal(time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)) {
				t.Errorf("%+v", q.Window)
			}
		}},
		{"filters and paging", with(func(f *viewFlags) {
			f.project, f.path, f.dev, f.limit, f.offset = "p1", "prompt", true, 10, 20
		}), "", func(t *testing.T, q metrics.Query) {
			if q.Project != "p1" || q.Path != "prompt" || !q.Dev || q.Limit != 10 || q.Offset != 20 {
				t.Errorf("%+v", q)
			}
		}},
		{"session filters", with(func(f *viewFlags) { f.has = "wrong" }), "s1", func(t *testing.T, q metrics.Query) {
			if q.Has != "wrong" || q.ID != "s1" {
				t.Errorf("%+v", q)
			}
		}},
		{"memory options", with(func(f *viewFlags) { f.sort, f.state, f.text = "rate", "hidden", true }), "m1", func(t *testing.T, q metrics.Query) {
			if q.Sort != "rate" || q.State != "hidden" || !q.Text || q.ID != "m1" {
				t.Errorf("%+v", q)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := viewQuery(c.f, c.id, nowT)
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, q)
		})
	}
	if _, err := viewQuery(with(func(f *viewFlags) { f.daysSet = true }), "", nowT); err == nil {
		t.Error("--days 0 was accepted")
	}
}

func TestViewErrorMessages(t *testing.T) {
	for _, c := range []struct {
		err  metrics.NotFoundError
		want string
	}{
		{metrics.NotFoundError{Kind: "session", ID: "nosuchsession"}, `no session "nosuchsession" in the metrics index`},
		{metrics.NotFoundError{Kind: "memory", ID: "nosuchmemory"}, `no memory "nosuchmemory" in the metrics index`},
		{metrics.NotFoundError{Kind: "session", ID: "ab", TooShort: true}, "session id: give at least 8 characters"},
		{metrics.NotFoundError{Kind: "memory", ID: "ab", TooShort: true}, "memory id: give at least 8 characters"},
		{metrics.NotFoundError{Kind: "session", ID: "abcdefgh", Matches: []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7"}}, `ambiguous session id "abcdefgh": a1, a2, a3, a4, a5`},
		{metrics.NotFoundError{Kind: "memory", ID: "abcdefgh", Matches: []string{"b1", "b2"}}, `ambiguous memory id "abcdefgh": b1, b2`},
		{metrics.NotFoundError{Kind: "project", ID: "nowhere"}, `no project "nowhere" in the window`},
		{metrics.NotFoundError{Kind: "metric", ID: "x.y"}, `no metric "x.y": backsight stats defs lists them`},
	} {
		err := viewError(c.err)
		var oe *output.Error
		if !asOutputError(err, &oe) || oe.Code != output.Usage || oe.Message != c.want {
			t.Errorf("%+v: %v, want exit 2 %q", c.err, err, c.want)
		}
	}
}

func asOutputError(err error, oe **output.Error) bool { return errors.As(err, oe) }

func TestSessionIDMessages(t *testing.T) {
	viewsFixture(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"stats", "sessions", "nosuch-session-id"}, `no session "nosuch-session-id" in the metrics index`},
		{[]string{"stats", "sessions", "sess"}, "session id: give at least 8 characters"},
		{[]string{"stats", "sessions", "sess-lon"}, `ambiguous session id "sess-lon": sess-long-1, sess-long-2`},
		{[]string{"stats", "memories", "nosuch-memory-id"}, `no memory "nosuch-memory-id" in the metrics index`},
		{[]string{"stats", "memories", "mem"}, "memory id: give at least 8 characters"},
		{[]string{"stats", "memories", "mem-long"}, `ambiguous memory id "mem-long": mem-long-1, mem-long-2`},
		{[]string{"stats", "projects", "nowhere"}, `no project "nowhere" in the window`},
		{[]string{"stats", "defs", "no.such"}, `no metric "no.such": backsight stats defs lists them`},
	} {
		code, out, errOut := run(t, c.args...)
		if code != output.Usage || !strings.Contains(errOut, c.want+"\n") || out != "" {
			t.Errorf("%v: exit %d %q %q, want exit 2 %q", c.args, code, out, errOut, c.want)
		}
	}
	// a unique prefix names the session
	if v := runView(t, "stats", "sessions", "sess-long-1"); v.code != output.OK {
		t.Errorf("exact id: exit %d", v.code)
	}
}

func TestViewsWithoutServerOrFeedback(t *testing.T) {
	viewsFixture(t)
	d, _, _ := machine()
	cfg, err := os.ReadFile(d.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("no config", func(t *testing.T) {
		if err := os.Remove(d.ConfigFile()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(d.ConfigFile(), cfg, 0o600) })
		for _, view := range metrics.Views {
			v := runView(t, "stats", view)
			if v.code != output.OK {
				t.Errorf("%s: exit %d: %s", view, v.code, v.raw)
				continue
			}
			if view == "overview" || view == "bank" || view == "pipeline" || view == "system" || view == "cost" {
				srv, _ := at(v.env.Data, "server").(map[string]any)
				if srv["ok"] != false || srv["error"] != "not_configured" {
					t.Errorf("%s: server = %v", view, srv)
				}
				if _, has := srv["last_ok"]; !has || srv["last_ok"] != nil {
					t.Errorf("%s: last_ok = %v, want null", view, srv["last_ok"])
				}
			}
		}
	})
	t.Run("server-only host", func(t *testing.T) {
		if err := os.WriteFile(d.ConfigFile(), append(append([]byte{}, cfg...), []byte("role = \"server\"\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(d.ConfigFile(), cfg, 0o600) })
		v := runView(t, "stats", "overview")
		if v.code != output.OK {
			t.Fatalf("exit %d: %s", v.code, v.raw)
		}
		if at(v.env.Data, "metrics", "recall.hit_rate", "status") != "no_data" || at(v.env.Data, "metrics", "recall.hit_rate", "reason") != "server_only_host" {
			t.Errorf("hit rate: %v", at(v.env.Data, "metrics", "recall.hit_rate"))
		}
		for _, axis := range []string{"found", "right"} {
			if got := at(v.env.Data, "verdict", "axes", axis, "state"); got != "too_early" {
				t.Errorf("axis %s = %v, want too_early", axis, got)
			}
		}
	})
	t.Run("feedback off", func(t *testing.T) {
		if err := os.WriteFile(d.ConfigFile(), append(append([]byte{}, cfg...), []byte("\n[feedback]\nenabled = false\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(d.ConfigFile(), cfg, 0o600) })
		v := runView(t, "stats", "quality")
		if v.code != output.OK {
			t.Fatalf("exit %d: %s", v.code, v.raw)
		}
		if at(v.env.Data, "metrics", "used.turn_rate", "status") != "no_data" || at(v.env.Data, "metrics", "used.turn_rate", "reason") != "feedback_off" {
			t.Errorf("turn rate: %v", at(v.env.Data, "metrics", "used.turn_rate"))
		}
	})
}

// A server that does not answer never fails a view: its sections read
// ok:false and the exit code stays 0.
func TestViewsSurviveADeadServer(t *testing.T) {
	viewsFixture(t)
	home, _ := os.UserHomeDir()
	pointAt(t, home, closedPort(t))
	for _, view := range []string{"overview", "bank", "pipeline", "system", "cost"} {
		v := runView(t, "stats", view)
		if v.code != output.OK {
			t.Fatalf("%s: exit %d: %s", view, v.code, v.raw)
		}
		if at(v.env.Data, "server", "ok") != false {
			t.Errorf("%s: server = %v", view, at(v.env.Data, "server"))
		}
	}
}

func TestViewsWaitForAnUpdate(t *testing.T) {
	home := devInstall(t, true)
	if err := state.WriteUpdateMarker(pathsOf(home), state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"overview", "sessions", "defs"} {
		v := runView(t, "stats", view)
		if v.code != output.Blocked || v.env.Error.Message != "an update is being applied, try again in a minute" {
			t.Errorf("%s: exit %d %q", view, v.code, v.env.Error.Message)
		}
	}
}

func TestFirstBuildShowsProgress(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	if err := os.MkdirAll(d.TelemetryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	ts := time.Now().UTC().Add(-time.Hour)
	for b.Len() < 2<<20 {
		b.WriteString(telLineFor(t, ts, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, MS: 10}, "s"+strconv.Itoa(b.Len()%7), "p1"))
	}
	if err := os.WriteFile(filepath.Join(d.TelemetryDir(), "events-"+ts.Format("2006-01")+".jsonl"), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	v := runView(t, "stats", "overview")
	if v.code != output.OK {
		t.Fatalf("exit %d: %s %s", v.code, v.raw, v.stderr)
	}
	if !strings.Contains(v.stderr, "metrics index:") {
		t.Errorf("no progress on stderr: %q", v.stderr)
	}
	if at(v.env.Data, "index", "partial") != false {
		t.Errorf("index = %v", at(v.env.Data, "index"))
	}
	if strings.Contains(v.raw, "metrics index:") {
		t.Error("progress leaked into stdout")
	}
}

func TestStatsAgreement(t *testing.T) {
	viewsFixture(t)
	win := []string{"--since", "2026-10-01T12:00:00Z", "--until", "2026-10-08T12:00:00Z"}
	view := runView(t, append([]string{"stats", "overview"}, win...)...)
	legacy := runView(t, append([]string{"stats"}, win...)...)
	if view.code != output.OK || legacy.code != output.OK {
		t.Fatalf("exit %d / %d", view.code, legacy.code)
	}
	act := at(view.env.Data, "tables", "activity")
	for _, c := range []struct {
		name  string
		view  any
		other any
	}{
		{"prompts", at(act, "prompts"), at(legacy.env.Data, "prompts", "human")},
		{"machine", at(act, "machine"), at(legacy.env.Data, "prompts", "machine")},
		{"recalls", at(act, "recalls"), at(legacy.env.Data, "prompts", "recalls")},
		{"with_hits", at(act, "with_hits"), at(legacy.env.Data, "prompts", "with_hits")},
		{"credits", at(act, "credits"), at(legacy.env.Data, "feedback", "verdicts")},
		{"retains", at(act, "retains"), at(legacy.env.Data, "retains")},
	} {
		if c.view == nil || !reflect.DeepEqual(c.view, c.other) {
			t.Errorf("%s: view %v, report %v", c.name, c.view, c.other)
		}
	}
	hr := at(view.env.Data, "metrics", "recall.hit_rate")
	if at(hr, "num") != at(act, "with_hits") || at(hr, "den") != at(act, "recalls") || at(hr, "den") == float64(0) {
		t.Errorf("hit rate %v against %v", hr, act)
	}
}

// Without --tz the window is read in this machine's zone.
func TestViewQueryDefaultsToTheLocalZone(t *testing.T) {
	q, err := viewQuery(viewFlags{limit: 50}, "", time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if q.Window.TZ != metrics.ZoneName(time.Local, os.Getenv, os.Readlink) || q.Window.Loc != time.Local {
		t.Errorf("window %+v", q.Window)
	}
}

// Every flag of a view is a parameter the dashboard knows, and the other way
// round: the two fronts cannot drift apart.
func TestParamsCoverEveryViewFlag(t *testing.T) {
	var stats *cobra.Command
	root := newRoot(&env{stdout: io.Discard, stderr: io.Discard})
	for _, c := range root.Commands() {
		if c.Name() == "stats" {
			stats = c
		}
	}
	if stats == nil {
		t.Fatal("no stats command")
	}
	for _, view := range metrics.Views {
		cmd, _, err := stats.Find([]string{view})
		if err != nil || cmd.Name() != view {
			t.Fatalf("no stats %s command: %v", view, err)
		}
		want := append(slices.Clone(views.CommonParams), views.ViewParams[view]...)
		have := map[string]bool{}
		for _, fs := range []*pflag.FlagSet{cmd.LocalFlags(), cmd.InheritedFlags()} {
			fs.VisitAll(func(f *pflag.Flag) {
				if f.Name != "json" && f.Name != "help" && f.Name != "text" {
					have[f.Name] = true
				}
			})
		}
		for n := range have {
			if !slices.Contains(want, n) {
				t.Errorf("stats %s: flag --%s is not a view parameter", view, n)
			}
		}
		for _, n := range want {
			if !have[n] {
				t.Errorf("stats %s: parameter %s is not a flag", view, n)
			}
		}
	}
}
