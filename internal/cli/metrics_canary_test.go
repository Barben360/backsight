package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/machines"
)

// The canary of the published daily figures: the document holds no text of
// any source. Its fields are a closed set of names and its values dates,
// integers, an OS enum, the machine id and guarded versions; nothing else.

var uuidShape = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// docFields are the names a field of the document may have; the keys of a
// `versions` object are versions instead.
var docFields = []string{"v", "machine", "at", "os", "first_seen", "binary_version", "until", "latency_edges_ms", "days",
	"day", "prompts", "hit", "used", "coverage", "wrong", "context", "flow", "latency", "versions"}

// localNames are the texts of this computer that must never be published:
// its name and the user's (a name under 3 characters would match anything).
func localNames() []string {
	var out []string
	add := func(s string) {
		if len(s) >= 3 && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	if u, err := user.Current(); err == nil {
		add(u.Username)
	}
	add(os.Getenv("USER"))
	add(os.Getenv("USERNAME"))
	return out
}

// assertNoCanary fails when raw holds a canary, this computer's name or the
// user's, not exactly one UUID-shaped text (the machine id), a field name
// outside the document's, or a string that is not a date, an OS enum, the
// machine id or a version that passes ValidVersion.
func assertNoCanary(tb testing.TB, raw string, canaries []string) {
	tb.Helper()
	for _, c := range canaries {
		if strings.Contains(raw, c) {
			tb.Errorf("canary %q in the document", c)
		}
	}
	for _, n := range localNames() {
		// an OS enum is no leak of a computer that happens to be called so
		if !slices.Contains([]string{"linux", "darwin", "windows", "other"}, n) && strings.Contains(raw, n) {
			tb.Errorf("this computer's or user's name %q in the document", n)
		}
	}
	if ids := uuidShape.FindAllString(raw, -1); len(ids) != 1 {
		tb.Errorf("%d UUID-shaped texts %v, want exactly the machine id", len(ids), ids)
	}
	var doc any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		tb.Errorf("the document is no JSON: %v", err)
		return
	}
	var walk func(v any, parent string)
	walk = func(v any, parent string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if parent == "versions" {
					if !machines.ValidVersion(k) {
						tb.Errorf("the versions key %q is no version", k)
					}
				} else if !slices.Contains(docFields, k) {
					tb.Errorf("the field name %q is not a field of the document", k)
				}
				walk(e, k)
			}
		case []any:
			for _, e := range x {
				walk(e, parent)
			}
		case string:
			_, derr := time.Parse(time.DateOnly, x)
			ok := derr == nil || x == "linux" || x == "darwin" || x == "windows" || x == "other" ||
				(uuidShape.MatchString(x) && len(x) == 36) || machines.ValidVersion(x)
			if !ok {
				tb.Errorf("the string value %q (of %q) is no date, OS, machine id or version", x, parent)
			}
		}
	}
	walk(doc, "")
}

