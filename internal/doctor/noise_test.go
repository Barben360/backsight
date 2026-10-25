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

	"github.com/Barben360/backsight/internal/retain"
)

const (
	tagsPath    = "/v1/default/banks/coding-agent%3A%3Amemoire/tags"
	noisePage   = 7 // the fake server's page size, below the client's: every list paginates
	transverse0 = ".claude"
)

// noiseBank fakes the memory bank's tags and the transverse pool's
// observations, paginated, and records what was read.
type noiseBank struct {
	tags         map[string]int // tag -> count
	observations []string       // texts tagged project:.claude
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
		switch path {
		case tagsPath:
			var names []string
			for tag := range b.tags {
				if strings.HasPrefix(tag, strings.TrimSuffix(q.Get("q"), "*")) {
					names = append(names, tag)
				}
			}
			sort.Strings(names)
			page(len(names), func(i int) any { return map[string]any{"tag": names[i], "count": b.tags[names[i]]} })
		case listPath:
			// Untagged lists every observation; the noise check only the pool.
			if q.Has("tags") && (q.Get("type") != "observation" || q.Get("tags") != "project:"+transverse0 || q.Get("tags_match") != "any_strict") {
				t.Errorf("transverse list query: %s", r.URL.RawQuery)
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
	named := []string{"Branch content/v1 in Barben360/trip-2031 holds two commits.", "trip-2031 uses Astro.", "Trip-2031's deploy is manual.",
		"ironwood runs on k3s."}
	b := &noiseBank{tags: someProjects, observations: pool(20, named...)}
	c := check(t, noiseFixture(t, b).run(), "noise-transverse")
	expect(t, c, OK, "4 of 20 transverse observations (project:.claude) name another project: trip-2031 (3), ironwood (1)")
	if c.Fix != "" {
		t.Errorf("no fix at 20 %%: %+v", c)
	}

	named = append(named, "ironwood and backsight share a VPS.", "backsight is written in Go.", "crawler logs to journald.")
	b = &noiseBank{tags: someProjects, observations: pool(20, named...)}
	c = check(t, noiseFixture(t, b).run(), "noise-transverse")
	// Top 3 only: crawler (1) is left out of the summary.
	expect(t, c, Warn, "7 of 20 transverse observations (project:.claude) name another project: trip-2031 (3), backsight (2), ironwood (2)")
	if strings.Contains(c.Summary, "crawler") || !strings.Contains(c.Fix, "backsight forget --project .claude --source subagent --plan") || !strings.Contains(c.Fix, "0071") {
		t.Errorf("%+v", c)
	}
	// report scrubs the names the check counted, the ones left out of the
	// summary too, and the alias its fix names.
	if got, want := strings.Join(c.Projects, ","), ".claude,backsight,ironwood,crawler,trip-2031"; got != want {
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
	expect(t, check(t, noiseFixture(t, b).run(), "noise-transverse"), Info, "project:.claude holds no observation")
}

// A name matches as a whole token, in any case: infra is not
// infrastructure, garage is not my-garage. Transverse aliases and names
// under 4 characters never count.
func TestNoiseTransverseMatchesWholeNames(t *testing.T) {
	tags := map[string]int{"project:.claude": 1, "project:home": 1, "project:api": 1, "project:infra": 1, "project:my-garage": 1, "project:garage": 1}
	texts := []string{
		"The infrastructure is in Terraform.",         // not infra
		"The home directory holds dotfiles.",          // home is a transverse alias
		"The api answers in JSON.",                    // api is under 4 characters
		"Deployed my-garage today.",                  // my-garage, not garage
		"INFRA moved to OpenTofu.",                    // infra, any case
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

// A headless session that sent records warns with its project and the
// forget fix; an interactive one, and a headless one that sent nothing
// (skipped since C1), are ignored. A headless session in the transverse
// pool is named but left out of the fix.
func TestNoiseHeadlessSessions(t *testing.T) {
	b := &noiseBank{tags: someProjects}
	f := noiseFixture(t, b)
	expect(t, check(t, f.run(), "noise-headless"), OK, "no headless session was sent")

	work := filepath.Join(filepath.Dir(f.home), "work")
	sentSession(t, f, "s1", "sdk-ts", filepath.Join(work, "notes-bot"), 3)
	sentSession(t, f, "s2", "cli", filepath.Join(work, "billing"), 5)
	sentSession(t, f, "s3", "sdk-cli", filepath.Join(work, "workspace"), 0)
	sentSession(t, f, "s4", "sdk-cli", filepath.Join(work, "crawler"), 1)
	c := check(t, f.run(), "noise-headless")
	expect(t, c, Warn, "2 headless session(s) were sent before privacy.skip_entrypoints: notes-bot (1), crawler (1)")
	if c.Fix != "backsight forget --project notes-bot --plan; backsight forget --project crawler --plan" ||
		strings.Contains(c.Summary, "billing") || strings.Contains(c.Summary, "workspace") {
		t.Errorf("%+v", c)
	}

	sentSession(t, f, "s5", "sdk-cli", f.home, 2)
	c = check(t, f.run(), "noise-headless")
	expect(t, c, Warn, "3 headless session(s)")
	if !strings.Contains(c.Summary, ".claude (1)") || strings.Contains(c.Fix, "--project .claude") || !strings.Contains(c.Detail, "not .claude") {
		t.Errorf("%+v", c)
	}
	if got, want := strings.Join(c.Projects, ","), ".claude,notes-bot,crawler"; got != want {
		t.Errorf("projects %q, want %q (what report scrubs)", got, want)
	}
}
