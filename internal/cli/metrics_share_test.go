package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// pubID is a machine id whose first byte (3f = 63) puts its publish offset at 3 minutes.
const pubID = "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6"

var pubT0 = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

func TestMetricsDueRule(t *testing.T) {
	const target = "url http://x bank b"
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	sent := func(s string) machines.Published { return machines.Published{At: at(s), Server: target} }
	failed := func(sendAt, failAt string) machines.Published {
		p := sent(sendAt)
		p.FailedAt, p.FailedSince, p.Failure = at(failAt), at(failAt), "server"
		return p
	}
	const zero = "00000000-0000-4000-8000-000000000000" // offset 0
	const m2d = "2d000000-0000-4000-8000-000000000000"  // offset 45 min
	const m3b = "3b000000-0000-4000-8000-000000000000"  // offset 59 min
	cases := []struct {
		name    string
		last    machines.Published
		ok      bool
		target  string
		machine string
		now     string
		due     bool
	}{
		{"no record", machines.Published{}, false, target, zero, "2026-11-06T10:00:00Z", true},
		{"record ahead of now", sent("2026-11-06T12:00:00Z"), true, target, zero, "2026-11-06T10:00:00Z", true},
		{"other target", sent("2026-11-06T09:00:00Z"), true, "url http://y bank b", zero, "2026-11-06T10:00:00Z", true},
		{"yesterday, a new UTC day", sent("2026-11-05T23:59:00Z"), true, target, zero, "2026-11-06T00:01:00Z", true},
		{"same UTC day", sent("2026-11-06T00:01:00Z"), true, target, zero, "2026-11-06T23:59:00Z", false},
		{"failure 30 min ago", failed("2026-11-05T09:00:00Z", "2026-11-06T09:30:00Z"), true, target, zero, "2026-11-06T10:00:00Z", false},
		{"failure 61 min ago", failed("2026-11-05T09:00:00Z", "2026-11-06T08:59:00Z"), true, target, zero, "2026-11-06T10:00:00Z", true},
		{"failure ahead of now", failed("2026-11-05T09:00:00Z", "2026-11-06T11:00:00Z"), true, target, zero, "2026-11-06T10:00:00Z", true},
		{"offset 45: before it", sent("2026-11-05T09:00:00Z"), true, target, m2d, "2026-11-06T00:30:00Z", false},
		{"offset 45: after it", sent("2026-11-05T09:00:00Z"), true, target, m2d, "2026-11-06T00:46:00Z", true},
		{"offset 59: before it", sent("2026-11-05T23:30:00Z"), true, target, m3b, "2026-11-06T00:58:00Z", false},
		{"offset 59: at it", sent("2026-11-05T23:30:00Z"), true, target, m3b, "2026-11-06T00:59:00Z", true},
		{"new target, failed 30 min ago", failed("2026-11-05T09:00:00Z", "2026-11-06T09:30:00Z"), true, "url http://y bank b", zero, "2026-11-06T10:00:00Z", false},
		{"new target, failed 61 min ago", failed("2026-11-05T09:00:00Z", "2026-11-06T08:59:00Z"), true, "url http://y bank b", zero, "2026-11-06T10:00:00Z", true},
		{"never sent, a failure record only", machines.Published{FailedAt: at("2026-11-06T09:30:00Z"), FailedSince: at("2026-11-06T09:30:00Z"), Failure: "server", Server: target}, true, target, zero, "2026-11-06T10:00:00Z", false},
		{"never sent, waiting for the index", machines.Published{FailedSince: at("2026-11-06T09:30:00Z"), Failure: "budget", Server: target}, true, target, zero, "2026-11-06T10:00:00Z", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := metricsDue(c.last, c.ok, c.target, c.machine, at(c.now)); got != c.due {
				t.Errorf("due = %v, want %v", got, c.due)
			}
		})
	}
}

func TestPublishOffsetSpreads(t *testing.T) {
	for id, want := range map[string]time.Duration{
		"00000000-0000-4000-8000-000000000000": 0,
		"3b000000-0000-4000-8000-000000000000": 59 * time.Minute,
		"ff000000-0000-4000-8000-000000000000": 15 * time.Minute,
		pubID:                                  3 * time.Minute,
		"":                                     0,
		"zz":                                   0,
	} {
		if got := publishOffset(id); got != want {
			t.Errorf("publishOffset(%q) = %s, want %s", id, got, want)
		}
	}
}

// metricsWrite is one metrics document the fake server was asked to store.
type metricsWrite struct {
	ID, Content string
	Meta        map[string]string
}

// watchMetricsWrites records every metrics document written to the fake
// server from now on (the fake itself counts them as memories).
func watchMetricsWrites(t *testing.T) func() []metricsWrite {
	t.Helper()
	var got []metricsWrite
	fakeHook = func(r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "-control/memories") {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Items []struct {
				DocumentID string            `json:"document_id"`
				Content    string            `json:"content"`
				Metadata   map[string]string `json:"metadata"`
			} `json:"items"`
		}
		_ = json.Unmarshal(raw, &body)
		for _, it := range body.Items {
			if strings.HasPrefix(it.DocumentID, machines.DocPrefix) {
				got = append(got, metricsWrite{it.DocumentID, it.Content, it.Metadata})
			}
		}
	}
	t.Cleanup(func() { fakeHook = nil })
	return func() []metricsWrite { return got }
}

// figures is one valid day of figures.
func figures(day string) machines.Day {
	return machines.Day{Day: day,
		Figures: machines.Figures{Prompts: 41, Hit: machines.Pair{12, 30}, Used: machines.Pair{5, 9}, Coverage: machines.Pair{9, 11},
			Wrong: machines.Pair{0, 6}, Context: machines.Pair{72000, 4}, Latency: machines.Latency{0, 2, 10, 9, 5, 3, 1, 0, 0}},
		Flow: machines.Pair{3, 3}}
}

// stubDaily replaces dailyFigures; the returned count is its calls.
func stubDaily(t *testing.T, days []machines.Day, err error) *int {
	t.Helper()
	old, calls := dailyFigures, 0
	dailyFigures = func(context.Context, metrics.Source, time.Time) ([]machines.Day, error) {
		calls++
		return slices.Clone(days), err
	}
	t.Cleanup(func() { dailyFigures = old })
	return &calls
}

