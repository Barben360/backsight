package doctor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/retain"
)

const (
	tagsPath    = "/v1/default/banks/coding-agent%3A%3Amemoire/tags"
	docsPath    = "/v1/default/banks/coding-agent%3A%3Amemoire/documents"
	noisePage   = 7 // the fake server's page size, below the client's: every list paginates
	transverse0 = ".claude"
)

// noiseBank fakes the memory bank's tags, the transverse pool's
// observations and the documents with their memories, paginated, and
// records what was read.
type noiseBank struct {
	tags         map[string]int // tag -> count
	observations []string       // texts tagged project:.claude
	rawSources   []string       // the source tag of each live raw fact tagged project:.claude
	docs         []noiseDoc
	mu           sync.Mutex
	reads        []string // path?query of every tags and list request
}

func (b *noiseBank) server(t *testing.T) *httptest.Server {
	base := fakeHandler(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, q := r.URL.EscapedPath(), r.URL.Query()
		if strings.HasSuffix(path, "/tags") || strings.HasSuffix(path, "/memories/list") {
			b.mu.Lock()
			b.reads = append(b.reads, path+"?"+r.URL.RawQuery)
			b.mu.Unlock()
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		limit = min(limit, noisePage)
		page := func(n int, item func(i int) any) {
			items := []any{}
			for i := offset; i < n && i < offset+limit; i++ {
				items = append(items, item(i))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": n})
		}
		switch {
		case path == docsPath:
			page(len(b.docs), func(i int) any { return map[string]any{"id": b.docs[i].id, "tags": []string{b.docs[i].tag}} })
		case path == listPath && q.Has("document_id"):
			var facts []map[string]any
			for _, d := range b.docs {
				if d.id == q.Get("document_id") {
					facts = d.facts
				}
			}
			page(len(facts), func(i int) any { return facts[i] })
		case path == tagsPath:
			var names []string
			for tag := range b.tags {
				if strings.HasPrefix(tag, strings.TrimSuffix(q.Get("q"), "*")) {
					names = append(names, tag)
				}
			}
			sort.Strings(names)
			page(len(names), func(i int) any { return map[string]any{"tag": names[i], "count": b.tags[names[i]]} })
		case path == listPath:
			// Untagged lists every observation; the noise check only the pool.
			raw := q.Get("type") == "world" || q.Get("type") == "experience"
			if q.Has("tags") && ((q.Get("type") != "observation" && !raw) || q.Get("tags") != "project:"+transverse0 || q.Get("tags_match") != "any_strict" || q.Has("state")) {
				t.Errorf("transverse list query: %s", r.URL.RawQuery)
			}
			if raw {
				var facts []map[string]any
				for i, src := range b.rawSources {
					if (i%2 == 0) == (q.Get("type") == "world") {
						facts = append(facts, map[string]any{"id": fmt.Sprintf("r%d", i), "fact_type": q.Get("type"), "text": "a raw fact", "tags": []string{"project:" + transverse0, "source:" + src}})
					}
				}
				page(len(facts), func(i int) any { return facts[i] })
				return
			}
			page(len(b.observations), func(i int) any {
				return map[string]any{"id": fmt.Sprintf("o%d", i), "fact_type": "observation", "text": b.observations[i], "tags": []string{"project:" + transverse0}}
			})
		default:
			base.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// noiseFixture is backsight's config on b's server, transverse aliases
// .claude and home.
func noiseFixture(t *testing.T, b *noiseBank) *fixture {
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+b.server(t).URL+"\"\nbank = \"coding-agent::memoire\"\n[projects]\ntransverse = [\".claude\", \"home\"]\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	return f
}

// pool is n observations, the first of them the given texts, the rest
// naming no project.
func pool(n int, texts ...string) []string {
	out := append([]string{}, texts...)
	for len(out) < n {
		out = append(out, fmt.Sprintf("The user prefers short answers (%d).", len(out)))
	}
	return out
}

func sameText(n int, s string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

var someProjects = map[string]int{"project:.claude": 40, "project:trip-2031": 9, "project:ironwood": 5, "project:backsight": 3, "project:crawler": 2}

// Exactly 20 % is not above the threshold; one more named observation is.
func TestNoiseTransverseThreshold(t *testing.T) {
	named := []string{"Branch content/v1 in example/trip-2031 holds two commits.", "trip-2031 uses Astro.", "Trip-2031's deploy is manual.",
		"ironwood runs on k3s."}
	b := &noiseBank{tags: someProjects, observations: pool(20, named...)}
	c := check(t, noiseFixture(t, b).run(), "noise-transverse")
	expect(t, c, OK, "4 of 20 transverse observations (project:.claude) name another project: trip-2031 (3), ironwood (1)")
	if c.Fix != "" {
		t.Errorf("no fix at 20 %%: %+v", c)
	}

	named = append(named, "ironwood and backsight share a VPS.", "backsight is written in Go.", "crawler logs to journald.")
	b = &noiseBank{tags: someProjects, observations: pool(20, named...), rawSources: []string{"chat", "subagent"}}
	c = check(t, noiseFixture(t, b).run(), "noise-transverse")
	// Top 3 only: crawler (1) is left out of the summary.
	expect(t, c, Warn, "7 of 20 transverse observations (project:.claude) name another project: trip-2031 (3), backsight (2), ironwood (2)")
	if strings.Contains(c.Summary, "crawler") || !strings.Contains(c.Fix, "backsight forget --project .claude --source subagent --plan") || !strings.Contains(c.Fix, "new sessions are attributed to their repository") {
		t.Errorf("%+v", c)
	}
	// report scrubs the names the check counted, the ones left out of the
	// summary too, and the alias its fix names.
	if got, want := strings.Join(c.Projects, ","), ".claude,backsight,crawler,ironwood,trip-2031"; got != want {
		t.Errorf("projects %q, want %q", got, want)
	}

	b = &noiseBank{tags: someProjects, observations: pool(20, append(named[:4:4], "ironwood and backsight share a VPS.")...)}
	expect(t, check(t, noiseFixture(t, b).run(), "noise-transverse"), Warn, "5 of 20")
}

// Under 20 observations nothing is judged, even when all name a project;
// an empty pool is an info too.
func TestNoiseTransverseTooFewOrEmpty(t *testing.T) {
	b := &noiseBank{tags: someProjects, observations: sameText(19, "trip-2031 uses Astro.")}
	c := check(t, noiseFixture(t, b).run(), "noise-transverse")
	expect(t, c, Info, "19 of 19")
	if !strings.Contains(c.Summary, "too few to judge") || c.Fix != "" {
		t.Errorf("%+v", c)
	}
	b = &noiseBank{tags: someProjects}
	c = check(t, noiseFixture(t, b).run(), "noise-transverse")
	expect(t, c, Info, "project:.claude holds no observation")
	// The summary names the alias: report hides what Projects lists.
	if got := strings.Join(c.Projects, ","); got != ".claude" {
		t.Errorf("projects %q, want the alias only", got)
	}
}

// A name matches as a whole token, in any case: infra is not
// infrastructure, garage is not my-garage. Transverse aliases and names
// under 4 characters never count.
func TestNoiseTransverseMatchesWholeNames(t *testing.T) {
	tags := map[string]int{"project:.claude": 1, "project:home": 1, "project:api": 1, "project:infra": 1, "project:my-garage": 1, "project:garage": 1}
	texts := []string{
		"The infrastructure is in Terraform.",        // not infra
		"The home directory holds dotfiles.",         // home is a transverse alias
		"The api answers in JSON.",                   // api is under 4 characters
		"Deployed my-garage today.",                  // my-garage, not garage
		"INFRA moved to OpenTofu.",                   // infra, any case
		"(infra) and \"my-garage\".",                 // punctuation is a boundary
		"garage_backup and infra-tools run nightly.", // '_' and '-' join a word
	}
	b := &noiseBank{tags: tags, observations: pool(20, texts...)}
	c := check(t, noiseFixture(t, b).run(), "noise-transverse")
	if want := "3 of 20 transverse observations (project:.claude) name another project: infra (2), my-garage (2)"; c.Status != OK || c.Summary != want {
		t.Errorf("got %s %q, want ok %q", c.Status, c.Summary, want)
	}
}

// Both lists paginate: a project tag on the tags' last page still counts,
// and every observation page is read. Past the read cap, the summary
// counts what was read and the detail says so.
func TestNoiseTransversePaginatesAndCaps(t *testing.T) {
	tags := map[string]int{}
	for i := range 3 * noisePage {
		tags[fmt.Sprintf("project:aaaa-%02d", i)] = 1
	}
	tags["project:zzzz-last"] = 1
	tags["project:"+transverse0] = 1
	b := &noiseBank{tags: tags, observations: pool(3*noisePage+2, "Nothing named here.")}
	b.observations[len(b.observations)-1] = "zzzz-last was renamed."
	c := check(t, noiseFixture(t, b).run(), "noise-transverse")
	expect(t, c, OK, fmt.Sprintf("1 of %d transverse observations", 3*noisePage+2))
	if !strings.Contains(c.Summary, "zzzz-last (1)") || c.Detail != "" {
		t.Errorf("%+v", c)
	}

	old := transverseReadMax
	transverseReadMax = 3 * noisePage
	t.Cleanup(func() { transverseReadMax = old })
	c = check(t, noiseFixture(t, b).run(), "noise-transverse")
	expect(t, c, OK, fmt.Sprintf("0 of %d transverse observations", 3*noisePage))
	if !strings.Contains(c.Detail, fmt.Sprintf("only the first %d observations", 3*noisePage)) {
		t.Errorf("%+v", c)
	}
}

// The share of subagent reports among chat, subagent and note facts is an
// info whatever it is; other source tags are not counted.
func TestNoiseSubagentShare(t *testing.T) {
	b := &noiseBank{tags: map[string]int{"source:chat": 70, "source:subagent": 25, "source:note": 5, "source:import": 100, "project:.claude": 3}}
	expect(t, check(t, noiseFixture(t, b).run(), "noise-subagent"), Info, "subagent reports are 25% of raw facts (25 of 100; chat 70, note 5)")
	b = &noiseBank{tags: map[string]int{"source:chat": 1, "source:subagent": 99}}
	expect(t, check(t, noiseFixture(t, b).run(), "noise-subagent"), Info, "99%")
	b = &noiseBank{tags: map[string]int{}}
	expect(t, check(t, noiseFixture(t, b).run(), "noise-subagent"), Info, "no raw fact")
}

// The noise checks read the memory bank alone: no tags or list request
// goes to <bank>-control or <bank>-docs.
func TestNoiseReadsOnlyTheMemoryBank(t *testing.T) {
	b := &noiseBank{tags: someProjects, observations: pool(20, "trip-2031 uses Astro.")}
	noiseFixture(t, b).run()
	if len(b.reads) == 0 {
		t.Fatal("nothing read")
	}
	for _, r := range b.reads {
		if strings.Contains(r, "-control") || strings.Contains(r, "-docs") {
			t.Errorf("read %s", r)
		}
	}
}

// Server down: the server-side checks are skipped, the local one runs.
func TestNoiseSkippedWhenServerDown(t *testing.T) {
	b := &noiseBank{tags: someProjects}
	f := noiseFixture(t, b)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	r := f.run()
	expect(t, check(t, r, "noise-transverse"), Info, "skipped: server not usable")
	expect(t, check(t, r, "noise-subagent"), Info, "skipped: server not usable")
	expect(t, check(t, r, "noise-headless"), OK, "no headless session")
	if len(b.reads) != 0 {
		t.Errorf("read %v", b.reads)
	}
}

// noiseDoc is a document of the bank, its project tag and its memories.
type noiseDoc struct {
	id, tag string
	facts   []map[string]any
}

// doc is document id of project p with raw facts in the given states
// ("" live) after one observation, the fact ids f-<id>-<n>.
func doc(id, p string, states ...string) noiseDoc {
	d := noiseDoc{id: id, tag: "project:" + p, facts: []map[string]any{{"id": "o-" + id, "fact_type": "observation"}}}
	for n, s := range states {
		d.facts = append(d.facts, map[string]any{"id": fmt.Sprintf("f-%s-%d", id, n), "fact_type": "world", "state": s})
	}
	return d
}

// sentSession writes a transcript started in cwd from entrypoint and its
// cursor with records sent.
func sentSession(t *testing.T, f *fixture, sid, entrypoint, cwd string, records int) {
	t.Helper()
	tr := filepath.Join(f.home, ".claude", "projects", "p", sid+".jsonl")
	line := fmt.Sprintf(`{"type":"user","uuid":"%s-u1","sessionId":%q,"cwd":%q,"entrypoint":%q,"timestamp":"2026-10-04T09:00:00Z","message":{"role":"user","content":"summarise this"}}`+"\n",
		sid, sid, cwd, entrypoint)
	put(t, tr, line, 0o600)
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := retain.SaveCursor(f.env.Dirs.CursorsDir(), sid, retain.Cursor{Transcript: tr, Records: records, Bank: "coding-agent::memoire"}); err != nil {
		t.Fatal(err)
	}
}

// A headless session warns only while the bank holds a live raw fact of
// one of its documents (decision 0071, M5 review rules): a purged or
// forgotten project stops warning. A project that also holds other
// sessions, like the transverse pool, gets its headless documents named,
// never forget --project. Interactive sessions and headless ones that sent
// nothing (skipped since C1) are ignored.
func TestNoiseHeadlessSessions(t *testing.T) {
	b := &noiseBank{tags: someProjects}
	f := noiseFixture(t, b)
	expect(t, check(t, f.run(), "noise-headless"), OK, "no headless session was sent")

	work := filepath.Join(filepath.Dir(f.home), "work")
	sentSession(t, f, "s1", "sdk-ts", filepath.Join(work, "notes-bot"), 3)
	sentSession(t, f, "s2", "cli", filepath.Join(work, "billing"), 5)
	sentSession(t, f, "s3", "sdk-cli", filepath.Join(work, "workspace"), 0)
	sentSession(t, f, "s4", "sdk-cli", filepath.Join(work, "crawler"), 1)
	sentSession(t, f, "s6", "sdk-cli", filepath.Join(work, "billing"), 2)
	sentSession(t, f, "s7", "sdk-cli", filepath.Join(work, "elsewhere"), 1)
	// Purged: the bank holds none of their documents any more.
	expect(t, check(t, f.run(), "noise-headless"), OK, "the 4 headless session(s) sent before privacy.skip_entrypoints hold no memory any more")

	b.docs = []noiseDoc{
		doc("conversation:s1", "notes-bot", ""),
		doc("subagent:s1:a1", "notes-bot", ""),
		doc("conversation:s2", "billing", ""),
		doc("conversation:s3", "workspace", ""),
		doc("conversation:s4", "crawler", "invalidated", ""),
		doc("conversation:s6", "billing", "", ""),
		doc("conversation:s7@crawler", "crawler", ""), // the bank's label wins
		doc("conversation:s9", "other", ""),
	}
	c := check(t, f.run(), "noise-headless")
	expect(t, c, Warn, "4 headless session(s) sent before privacy.skip_entrypoints still have memories: crawler (2), billing (1), notes-bot (1)")
	if want := "backsight forget f-conversation:s6-0 --purge (document conversation:s6); backsight forget --project crawler --plan; backsight forget --project notes-bot --plan"; c.Fix != want {
		t.Errorf("fix %q, want %q", c.Fix, want)
	}
	if !strings.Contains(c.Detail, "billing also hold other sessions") || strings.Contains(c.Summary, "workspace") || strings.Contains(c.Summary, "elsewhere") {
		t.Errorf("%+v", c)
	}
	if got, want := strings.Join(c.Projects, ","), "billing,crawler,notes-bot"; got != want {
		t.Errorf("projects %q, want %q (what report scrubs)", got, want)
	}

	// Forgotten without --purge: only observations and retired facts are
	// left, nothing to clean.
	b.docs = []noiseDoc{doc("conversation:s1", "notes-bot", "invalidated"), doc("conversation:s4", "crawler")}
	expect(t, check(t, f.run(), "noise-headless"), OK, "hold no memory any more")

	sentSession(t, f, "s5", "sdk-cli", f.home, 2)
	b.docs = []noiseDoc{doc("conversation:s5", ".claude", "")}
	c = check(t, f.run(), "noise-headless")
	expect(t, c, Warn, "1 headless session(s) sent before privacy.skip_entrypoints still have memories: .claude (1)")
	if c.Fix != "backsight forget f-conversation:s5-0 --purge (document conversation:s5)" || !strings.Contains(c.Detail, ".claude also hold other sessions") {
		t.Errorf("the transverse pool is never forgotten whole: %+v", c)
	}

	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	expect(t, check(t, f.run(), "noise-headless"), Info, "5 headless session(s) were sent before privacy.skip_entrypoints; the bank was not read: server not usable")
}

// A purge's successor id (conversation:<sid>#<n>) is the same session's.
func TestDocSessionOfASuccessorID(t *testing.T) {
	for id, want := range map[string]string{"conversation:s1#2": "s1", "conversation:s1@infra#3": "s1", "conversation:s1@infra": "s1", "subagent:s1:a1": "s1", "note:x": ""} {
		if got := docSession(id); got != want {
			t.Errorf("docSession(%s) = %q, want %q", id, got, want)
		}
	}
}

// The transverse noise is compared with the reading jobs maintain kept a
// week ago: a rise of 5 points that is also 3 observations or more warns,
// even under the 20 % bar; small pools give no trend.
func TestNoiseTransverseWarnsWhenItGrows(t *testing.T) {
	var named []string
	for i := range 12 {
		named = append(named, fmt.Sprintf("trip-2031 step %d uses Astro.", i))
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	week := now.Add(-7 * 24 * time.Hour)
	for _, c := range []struct {
		name    string
		pool    []string
		history []retain.NoisePoint
		status  Status
		summary string
	}{
		{"no history", pool(60, named...), nil, OK, "12 of 60 transverse observations (project:.claude) name another project: trip-2031 (12)"},
		{"from 9 of 60 a week ago: +5 points, +3 observations", pool(60, named...), []retain.NoisePoint{{At: week, Named: 9, Total: 60}}, Warn, "(20%, 15% a week ago): growing"},
		{"steady", pool(60, named...), []retain.NoisePoint{{At: week, Named: 12, Total: 60}}, OK, "(20%, 20% a week ago)"},
		{"the closest to a week is used", pool(60, named...), []retain.NoisePoint{{At: now.Add(-9 * 24 * time.Hour), Named: 0, Total: 60}, {At: week, Named: 12, Total: 60}, {At: now.Add(-24 * time.Hour), Named: 0, Total: 60}}, OK, "(20%, 20% a week ago)"},
		{"a reading too old is no comparison", pool(60, named...), []retain.NoisePoint{{At: now.Add(-20 * 24 * time.Hour), Named: 0, Total: 60}}, OK, "name another project: trip-2031 (12)"},
		{"a small pool: one more observation is no trend", pool(20, named[:4]...), []retain.NoisePoint{{At: week, Named: 3, Total: 20}}, OK, "4 of 20"},
		{"+5 points but 2 observations", pool(60, named...), []retain.NoisePoint{{At: week, Named: 10, Total: 90}}, OK, "(20%, 11% a week ago)"},
	} {
		f := noiseFixture(t, &noiseBank{tags: someProjects, observations: c.pool})
		f.env.Now = func() time.Time { return now }
		for _, p := range c.history {
			if err := retain.RecordNoise(f.env.Dirs.State, p); err != nil {
				t.Fatal(err)
			}
		}
		got := check(t, f.run(), "noise-transverse")
		if got.Status != c.status || !strings.Contains(got.Summary, c.summary) {
			t.Errorf("%s: %s %q, want %s %q", c.name, got.Status, got.Summary, c.status, c.summary)
		}
		if c.status == Warn && !strings.Contains(got.Fix, "retain reattribute --plan") {
			t.Errorf("%s: fix %q", c.name, got.Fix)
		}
		if got.Measure["total"] != float64(len(c.pool)) {
			t.Errorf("%s: measure %v", c.name, got.Measure)
		}
	}
}

func TestReattributionCheckShowsWhatIsLeft(t *testing.T) {
	at := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		st      *retain.ReattributionStatus
		status  Status
		summary string
		fix     string
	}{
		{"never run", nil, Info, "never judged", "retain reattribute --plan"},
		{"nothing left", &retain.ReattributionStatus{JudgedAt: at, Retagged: 3}, OK, "nothing left to send again (judged 2026-10-25)", ""},
		{"pending", &retain.ReattributionStatus{JudgedAt: at, Pending: []retain.Resend{{Session: "s1"}, {Session: "s2"}},
			Cost: retain.Estimate{InputTokens: 120000, Cost: 0.42}}, Warn, "2 session(s) mix projects in their documents (judged 2026-10-25): sending them again per project costs about 120000 input tokens, 0.42", "--answer resend=yes"},
		{"declined", &retain.ReattributionStatus{JudgedAt: at, Pending: []retain.Resend{{Session: "s1"}}, Declined: true}, Info, "sending them again was declined", ""},
		{"backups off", &retain.ReattributionStatus{Skipped: "backup.enabled is off, and the moves wait for a backup", SkippedAt: at}, Warn,
			"the daily re-attribution does not run (2026-10-25): backup.enabled is off", "retain reattribute --plan"},
		{"another integration writes", &retain.ReattributionStatus{Skipped: "components.write is off"}, Info, "does not run: components.write is off", ""},
		{"judged long ago", &retain.ReattributionStatus{JudgedAt: at.Add(-5 * 24 * time.Hour)}, Warn, "last judged against today's project rules on 2026-10-20", "jobs"},
	} {
		f := newFixture(t)
		f.env.Now = func() time.Time { return at.Add(time.Hour) }
		if c.st != nil {
			if err := retain.SaveReattributionStatus(f.env.Dirs.State, *c.st); err != nil {
				t.Fatal(err)
			}
		}
		got := checkReattribution(f.env)
		if got.Status != c.status || !strings.Contains(got.Summary, c.summary) || !strings.Contains(got.Fix, c.fix) {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

// A noise history that cannot be read makes the trend blind: doctor says
// so instead of staying quiet.
func TestAnUnreadableNoiseHistoryIsReported(t *testing.T) {
	f := noiseFixture(t, &noiseBank{tags: someProjects, observations: pool(60)})
	if err := os.MkdirAll(retain.NoiseHistoryFile(f.env.Dirs.State), 0o700); err != nil { // a directory: unreadable as a file
		t.Fatal(err)
	}
	c := check(t, f.run(), "noise-transverse")
	if c.Status != Warn || !strings.Contains(c.Summary, "daily readings cannot be read") || !strings.Contains(c.Fix, "noise-history.jsonl") {
		t.Errorf("%+v", c)
	}
}

// The fix names the subagent cleanup only while the pool still holds a
// live subagent fact; once they are retired, what is left came from
// conversations, which only a reattribution can move.
func TestNoiseTransverseFixFollowsWhatIsLeft(t *testing.T) {
	named := pool(20, sameText(10, "trip-2031 uses Astro.")...)
	for _, tc := range []struct {
		sources []string
		want    string
	}{
		{[]string{"chat", "note", "chat", "subagent"}, "new sessions are attributed to their repository; clean older ones with backsight forget --project .claude --source subagent --plan, or leave them"},
		{[]string{"chat", "note", "chat"}, "new sessions are attributed to their repository; what is left was learnt in conversations started from this directory: backsight retain reattribute moves them once it can tell their repository (jobs maintain runs it daily), or leave them"},
		{nil, "new sessions are attributed to their repository; what is left was learnt in conversations started from this directory: backsight retain reattribute moves them once it can tell their repository (jobs maintain runs it daily), or leave them"},
	} {
		b := &noiseBank{tags: someProjects, observations: named, rawSources: tc.sources}
		c := check(t, noiseFixture(t, b).run(), "noise-transverse")
		expect(t, c, Warn, "10 of 20")
		if c.Fix != tc.want {
			t.Errorf("sources %v: fix %q, want %q", tc.sources, c.Fix, tc.want)
		}
	}
	// Under the threshold, no raw fact is read.
	b := &noiseBank{tags: someProjects, observations: pool(20), rawSources: []string{"subagent"}}
	expect(t, check(t, noiseFixture(t, b).run(), "noise-transverse"), OK, "0 of 20")
	for _, r := range b.reads {
		if strings.Contains(r, "type=world") || strings.Contains(r, "type=experience") {
			t.Errorf("raw facts read with no warning: %s", r)
		}
	}
}