// recordingTB is a testing.TB that records failures instead of failing.
type recordingTB struct {
	testing.TB
	failed bool
	log    []string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Errorf(f string, a ...any) {
	r.failed = true
	r.log = append(r.log, f)
}

const leakCanary = "CANARY-PROJECT-LEAK"

func TestCanaryCheckCatchesALeak(t *testing.T) {
	names := localNames()
	host, _ := os.Hostname()
	good := `{"v":1,"machine":"` + pubID + `","at":1,"os":"linux","first_seen":"2026-09-02","binary_version":"v0.1.30-m7","until":"2026-11-05","latency_edges_ms":[50],"days":[{"day":"2026-11-05","prompts":1,"versions":{"v0.1.30-m7":{"prompts":1}}}]}`
	check := func(raw string) *recordingTB {
		r := &recordingTB{TB: t}
		assertNoCanary(r, raw, []string{leakCanary})
		return r
	}
	if r := check(good); r.failed {
		t.Fatalf("a clean document fails the check: %v", r.log)
	}
	leaks := map[string]string{
		"the canary project as a versions key": strings.Replace(good, `"v0.1.30-m7":{"prompts":1}`, `"`+leakCanary+`":{"prompts":1}`, 1),
		"a second UUID":                        strings.Replace(good, `"os":"linux"`, `"os":"linux","note":"00000000-0000-4000-8000-000000000000"`, 1),
		"a free field":                         strings.Replace(good, `"os":"linux"`, `"os":"linux","project":"x"`, 1),
		"a free string value":                  strings.Replace(good, `"os":"linux"`, `"os":"two words"`, 1),
	}
	if len(names) > 0 && len(host) >= 3 {
		hb, _ := json.Marshal(host)
		leaks["the hostname as binary_version"] = strings.Replace(good, `"binary_version":"v0.1.30-m7"`, `"binary_version":`+string(hb), 1)
	} else {
		t.Log("no name of 3 characters or more on this computer: the hostname case is skipped")
	}
	for what, raw := range leaks {
		if r := check(raw); !r.failed {
			t.Errorf("%s: the check passes the leaking document\n%s", what, raw)
		}
	}
}

// canaryDailyLines are telemetry lines of the previous UTC day that carry a
// canary in every source Daily reads: a typed prompt with a canary project,
// session, prompt text and cwd, an invalid binary version on a third of the
// prompts, a briefing with a text, a document with a canary name.
func canaryDailyLines(day time.Time) []string {
	at := func(m int) string {
		return day.Add(12*time.Hour + time.Duration(m)*time.Minute).Format(time.RFC3339Nano)
	}
	var lines []string
	for i := range 9 {
		ver := "v0.0.7"
		if i%3 == 0 {
			ver = "v1 CANARY-ver"
		}
		lines = append(lines, `{"event":"prompt","ts":"`+at(i)+`","session":"CANARY-SESSION-D`+strconv.Itoa(i)+`","project":"CANARY-PROJECT-D","binary_version":"`+ver+
			`","decision":"recall","n":1,"plen":80,"ms":`+strconv.Itoa(40+i*300)+`,"injected":`+strconv.Itoa(i%2)+`,"prompt":"CANARY-PROMPT-D","cwd":"/home/u/CANARY-PATH-D/p"}`)
	}
	lines = append(lines,
		`{"event":"briefing","ts":"`+at(20)+`","session":"CANARY-SESSION-D0","project":"CANARY-PROJECT-D","binary_version":"v1 CANARY-ver","ids":["CANARY-BRIEF-ID"],"chars":300,"source":"raw","text":"CANARY-BRIEF-TEXT-D"}`,
		`{"event":"retain","ts":"`+at(21)+`","session":"CANARY-SESSION-D1","document":"conversation:CANARY-DOC-NAME-D","items":1,"status":"ok","ms":3}`)
	return lines
}

func TestPublishedDocumentHoldsNoCanary(t *testing.T) {
	home := canaryState(t)
	canaryServer(t, home)
	canaryCollect(t, home)
	d := pathsOf(home)
	// The publish's clock is pinned well before the fixtures: canaryState writes
	// prompts relative to the wall clock (about an hour back), so their rows fall
	// after the window's end and never join the canary day, whatever date or
	// hour the test runs on.
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	yesterday := utcMidnight(now).AddDate(0, 0, -1)
	month := yesterday.Format("2006-01")
	f, err := os.OpenFile(filepath.Join(d.TelemetryDir(), "events-"+month+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Join(canaryDailyLines(yesterday), "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Feedback.Enabled, cfg.Feedback.ShareLedger = true, true
	// The server's address and the bank, the two settings canaryConfig
	// leaves as they are to reach the fake server, are checked here: the
	// publish record names its target by a hash only.
	cfg.Server.Bank = "CANARY-CFG-Server-Bank"
	serverHost := strings.TrimPrefix(cfg.Server.URL, "http://")
	defer func() {
		for name, b := range metricsFilesOf(t, home) {
			for _, c := range []string{cfg.Server.Bank, serverHost} {
				if strings.Contains(b, c) {
					t.Errorf("%q in metrics/%s", c, name)
				}
			}
		}
	}()
	cl, err := hindsight.New(cfg.Server.URL, "k", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	writes := watchMetricsWrites(t)
	s, err := publishMetrics(context.Background(), cl, d, home, cfg, pubID, now)
	if err != nil || !s.Up {
		t.Fatalf("publish: %+v (%v)", s, err)
	}
	got := writes()
	if len(got) != 1 {
		t.Fatalf("%d metrics documents written, want 1", len(got))
	}
	w := got[0]
	raw := w.Meta[machines.MetaKey]

	canaryList := slices.Concat(canaries, serverCanaries, []string{"CANARY", ctlProject, ctlMemory, ctlListed, ctlSessionA, ctlSessionB, ctlSessionC, "v1 CANARY-ver"})
	assertNoCanary(t, raw, canaryList)
	// the id, the content and every other metadata key are as clean
	for _, text := range []string{w.ID, w.Content} {
		for _, c := range canaryList {
			if strings.Contains(text, c) {
				t.Errorf("canary %q in %q", c, text)
			}
		}
	}
	if len(w.Meta) != 1 {
		t.Errorf("metadata %v: the figures only", w.Meta)
	}

	// the document is not empty: the invalid version's prompts are in the
	// day, the valid one's are the only breakdown
	doc := parseWrite(t, w, now)
	var day *machines.Day
	for i := range doc.Days {
		if doc.Days[i].Day == yesterday.Format(time.DateOnly) {
			day = &doc.Days[i]
		}
	}
	if day == nil || day.Prompts != 9 {
		t.Fatalf("the canary day holds %+v, want its 9 prompts", doc.Days)
	}
	vs := slices.Collect(func(yield func(string) bool) {
		for v := range day.Versions {
			if !yield(v) {
				return
			}
		}
	})
	if len(vs) != 1 || vs[0] != "v0.0.7" || day.Versions["v0.0.7"].Prompts != 6 {
		t.Errorf("the day's versions %v, want only v0.0.7 with 6 prompts", day.Versions)
	}
	if strings.Contains(raw, "CANARY") {
		t.Errorf("CANARY in the document: %s", raw)
	}
}