// stubCatchUp replaces publishCatchUp with fn.
func stubCatchUp(t *testing.T, fn func() (index.Freshness, error)) {
	t.Helper()
	old := publishCatchUp
	publishCatchUp = func(context.Context, *index.Index, time.Duration) (index.Freshness, error) { return fn() }
	t.Cleanup(func() { publishCatchUp = old })
}

func (m *shareMachine) publish(at time.Time) metricsShare {
	m.t.Helper()
	s, err := publishMetrics(context.Background(), m.cl, m.d, filepath.Dir(m.d.Data), m.cfg, pubID, at)
	if err != nil {
		m.t.Fatalf("publishMetrics: %v", err)
	}
	return s
}

func (m *shareMachine) record() machines.Published {
	m.t.Helper()
	p, _ := machines.LoadPublished(m.d.MetricsDir())
	return p
}

func parseWrite(t *testing.T, w metricsWrite, now time.Time) machines.Doc {
	t.Helper()
	doc, err := machines.Parse(w.ID, w.Meta[machines.MetaKey], now)
	if err != nil {
		t.Fatalf("the written document does not parse: %v\n%s", err, w.Meta[machines.MetaKey])
	}
	return doc
}

func TestPublishWritesOneDocumentADay(t *testing.T) {
	m := newShareMachine(t)
	writes := watchMetricsWrites(t)
	stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
	setBuild(t, "v0.1.30-m7", "abc")

	if s := m.publish(pubT0); !s.Up || s.Days != 1 || s.Bytes == 0 {
		t.Fatalf("first publish: %+v", s)
	}
	if s := m.publish(pubT0.Add(15 * time.Minute)); s.Up {
		t.Errorf("a second publish the same UTC day: %+v", s)
	}
	if s := m.publish(pubT0.Add(13 * time.Hour)); s.Up { // 23:00Z, still the 6th
		t.Errorf("a third publish the same UTC day: %+v", s)
	}
	if n := len(writes()); n != 1 {
		t.Fatalf("%d writes the first day, want 1", n)
	}
	w := writes()[0]
	if w.ID != "backsight:metrics:"+pubID || w.Content != "daily figures of machine "+pubID+": 1 days" {
		t.Errorf("id %q, content %q", w.ID, w.Content)
	}
	if len(w.Meta) != 1 || w.Meta["metrics"] == "" {
		t.Errorf("metadata %v: must hold only metrics", w.Meta)
	}
	doc := parseWrite(t, w, pubT0)
	if doc.Machine != pubID || doc.Until != "2026-11-05" || !doc.At.Equal(pubT0) || doc.BinaryVersion != "v0.1.30-m7" || len(doc.Days) != 1 || doc.Days[0].Hit != (machines.Pair{12, 30}) {
		t.Errorf("document %+v", doc)
	}
	if doc.OS != machines.OS(runtime.GOOS) {
		t.Errorf("os %q", doc.OS)
	}

	// The next UTC day: a second write, though the figures did not change.
	next := pubT0.Add(24 * time.Hour)
	if s := m.publish(next); !s.Up {
		t.Fatalf("next day: %+v", s)
	}
	if n := len(writes()); n != 2 {
		t.Fatalf("%d writes after two days, want 2", n)
	}
	if got := parseWrite(t, writes()[1], next); got.Until != "2026-11-06" || !got.At.Equal(next) {
		t.Errorf("second document %+v", got)
	}
	// The server holds one document for this machine.
	docs, err := retain.ControlDocuments(context.Background(), m.cl, m.cfg.Server.Bank, machines.DocPrefix)
	if err != nil || len(docs) != 1 || docs[0].ID != "backsight:metrics:"+pubID {
		t.Errorf("server holds %+v (%v)", docs, err)
	}
}

func TestPublishBinaryVersionIsValidatedOrOther(t *testing.T) {
	for version, want := range map[string]string{"v0.1.30-m7": "v0.1.30-m7", "(devel) with space": "other", "": "other"} {
		m := newShareMachine(t)
		writes := watchMetricsWrites(t)
		stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
		setBuild(t, version, "abc")
		if s := m.publish(pubT0); !s.Up {
			t.Fatalf("%q: %+v", version, s)
		}
		if got := parseWrite(t, writes()[0], pubT0).BinaryVersion; got != want {
			t.Errorf("version %q published as %q, want %q", version, got, want)
		}
	}
}

func TestPublishFitsTheCap(t *testing.T) {
	m := newShareMachine(t)
	writes := watchMetricsWrites(t)
	var days []machines.Day
	first := time.Date(2025, 10, 2, 0, 0, 0, 0, time.UTC) // 400 days ending 2026-11-05
	for i := range 400 {
		d := figures(first.AddDate(0, 0, i).Format("2006-01-02"))
		d.Versions = map[string]machines.Figures{}
		for v := range 8 {
			d.Versions["v0.1."+strconv.Itoa(v)] = d.Figures
		}
		days = append(days, d)
	}
	whole, err := machines.Doc{Machine: pubID, At: pubT0, OS: "linux", FirstSeen: "2026-11-06", BinaryVersion: "other", Until: "2026-11-05", Days: days}.Marshal()
	if err != nil || len(whole) <= machines.WriteMax {
		t.Fatalf("the fixture must be over the cap: %d bytes (%v)", len(whole), err)
	}
	stubDaily(t, days, nil)

	s := m.publish(pubT0)
	if !s.Up || len(writes()) != 1 {
		t.Fatalf("publish: %+v, %d writes", s, len(writes()))
	}
	meta := writes()[0].Meta["metrics"]
	if len(meta) > machines.WriteMax || len(meta) < machines.WriteMax-4096 {
		t.Errorf("written %d bytes, want just under %d", len(meta), machines.WriteMax)
	}
	if rec := m.record(); rec.Bytes != len(meta) || s.Bytes != len(meta) {
		t.Errorf("record bytes %d, result %d, written %d", rec.Bytes, s.Bytes, len(meta))
	}
	if s.Dropped.VersionDays == 0 || s.Days != 400-s.Dropped.Days {
		t.Errorf("dropped %+v, days %d", s.Dropped, s.Days)
	}
	var r tickResult
	r.applyMetrics(s, nil)
	if text := tickText(r); !strings.Contains(text, "version breakdown of "+strconv.Itoa(s.Dropped.VersionDays)+" oldest day(s) left out") {
		t.Errorf("tick line does not name the dropped days: %s", text)
	}
}

