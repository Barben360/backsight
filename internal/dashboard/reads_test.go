package dashboard

import (
	"html"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// reads is what one request cost the data layer: the index's CatchUp and
// ReadTx calls and the server's calls per method.
type reads struct {
	catchUps, readTxs int
	server            map[string]int
}

func (r reads) equal(o reads) bool {
	return r.catchUps == o.catchUps && r.readTxs == o.readTxs && maps.Equal(r.server, o.server)
}

// readsOf requests path with hdr, which must answer 200, and returns the
// body and what the request cost the fakes. Nothing else reads them
// meanwhile: the harness's clock does not move. The request has a
// connection of its own: the client never sends it again unseen (it
// would on a kept connection the server closed, past its WriteTimeout).
func (h *harness) readsOf(path string, hdr []string) (string, reads) {
	h.t.Helper()
	c0, r0, _ := h.ix.counts()
	s0 := h.srv.counts()
	resp := h.do("GET", path, append(slices.Clone(hdr), "Connection", "close")...)
	b := body(h.t, resp)
	if resp.StatusCode != 200 {
		h.t.Errorf("GET %s: %d", path, resp.StatusCode)
	}
	c1, r1, _ := h.ix.counts()
	d := reads{catchUps: c1 - c0, readTxs: r1 - r0, server: map[string]int{}}
	for m, n := range h.srv.counts() {
		if n != s0[m] {
			d.server[m] = n - s0[m]
		}
	}
	return b, d
}

// readPath is a page the read tests request: key names it the same on
// every history (a detail page's path names a row of that history).
type readPath struct{ key, path string }

// readPaths is every page route, from the route table so a new page is
// counted (a detail page asked for a project, session, memory and day of
// h's history); the Overview, System and Cost pages at the longest window
// the picker takes (they draw the most positions); and the pages that
// merge the other PCs, with machines=all; and the pages of one coding
// tool, with client=codex.
func readPaths(t *testing.T, h *harness) []readPath {
	t.Helper()
	sample := map[string]string{
		"project": "/projects/" + url.PathEscape(apiPath(t, h, "/api/v1/projects").Tables.Projects[0].Project),
		"session": "/sessions/" + url.PathEscape(apiPath(t, h, "/api/v1/sessions").Tables.Sessions[0].ID),
		"memory":  "/memories/" + url.PathEscape(apiPath(t, h, "/api/v1/memories").Tables.Memories[0].ID),
		"day":     "/days/" + apiPath(t, h, "/api/v1/days").Tables.Days[0].Day,
	}
	var out []readPath
	for _, rt := range routes {
		if rt.Kind != "page" {
			continue
		}
		path := rt.Sample
		if s, ok := sample[rt.Page]; ok {
			path = s
		}
		out = append(out, readPath{rt.Page, path})
	}
	for _, p := range []string{"/", "/system", "/cost"} {
		out = append(out, readPath{p + " long", p + "?since=" + longSince + "&until=" + longUntil})
	}
	for _, p := range []string{"/", "/quality", "/cost", "/days", "/releases", "/system", "/pipeline", sample["day"]} {
		key := p
		if p == sample["day"] {
			key = "day"
		}
		out = append(out, readPath{key + " all", p + "?machines=all"})
	}
	for _, p := range []string{"/", "/quality", "/cost", "/sessions", "/system", sample["day"]} {
		key := p
		if p == sample["day"] {
			key = "day"
		}
		out = append(out, readPath{key + " codex", p + "?client=codex"})
	}
	return out
}

// withOtherPCs is spec with the metrics/machines.json of 8 other PCs, and
// one session in four Codex's (the Overview splits the window by tool).
func withOtherPCs(spec indexgen.Spec) indexgen.Spec {
	spec.OtherPCs = 8
	spec.Codex = true
	return spec
}

// TestPageReadsDoNotGrowWithTheHistory: what a page costs is bounded by
// the reads it makes, counted, not by a time a loaded machine would miss.
// With every cache off, each page makes the same ReadTx, CatchUp and
// server calls on a month of history as on 13 months (the volume is the
// views' SQL, inside each read); with the caches on, a page asked again
// makes none: no ReadTx, no server call.
func TestPageReadsDoNotGrowWithTheHistory(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("13 months of pages, cold, take minutes under -race: make test-sweep runs it without")
	}
	// Restored after the subtests' harnesses stopped.
	oldSpec, oldResult, oldServer := dataSpec, resultTTL, serverTTL
	t.Cleanup(func() { dataSpec, resultTTL, serverTTL = oldSpec, oldResult, oldServer })

	// cold is, per page, what its second request cost with every cache
	// off (the first one pays what is done once, such as the doctor).
	cold := func(t *testing.T, spec indexgen.Spec) map[string]reads {
		dataSpec, resultTTL, serverTTL = spec, 0, 0
		h := dataHarness(t)
		ck := cookieHdr(h.login())
		out := map[string]reads{}
		for _, p := range readPaths(t, h) {
			h.readsOf(p.path, ck)
			b, r := h.readsOf(p.path, ck)
			if strings.HasSuffix(p.key, " all") && !strings.Contains(b, "all PCs on this server (9)") {
				t.Errorf("GET %s: not on all PCs", p.path)
			}
			if strings.HasSuffix(p.key, " codex") && !strings.Contains(b, `data-client="Codex" aria-current="true"`) {
				t.Errorf("GET %s: not on Codex alone", p.path)
			}
			if p.path == "/" && !strings.Contains(b, "Which coding tool used memory?") {
				t.Errorf("GET /: no split by coding tool")
			}
			out[p.key] = r
		}
		return out
	}
	var small, full map[string]reads
	t.Run("a month", func(t *testing.T) { small = cold(t, withOtherPCs(oldSpec)) })
	t.Run("13 months", func(t *testing.T) { full = cold(t, withOtherPCs(acceptanceSpec)) })
	if len(small) == 0 || len(small) != len(full) {
		t.Fatalf("%d pages counted on a month, %d on 13 months", len(small), len(full))
	}
	for k, s := range small {
		if f := full[k]; !s.equal(f) {
			t.Errorf("%s: %+v on a month, %+v on 13 months: the reads grow with the history", k, s, f)
		}
		// the count is of real reads: a page that reads nothing proves nothing
		if s.readTxs == 0 {
			t.Errorf("%s: no ReadTx with the caches off", k)
		}
	}

	t.Run("warm", func(t *testing.T) {
		dataSpec, resultTTL, serverTTL = withOtherPCs(acceptanceSpec), oldResult, oldServer
		h := dataHarness(t)
		ck := cookieHdr(h.login())
		for _, p := range readPaths(t, h) {
			h.readsOf(p.path, ck) // the warming request
			if _, r := h.readsOf(p.path, ck); r.readTxs != 0 || len(r.server) != 0 {
				t.Errorf("GET %s again: %d ReadTx, server %v, want none (the caches answer)", p.path, r.readTxs, r.server)
			}
		}
	})

	// The server down: every call blocks until its context ends, at the
	// real server timeout; the doctor cache was warmed at start. The page
	// still says so and shows what the index holds.
	t.Run("server down", func(t *testing.T) {
		dataSpec, resultTTL, serverTTL = acceptanceSpec, 0, 0
		h := dataHarness(t)
		ck := cookieHdr(h.login())
		h.srv.set(func(f *fakeServer) { f.down = true })
		r := h.do("GET", "/", ck...)
		b := html.UnescapeString(body(t, r))
		if r.StatusCode != 200 {
			t.Fatalf("/ with the server down: %d", r.StatusCode)
		}
		sec := parseHTML(t, b).first("section.card[aria-labelledby=w-glance]")
		if sec == nil || !strings.Contains(sec.text(), "Memory server not answering") {
			t.Errorf("the memory-size card does not say the server is not answering: %v", sec)
		}
		hit, _ := metrics.DefOf("recall.hit_rate")
		res := apiResult(t, h, "overview")
		want := views.FormatValue(hit, res.Metrics["recall.hit_rate"])
		if want == "—" || !strings.Contains(visibleText(b), want) {
			t.Errorf("recall.hit_rate %q not shown with the server down", want)
		}
	})
}