func TestPublishTooLargeIsAFailureClass(t *testing.T) {
	m := newShareMachine(t)
	writes := watchMetricsWrites(t)
	stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
	old := metricsWriteMax
	metricsWriteMax = 100
	t.Cleanup(func() { metricsWriteMax = old })
	s := m.publish(pubT0)
	if s.Up || s.Failure != "too_large" || len(writes()) != 0 {
		t.Fatalf("%+v, %d writes", s, len(writes()))
	}
	if rec := m.record(); rec.Failure != "too_large" || !rec.FailedAt.Equal(pubT0) || !rec.At.IsZero() {
		t.Errorf("record %+v", rec)
	}
}

// The publish record tells a change of server or bank by a hash: the file
// in metrics/ never holds the server's address or the bank's name. An
// older binary's record, holding them as text, still counts as sent to the
// same target (no second send that day); another bank is a new target.
func TestPublishRecordHoldsNoServerText(t *testing.T) {
	m := newShareMachine(t)
	writes := watchMetricsWrites(t)
	stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
	if s := m.publish(pubT0); !s.Up {
		t.Fatalf("publish: %+v", s)
	}
	raw, err := os.ReadFile(machines.PublishedFile(m.d.MetricsDir()))
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(strings.TrimPrefix(m.cfg.Server.URL, "http://"), "https://")
	for _, text := range []string{host, m.cfg.Server.Bank, shareTarget(m.d, m.cfg)} {
		if strings.Contains(string(raw), text) {
			t.Errorf("published.json holds %q: %s", text, raw)
		}
	}
	if rec := m.record(); rec.Server != machines.TargetKey(shareTarget(m.d, m.cfg)) {
		t.Errorf("server %q, want the target's hash", rec.Server)
	}

	legacy := m.record()
	legacy.Server = shareTarget(m.d, m.cfg)
	if err := machines.SavePublished(m.d.MetricsDir(), legacy); err != nil {
		t.Fatal(err)
	}
	if s := m.publish(pubT0.Add(15 * time.Minute)); s.Up || len(writes()) != 1 {
		t.Errorf("an older record of the same target: sent again (%+v, %d writes)", s, len(writes()))
	}
	m.cfg.Server.Bank = "another-bank"
	if s := m.publish(pubT0.Add(30 * time.Minute)); !s.Up {
		t.Errorf("another bank: not sent (%+v)", s)
	}
}

func TestPublishWaitsForAFullCatchUp(t *testing.T) {
	m := newShareMachine(t)
	writes := watchMetricsWrites(t)
	calls := stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
	// A send from yesterday, to be due today.
	prev := machines.Published{At: pubT0.Add(-24 * time.Hour), Server: shareTarget(m.d, m.cfg), Until: "2026-11-04", FirstSeen: "2026-10-01", Bytes: 99}
	if err := machines.SavePublished(m.d.MetricsDir(), prev); err != nil {
		t.Fatal(err)
	}
	answers := []func() (index.Freshness, error){
		func() (index.Freshness, error) { return index.Freshness{Partial: true}, nil },
		func() (index.Freshness, error) { return index.Freshness{Partial: true, Yielded: true}, nil },
		func() (index.Freshness, error) { return index.Freshness{}, context.DeadlineExceeded },
	}
	i := 0
	stubCatchUp(t, func() (index.Freshness, error) {
		if i < len(answers) {
			i++
			return answers[i-1]()
		}
		return index.Freshness{}, nil
	})

	first := pubT0
	for n := range answers {
		at := first.Add(time.Duration(n) * 15 * time.Minute)
		s := m.publish(at)
		if s.Up || !s.Waiting || s.Failure != "budget" {
			t.Fatalf("wait %d: %+v", n, s)
		}
		rec := m.record()
		if !rec.At.Equal(prev.At) || rec.Until != prev.Until || !rec.FailedAt.IsZero() || rec.Failure != "budget" || !rec.FailedSince.Equal(first) {
			t.Fatalf("wait %d: record %+v (at, until and failed_at stay; failed_since is the first wait)", n, rec)
		}
	}
	if len(writes()) != 0 || *calls != 0 {
		t.Fatalf("%d writes, %d computes while the index was not caught up", len(writes()), *calls)
	}

	s := m.publish(first.Add(time.Hour))
	if !s.Up || s.Waiting || len(writes()) != 1 {
		t.Fatalf("after a full catch-up: %+v, %d writes", s, len(writes()))
	}
	rec := m.record()
	if !rec.FailedSince.IsZero() || rec.Failure != "" || !rec.FailedAt.IsZero() || rec.Until != "2026-11-05" || rec.FirstSeen != "2026-10-01" {
		t.Errorf("record after the publish %+v", rec)
	}
	var r tickResult
	r.applyMetrics(metricsShare{Waiting: true, Failure: "budget"}, nil)
	if text := tickText(r); !strings.Contains(text, "daily figures: waiting for the index") {
		t.Errorf("tick line: %s", text)
	}
}

func TestPublishFailureIsRetriedAfterAnHour(t *testing.T) {
	m := newShareMachine(t)
	writes := watchMetricsWrites(t)
	stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
	retainStatus.Store(http.StatusInternalServerError)
	t.Cleanup(func() { retainStatus.Store(0) })

	s := m.publish(pubT0)
	if s.Up || s.Failure != "server" {
		t.Fatalf("500: %+v", s)
	}
	rec := m.record()
	if !rec.FailedAt.Equal(pubT0) || !rec.FailedSince.Equal(pubT0) || rec.Failure != "server" || !rec.At.IsZero() {
		t.Fatalf("record %+v", rec)
	}
	seen := len(writes())
	if s := m.publish(pubT0.Add(30 * time.Minute)); s.Up || s.Failure != "" || len(writes()) != seen {
		t.Errorf("30 min later: %+v, %d writes (was %d)", s, len(writes()), seen)
	}
	// 61 minutes later: tried again, fails again; failed_since stays.
	later := pubT0.Add(61 * time.Minute)
	if s := m.publish(later); s.Failure != "server" || len(writes()) != seen+1 {
		t.Fatalf("61 min later: %+v, %d writes (was %d)", s, len(writes()), seen)
	}
	if rec := m.record(); !rec.FailedAt.Equal(later) || !rec.FailedSince.Equal(pubT0) {
		t.Errorf("record %+v: failed_at moves, failed_since stays", rec)
	}
	// The server is back: a success clears the failure.
	retainStatus.Store(0)
	ok := later.Add(2 * time.Hour)
	if s := m.publish(ok); !s.Up {
		t.Fatalf("recovered: %+v", s)
	}
	if rec := m.record(); !rec.FailedAt.IsZero() || !rec.FailedSince.IsZero() || rec.Failure != "" || !rec.At.Equal(ok) {
		t.Errorf("record after success %+v", rec)
	}
	var r tickResult
	r.applyMetrics(metricsShare{Failure: "server"}, nil)
	if text := tickText(r); !strings.Contains(text, "daily figures not shared (server)") {
		t.Errorf("tick line: %s", text)
	}
}

func TestPublishComputeFailureAndEmptyIndex(t *testing.T) {
	m := newShareMachine(t)
	stubDaily(t, nil, context.Canceled)
	if s := m.publish(pubT0); s.Up || s.Failure != "compute" || m.record().Failure != "compute" {
		t.Fatalf("compute error: %+v, %+v", s, m.record())
	}
	stubDaily(t, nil, context.DeadlineExceeded)
	if s := m.publish(pubT0.Add(2 * time.Hour)); s.Failure != "budget" {
		t.Errorf("budget exhausted while computing: %+v", s)
	}

	// An empty index has nothing to share: no write, no record, no failure.
	m2 := newShareMachine(t)
	w2 := watchMetricsWrites(t)
	stubDaily(t, nil, nil)
	s := m2.publish(pubT0)
	if s.Up || !s.Empty || s.Failure != "" || len(w2()) != 0 {
		t.Fatalf("empty: %+v, %d writes", s, len(w2()))
	}
	if _, ok := machines.LoadPublished(m2.d.MetricsDir()); ok {
		t.Error("a record was written for nothing")
	}
	var r tickResult
	r.applyMetrics(s, nil)
	if text := tickText(r); !strings.Contains(text, "daily figures: nothing to share yet") {
		t.Errorf("tick line: %s", text)
	}
}

func TestFirstSeenIsKept(t *testing.T) {
	m := newShareMachine(t)
	writes := watchMetricsWrites(t)
	stubDaily(t, []machines.Day{figures("2026-10-31")}, nil)
	for i, day := range []string{"2026-11-01", "2026-11-02", "2026-11-05"} {
		at, _ := time.Parse("2006-01-02", day)
		at = at.Add(10 * time.Hour)
		if s := m.publish(at); !s.Up {
			t.Fatalf("%s: %+v", day, s)
		}
		if got := parseWrite(t, writes()[i], at).FirstSeen; got != "2026-11-01" {
			t.Errorf("%s: first_seen in the document %q", day, got)
		}
		if got := m.record().FirstSeen; got != "2026-11-01" {
			t.Errorf("%s: first_seen in the record %q", day, got)
		}
	}
}

// Nothing is published unless sharing is on, this PC is a client, and (through the tick) the
// joined server answers and this PC's sends have not started over.
func TestNoPublishWithoutSharing(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"share_ledger off":      func(c *config.Config) { c.Feedback.ShareLedger = false },
		"feedback off":          func(c *config.Config) { c.Feedback.Enabled = false },
		"a server-only host":    func(c *config.Config) { c.Server.Role = config.RoleServer },
		"no machine id (blank)": nil,
	} {
		t.Run(name, func(t *testing.T) {
			m := newShareMachine(t)
			writes := watchMetricsWrites(t)
			calls := stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
			machine := pubID
			if mutate != nil {
				mutate(&m.cfg)
			} else {
				machine = ""
			}
			s, err := publishMetrics(context.Background(), m.cl, m.d, filepath.Dir(m.d.Data), m.cfg, machine, pubT0)
			if err != nil || s.Up || len(writes()) != 0 || *calls != 0 {
				t.Errorf("%+v, %v, %d writes, %d computes", s, err, len(writes()), *calls)
			}
			if _, ok := machines.LoadPublished(m.d.MetricsDir()); ok {
				t.Error("a record was written")
			}
		})
	}

	t.Run("another server answers at the joined address", func(t *testing.T) {
		home := devInstall(t, true)
		pointAt(t, home, fakeHindsight(t))
		recordMismatch(t, home)
		writes := watchMetricsWrites(t)
		stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
		withClock(t, pubT0)
		if r := tickOf(t); r.MetricsUp || len(writes()) != 0 {
			t.Errorf("%+v, %d writes", r, len(writes()))
		}
		if _, ok := machines.LoadPublished(pathsOf(home).MetricsDir()); ok {
			t.Error("a record was written")
		}
	})

	t.Run("this PC's sends started over", func(t *testing.T) {
		home := devInstall(t, true)
		pointAt(t, home, fakeHindsight(t))
		writes := watchMetricsWrites(t)
		hook := fakeHook
		fakeHook = func(r *http.Request) {
			hook(r)
			if r.URL.Path == "/health" {
				_ = state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil })
			}
		}
		stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
		withClock(t, pubT0)
		if r := tickOf(t); r.MetricsUp || len(writes()) != 0 {
			t.Errorf("%+v, %d writes", r, len(writes()))
		}
		if _, ok := machines.LoadPublished(pathsOf(home).MetricsDir()); ok {
			t.Error("a record was written")
		}
	})
}

// Through the tick: the step runs after the shared counts, reports in the
// tick line, the envelope and the job event, and a server failure never
// fails the tick.
func TestTickPublishesAndReports(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	writes := watchMetricsWrites(t)
	stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
	withClock(t, pubT0)
	d := pathsOf(home)

	code, out, _ := run(t, "jobs", "tick")
	if code != output.OK || !strings.Contains(out, "daily figures sent") || len(writes()) != 1 {
		t.Fatalf("exit %d, %d writes\n%s", code, len(writes()), out)
	}
	machine := retain.ReadMachineID(d.State)
	if writes()[0].ID != machines.DocPrefix+machine {
		t.Errorf("document %q for machine %q", writes()[0].ID, machine)
	}
	// The job event carries counts and a class, never text.
	raw, _ := filepath.Glob(filepath.Join(d.TelemetryDir(), "events-*.jsonl"))
	var details map[string]any
	for _, f := range raw {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			var ev struct {
				Event, Name string
				Details     map[string]any
			}
			if json.Unmarshal([]byte(line), &ev) == nil && ev.Event == "job" && ev.Name == "tick" {
				details = ev.Details
			}
		}
	}
	if details["metrics_up"] != true || details["metrics_days"] != float64(1) || details["metrics_bytes"].(float64) <= 0 || details["metrics_failure"] != "" {
		t.Errorf("job event details %v", details)
	}

	// The next UTC day, the server refusing: the tick is done and names the class.
	retainStatus.Store(http.StatusInternalServerError)
	t.Cleanup(func() { retainStatus.Store(0) })
	withClock(t, pubT0.Add(24*time.Hour))
	r := tickOf(t)
	if r.Status != "done" || r.MetricsUp || r.MetricsFailure != "server" {
		t.Errorf("failing server: %+v", r)
	}
	if rec, _ := machines.LoadPublished(d.MetricsDir()); rec.Failure != "server" || rec.FailedSince.IsZero() {
		t.Errorf("record %+v", rec)
	}
	if text := tickText(r); !strings.Contains(text, "daily figures not shared (server)") {
		t.Errorf("tick line: %s", text)
	}
}

// A tick whose publish is due and whose index holds more than one budget of
// lines does one catch-up, the publish's: the tick's line is its freshness.
func TestTickCatchesUpOnceWhenThePublishIsDue(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	const total = 12_000
	writeIndexLines(t, d, "2026-09", total, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	var mu sync.Mutex
	at := time.Now()
	prevClock, prevBudget := tickIndexClock, tickIndexBudget
	tickIndexClock = func() time.Time { mu.Lock(); defer mu.Unlock(); at = at.Add(time.Second); return at }
	tickIndexBudget = index.TickBudget
	t.Cleanup(func() { tickIndexClock, tickIndexBudget = prevClock, prevBudget })
	stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
	calls, seen := 0, index.Freshness{}
	old := publishCatchUp
	publishCatchUp = func(ctx context.Context, ix *index.Index, b time.Duration) (index.Freshness, error) {
		calls++
		fr, err := ix.CatchUp(ctx, b)
		seen = fr
		return fr, err
	}
	t.Cleanup(func() { publishCatchUp = old })
	withClock(t, pubT0)

	_, res := tickJSON(t)
	n := indexCount(t, home, `SELECT count(*) FROM tel_prompt WHERE session = ?`, indexCanary)
	if calls != 1 || !seen.Partial {
		t.Fatalf("%d publish catch-up(s), freshness %+v: the backlog must outlast one budget", calls, seen)
	}
	if n <= 0 || n >= total {
		t.Errorf("%d rows after one tick: the tick caught up again (want a single partial pass)", n)
	}
	if res.MetricsIndex != tickIndexText(seen) || !res.MetricsWaiting {
		t.Errorf("metrics_index %q, want %q; waiting %v", res.MetricsIndex, tickIndexText(seen), res.MetricsWaiting)
	}
}

// A version figure above the day's makes a reader refuse the document: the
// publish drops the breakdown rather than send what no PC would accept.
func TestPublishDropsVersionsAReaderWouldRefuse(t *testing.T) {
	m := newShareMachine(t)
	writes := watchMetricsWrites(t)
	day := figures("2026-11-05")
	over := day.Figures
	over.Prompts = day.Prompts + 1
	day.Versions = map[string]machines.Figures{"0.1.30": over}
	stubDaily(t, []machines.Day{day}, nil)

	if s := m.publish(pubT0); !s.Up || s.Days != 1 {
		t.Fatalf("publish: %+v", s)
	}
	w := writes()
	if len(w) != 1 {
		t.Fatalf("%d documents written, want 1", len(w))
	}
	doc := parseWrite(t, w[0], pubT0)
	if len(doc.Days) != 1 || doc.Days[0].Versions != nil || doc.Days[0].Prompts != day.Prompts {
		t.Errorf("published days %+v, want the day's figures with no versions", doc.Days)
	}
	if strings.Contains(w[0].Meta[machines.MetaKey], `"versions"`) {
		t.Errorf("the document still carries versions: %s", w[0].Meta[machines.MetaKey])
	}
}

// What cannot be published even without versions is not written, and the
// failure is the class compute, never the error's text.
func TestPublishWritesNothingThatDoesNotParse(t *testing.T) {
	validID := pubID
	cases := []struct {
		name    string
		machine string
		first   string
	}{
		{"invalid first_seen in the record", validID, "not-a-date"},
		{"invalid machine id", "../../etc/passwd-0123456789abcdef!", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newShareMachine(t)
			writes := watchMetricsWrites(t)
			stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
			if c.first != "" {
				if err := machines.SavePublished(m.d.MetricsDir(), machines.Published{At: pubT0.Add(-48 * time.Hour), Server: shareTarget(m.d, m.cfg), FirstSeen: c.first}); err != nil {
					t.Fatal(err)
				}
			}
			s, err := publishMetrics(context.Background(), m.cl, m.d, filepath.Dir(m.d.Data), m.cfg, c.machine, pubT0)
			if err != nil {
				t.Fatal(err)
			}
			if s.Up || s.Failure != failCompute {
				t.Fatalf("result %+v, want a failure of class compute", s)
			}
			if w := writes(); len(w) != 0 {
				t.Errorf("%d documents written, want none: %+v", len(w), w)
			}
			if got := m.record().Failure; got != failCompute {
				t.Errorf("recorded failure %q, want %q", got, failCompute)
			}
		})
	}
}

// otherPC is a machine id for the fetch tests: n makes it distinct, its
// first 8 characters are n repeated.
func otherPC(n int) string {
	h := strings.Repeat(strconv.FormatInt(int64(n)%16, 16), 8)
	return h[:8] + "-" + strconv.Itoa(1000+n) + "-4000-8000-000000000000"
}

// otherDoc is another PC's valid document metadata, sent at at.
func otherDoc(t *testing.T, id string, at time.Time) string {
	t.Helper()
	until := at.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	meta, err := machines.Doc{Machine: id, At: at, OS: "linux", FirstSeen: "2026-10-01", BinaryVersion: "0.1.28", Until: until,
		Days: []machines.Day{figures(until)}}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

// putMetrics stores a metrics document on the server as a PC's tick would.
func (m *shareMachine) putMetrics(id, meta string) {
	m.t.Helper()
	if err := retain.WriteControlDocument(context.Background(), m.cl, m.cfg.Server.Bank, machines.DocPrefix+id, "x", map[string]string{machines.MetaKey: meta}); err != nil {
		m.t.Fatal(err)
	}
}

// walks counts the full listings of the control bank from now on.
func watchWalks(t *testing.T) func() int {
	t.Helper()
	n := 0
	fakeHook = func(r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "-control/documents") && r.URL.Query().Get("offset") == "0" {
			n++
		}
	}
	t.Cleanup(func() { fakeHook = nil })
	return func() int { return n }
}

// fetch is one tick's fetch, with its own listing.
func (m *shareMachine) fetch(at time.Time) (int, int, error) {
	m.t.Helper()
	return refreshMetricsDocs(context.Background(), m.cl, &controlListing{}, m.d, m.cfg, pubID, at)
}

func (m *shareMachine) machinesCache() machines.Cache {
	m.t.Helper()
	c, err := machines.LoadCache(m.d.MetricsDir())
	if err != nil {
		m.t.Fatal(err)
	}
	return c
}

func TestFetchEvery6h(t *testing.T) {
	m := newShareMachine(t)
	m.putMetrics(otherPC(1), otherDoc(t, otherPC(1), pubT0))
	walks := watchWalks(t)
	if f, _, err := m.fetch(pubT0); err != nil || f != 1 || walks() != 1 {
		t.Fatalf("first fetch: %d, %v, %d walks", f, err, walks())
	}
	if f, i, err := m.fetch(pubT0.Add(5 * time.Hour)); err != nil || f != 0 || i != 0 || walks() != 1 {
		t.Errorf("5 h later: %d, %d, %v, %d walks (the cache is fresh)", f, i, err, walks())
	}
	if f, _, err := m.fetch(pubT0.Add(6 * time.Hour)); err != nil || f != 1 || walks() != 2 {
		t.Errorf("6 h later: %d, %v, %d walks", f, err, walks())
	}
	// A cache dated ahead of now (the clock stepped back) is fetched again.
	c := m.machinesCache()
	c.FetchedAt = pubT0.Add(30 * time.Hour)
	if err := machines.SaveCache(m.d.MetricsDir(), c); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.fetch(pubT0.Add(7 * time.Hour)); err != nil || walks() != 3 {
		t.Errorf("a cache dated ahead: %v, %d walks", err, walks())
	}
	// An unreadable cache too.
	if err := os.WriteFile(machines.CacheFile(m.d.MetricsDir()), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, _, err := m.fetch(pubT0.Add(7 * time.Hour)); err != nil || f != 1 || walks() != 4 {
		t.Errorf("an unreadable cache: %d, %v, %d walks", f, err, walks())
	}
}

func TestFetchKeepsValidDocumentsOnly(t *testing.T) {
	m := newShareMachine(t)
	good, big, v2, liar := otherPC(1), otherPC(2), otherPC(3), otherPC(4)
	m.putMetrics(good, otherDoc(t, good, pubT0))
	m.putMetrics(big, strings.Repeat("x", machines.MaxBytes+1))
	newer := strings.Replace(otherDoc(t, v2, pubT0), `"v":1`, `"v":2`, 1)
	if !strings.Contains(newer, `"v":2`) {
		t.Fatalf("no version field to change: %s", newer)
	}
	m.putMetrics(v2, newer)
	m.putMetrics(liar, otherDoc(t, otherPC(9), pubT0)) // valid, under another machine's name

	fetched, ignored, err := m.fetch(pubT0)
	if err != nil || fetched != 1 || ignored != 3 {
		t.Fatalf("%d fetched, %d ignored, %v", fetched, ignored, err)
	}
	c := m.machinesCache()
	if len(c.Docs) != 1 || c.Docs[0].Name != machines.DocPrefix+good || c.Docs[0].Raw != otherDoc(t, good, pubT0) {
		t.Errorf("docs %+v", c.Docs)
	}
	want := []machines.Ignored{{Machine: big[:8], Reason: machines.TooLarge}, {Machine: v2[:8], Reason: machines.UnknownVersion}, {Machine: liar[:8], Reason: machines.NotValid}}
	if !slices.Equal(c.Ignored, want) {
		t.Errorf("ignored %+v, want %+v", c.Ignored, want)
	}
	if !c.FetchedAt.Equal(pubT0) {
		t.Errorf("fetched_at %s", c.FetchedAt)
	}
}

// A document under a name that holds no machine id is recorded without a
// short id, and nothing of its text reaches the cache's ignored list.
func TestFetchIgnoredHoldsNoNameWithoutAMachineId(t *testing.T) {
	m := newShareMachine(t)
	m.putMetrics("my-secret-host", "{}")
	if _, i, err := m.fetch(pubT0); err != nil || i != 1 {
		t.Fatalf("%d, %v", i, err)
	}
	c := m.machinesCache()
	if len(c.Ignored) != 1 || c.Ignored[0].Machine != "" || c.Ignored[0].Reason != machines.NotValid {
		t.Errorf("ignored %+v", c.Ignored)
	}
	raw, _ := os.ReadFile(machines.CacheFile(m.d.MetricsDir()))
	if strings.Contains(string(raw), "secret") {
		t.Errorf("a name reached the cache: %s", raw)
	}
}

func TestFetchLeavesOutOwnAndOld(t *testing.T) {
	m := newShareMachine(t)
	m.putMetrics(pubID, otherDoc(t, pubID, pubT0))
	m.putMetrics(otherPC(1), otherDoc(t, otherPC(1), pubT0.Add(-31*24*time.Hour)))
	m.putMetrics(otherPC(2), otherDoc(t, otherPC(2), pubT0.Add(-29*24*time.Hour)))
	f, i, err := m.fetch(pubT0)
	if err != nil || f != 1 || i != 0 {
		t.Fatalf("%d fetched, %d ignored, %v", f, i, err)
	}
	if c := m.machinesCache(); len(c.Docs) != 1 || c.Docs[0].Name != machines.DocPrefix+otherPC(2) || len(c.Ignored) != 0 {
		t.Errorf("%+v", c)
	}
}

func TestFetchKeeps64Newest(t *testing.T) {
	m := newShareMachine(t)
	const total = 70
	for n := range total { // PC n sent n hours ago
		m.putMetrics(otherPC(n), otherDoc(t, otherPC(n), pubT0.Add(-time.Duration(n)*time.Hour)))
	}
	f, _, err := m.fetch(pubT0)
	if err != nil || f != machines.MaxDocs {
		t.Fatalf("%d fetched, %v", f, err)
	}
	have := map[string]bool{}
	for _, d := range m.machinesCache().Docs {
		have[d.Name] = true
	}
	for n := range total {
		if want := n < machines.MaxDocs; have[machines.DocPrefix+otherPC(n)] != want {
			t.Errorf("PC %d (sent %d h ago) cached: %v, want %v", n, n, !want, want)
		}
	}
}

func TestFetchFailureKeepsTheCache(t *testing.T) {
	m := newShareMachine(t)
	m.putMetrics(otherPC(1), otherDoc(t, otherPC(1), pubT0))
	if _, _, err := m.fetch(pubT0); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(machines.CacheFile(m.d.MetricsDir()))
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(broken.Close)
	cl, err := hindsight.New(broken.URL, "k", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f, i, err := refreshMetricsDocs(context.Background(), cl, &controlListing{}, m.d, m.cfg, pubID, pubT0.Add(7*time.Hour))
	if err == nil || f != 0 || i != 0 {
		t.Errorf("%d, %d, %v: a failed listing must be an error", f, i, err)
	}
	if after, _ := os.ReadFile(machines.CacheFile(m.d.MetricsDir())); !bytes.Equal(before, after) {
		t.Errorf("the cache changed:\n%s\n%s", before, after)
	}
	// The class never carries the error's text.
	if _, _, class := fetchMetrics(context.Background(), cl, &controlListing{}, m.d, m.cfg, pubID, pubT0.Add(7*time.Hour)); class != fetchServer {
		t.Errorf("class %q", class)
	}
}

func TestFetchWithoutSharing(t *testing.T) {
	for name, mutate := range map[string]func(*shareMachine) string{
		"share_ledger off": func(m *shareMachine) string { m.cfg.Feedback.ShareLedger = false; return pubID },
		"feedback off":     func(m *shareMachine) string { m.cfg.Feedback.Enabled = false; return pubID },
		"no machine id":    func(*shareMachine) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			m := newShareMachine(t)
			m.putMetrics(otherPC(1), otherDoc(t, otherPC(1), pubT0))
			walks := watchWalks(t)
			machine := mutate(m)
			f, i, err := refreshMetricsDocs(context.Background(), m.cl, &controlListing{}, m.d, m.cfg, machine, pubT0)
			if f != 0 || i != 0 || err != nil || walks() != 0 {
				t.Errorf("%d, %d, %v, %d walks", f, i, err, walks())
			}
			if _, err := os.Stat(machines.CacheFile(m.d.MetricsDir())); err == nil {
				t.Error("a cache was written")
			}
		})
	}
	// An existing cache stays as it is.
	m := newShareMachine(t)
	m.putMetrics(otherPC(1), otherDoc(t, otherPC(1), pubT0))
	if _, _, err := m.fetch(pubT0); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(machines.CacheFile(m.d.MetricsDir()))
	m.cfg.Feedback.ShareLedger = false
	m.putMetrics(otherPC(2), otherDoc(t, otherPC(2), pubT0))
	if _, _, err := m.fetch(pubT0.Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(machines.CacheFile(m.d.MetricsDir())); !bytes.Equal(before, after) {
		t.Error("the cache changed with sharing off")
	}
}

// A server-only host publishes nothing and fetches the others', through the
// tick.
func TestServerOnlyHostFetches(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	d := pathsOf(home)
	raw, _ := os.ReadFile(d.ConfigFile())
	if err := os.WriteFile(d.ConfigFile(), bytes.Replace(raw, []byte("[server]\n"), []byte("[server]\nrole = \"server\"\n"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	cl, err := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "k", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(d, home)
	other := otherPC(1)
	withClock(t, pubT0)
	if err := retain.WriteControlDocument(context.Background(), cl, cfg.Server.Bank, machines.DocPrefix+other, "x", map[string]string{machines.MetaKey: otherDoc(t, other, pubT0)}); err != nil {
		t.Fatal(err)
	}
	writes := watchMetricsWrites(t)
	calls := stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)

	r := tickOf(t)
	if r.MetricsFetched != 1 || r.MetricsFetchError != "" {
		t.Fatalf("%+v", r)
	}
	if r.MetricsUp || len(writes()) != 0 || *calls != 0 {
		t.Errorf("a server-only host published: %+v, %d writes", r, len(writes()))
	}
	if _, ok := machines.LoadPublished(d.MetricsDir()); ok {
		t.Error("a publish record was written")
	}
	c, err := machines.LoadCache(d.MetricsDir())
	if err != nil || len(c.Docs) != 1 || c.Docs[0].Name != machines.DocPrefix+other {
		t.Errorf("%+v, %v", c, err)
	}
}

// The tick reports the fetch in its line, envelope and job event; a failing
// server never fails the tick nor puts text in the event.
func TestTickFetchesAndReports(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	d := pathsOf(home)
	withClock(t, pubT0)
	stubDaily(t, []machines.Day{figures("2026-11-05")}, nil)
	cl, err := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "k", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m := &shareMachine{t: t, d: d, cfg: config.Default(d, home), cl: cl}
	m.putMetrics(otherPC(1), otherDoc(t, otherPC(1), pubT0))
	m.putMetrics(otherPC(2), "{}")

	code, out, _ := run(t, "jobs", "tick")
	if code != output.OK || !strings.Contains(out, "daily figures of 1 other PC(s) fetched, 1 document(s) ignored") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	raw, _ := filepath.Glob(filepath.Join(d.TelemetryDir(), "events-*.jsonl"))
	var details map[string]any
	for _, f := range raw {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			var ev struct {
				Event, Name string
				Details     map[string]any
			}
			if json.Unmarshal([]byte(line), &ev) == nil && ev.Event == "job" && ev.Name == "tick" {
				details = ev.Details
			}
		}
	}
	if details["metrics_fetched"] != float64(1) || details["metrics_ignored"] != float64(1) || details["metrics_fetch_error"] != "" {
		t.Errorf("job event details %v", details)
	}
	// Five hours later nothing is fetched and the line says nothing of it.
	withClock(t, pubT0.Add(5*time.Hour))
	if r := tickOf(t); r.MetricsFetched != 0 || r.fetchText() != "" {
		t.Errorf("%+v", r)
	}
}

// The shared counts and the daily figures share the tick's one listing.
func TestOneListingPerTick(t *testing.T) {
	m := newShareMachine(t)
	m.put(shareM1, sharedMeta(t, shareM1, pubT0, map[string]feedback.SharedEntry{}))
	m.putMetrics(otherPC(1), otherDoc(t, otherPC(1), pubT0))
	walks := watchWalks(t)
	tick := func(at time.Time) (machinesN int) {
		l := &controlListing{}
		n, _, err := refreshShared(context.Background(), m.cl, l, m.d, m.cfg.Server.Bank, pubID, at, feedbackParams(m.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := refreshMetricsDocs(context.Background(), m.cl, l, m.d, m.cfg, pubID, at); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := tick(pubT0); walks() != 1 || n != 1 {
		t.Errorf("both due: %d walk(s), %d machine(s) counted", walks(), n)
	}
	if c := m.machinesCache(); len(c.Docs) != 1 {
		t.Errorf("the daily figures were not cached: %+v", c)
	}
	if n := tick(pubT0.Add(time.Hour)); walks() != 1 || n != 1 {
		t.Errorf("neither due: %d walk(s) in all, %d machine(s) counted", walks(), n)
	}
	// Only the daily figures due.
	if err := os.Remove(machines.CacheFile(m.d.MetricsDir())); err != nil {
		t.Fatal(err)
	}
	if n := tick(pubT0.Add(2 * time.Hour)); walks() != 2 || n != 1 {
		t.Errorf("only the figures due: %d walk(s) in all, %d machine(s) counted", walks(), n)
	}
	// Only the shared counts due.
	if err := os.Remove(filepath.Join(m.d.FeedbackDir(), "shared.json")); err != nil {
		t.Fatal(err)
	}
	if n := tick(pubT0.Add(3 * time.Hour)); walks() != 3 || n != 1 {
		t.Errorf("only the counts due: %d walk(s) in all, %d machine(s) counted", walks(), n)
	}
}

// padded is a valid document made as large as a reader accepts with an unknown
// field full of characters JSON escapes to six bytes.
func padded(t *testing.T, id string, at time.Time) string {
	t.Helper()
	meta := otherDoc(t, id, at)
	pad := machines.MaxBytes - len(meta) - len(`,"z":""`)
	padded := meta[:len(meta)-1] + `,"z":"` + strings.Repeat("<", pad) + `"}`
	if _, err := machines.Parse(machines.DocPrefix+id, padded, at); err != nil {
		t.Fatalf("the padded document must be valid: %v", err)
	}
	return padded
}

// Valid documents padded with an unknown field, which JSON escaping would
// multiply by six, must not make the cache unloadable.
func TestFetchCacheStaysLoadableWithPaddedDocuments(t *testing.T) {
	m := newShareMachine(t)
	const n = 20 // 20 MiB on the wire, 120 MiB escaped when cached as fetched
	// The listing is handed over: the test server escapes the padding too, and
	// the client would refuse its page; a real server need not.
	l := &controlListing{done: true}
	for i := range n {
		l.items = append(l.items, hindsight.DocumentItem{ID: machines.DocPrefix + otherPC(i),
			Metadata: map[string]string{machines.MetaKey: padded(t, otherPC(i), pubT0.Add(-time.Duration(i)*time.Minute))}})
	}
	f, _, err := refreshMetricsDocs(context.Background(), m.cl, l, m.d, m.cfg, pubID, pubT0)
	if err != nil || f != n {
		t.Fatalf("%d fetched, %v", f, err)
	}
	c, err := machines.LoadCache(m.d.MetricsDir())
	if err != nil {
		t.Fatalf("the cache must load: %v", err)
	}
	if len(c.Docs) != n {
		t.Fatalf("%d documents cached, want %d", len(c.Docs), n)
	}
	for _, d := range c.Docs {
		if len(d.Raw) > 4096 || strings.Contains(d.Raw, "<") {
			t.Errorf("%s: the cache holds more than the known fields (%d bytes)", d.Name, len(d.Raw))
		}
	}
}

func TestWithinCacheBudget(t *testing.T) {
	mk := func(name string, n int) machines.CachedDoc {
		return machines.CachedDoc{Name: name, Raw: strings.Repeat("a", n)}
	}
	docs := []machines.CachedDoc{mk("a", 100), mk("b", 100), mk("c", 10), mk("d", 100)}
	kept, over := withinCacheBudget(docs, func(d machines.CachedDoc) machines.CachedDoc { return d }, 260)
	names := func(l []machines.CachedDoc) (s string) {
		for _, d := range l {
			s += d.Name
		}
		return s
	}
	// a and b (about 124 bytes each) fit; c would push past 260, and d too
	if names(kept) != "ab" || names(over) != "cd" {
		t.Errorf("kept %q, over %q", names(kept), names(over))
	}
	kept, over = withinCacheBudget(docs, func(d machines.CachedDoc) machines.CachedDoc { return d }, 1<<20)
	if len(kept) != 4 || len(over) != 0 {
		t.Errorf("within a large budget: kept %d, over %d", len(kept), len(over))
	}
}

// A document dated ahead (Parse allows 24 h) counts as sent now, so it cannot
// outrank a real PC: with every real PC sent at now, it ties and loses (its
// name sorts last), where its own date would have put it first.
func TestFetchFutureDateDoesNotEvictARealPC(t *testing.T) {
	m := newShareMachine(t)
	for n := range machines.MaxDocs {
		m.putMetrics(otherPC(n), otherDoc(t, otherPC(n), pubT0))
	}
	liar := "ffffffff-9999-4000-8000-000000000000" // sorts after every real PC
	m.putMetrics(liar, otherDoc(t, liar, pubT0.Add(23*time.Hour)))
	f, _, err := m.fetch(pubT0)
	if err != nil || f != machines.MaxDocs {
		t.Fatalf("%d fetched, %v", f, err)
	}
	have := map[string]bool{}
	for _, d := range m.machinesCache().Docs {
		have[d.Name] = true
	}
	if have[machines.DocPrefix+liar] {
		t.Errorf("the future-dated document was kept")
	}
	for n := range machines.MaxDocs {
		if !have[machines.DocPrefix+otherPC(n)] {
			t.Errorf("real PC %d was evicted", n)
		}
	}
}
