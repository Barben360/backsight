package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// gateServer is a server for the extraction gate: the latest failed and
// completed retain (an RFC 3339 time, "" for none), the dry-run's answer,
// and whether the operations list fails. probes counts the dry runs.
type gateServer struct {
	failedAt, completedAt string
	probeStatus           int
	probeBody             string
	opsDown               bool
	probes                atomic.Int64
}

func (g *gateServer) client(t *testing.T) *hindsight.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/operations"):
			if g.opsDown {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			at := map[string]string{"failed": g.failedAt, "completed": g.completedAt}[r.URL.Query().Get("status")]
			if at == "" || r.URL.Query().Get("type") != "retain" {
				_, _ = w.Write([]byte(`{"total":0,"operations":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"total":1,"operations":[{"id":"op","operation_type":"retain","status":"` + r.URL.Query().Get("status") + `","created_at":"` + at + `","updated_at":"` + at + `"}]}`))
		case strings.HasSuffix(r.URL.Path, "/memories/dry-run-extract"):
			g.probes.Add(1)
			if g.probeStatus != 0 {
				w.WriteHeader(g.probeStatus)
				_, _ = w.Write([]byte(g.probeBody))
				return
			}
			_, _ = w.Write([]byte(`{"facts":[{"text":"The billing service keeps PostgreSQL 16 for partial indexes.","fact_type":"world"}],"usage":{"input_tokens":900,"output_tokens":40}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	cl, err := hindsight.New(srv.URL, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

const gateBank = "backsight"

var gateNow = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func gateDirs(t *testing.T) paths.Dirs {
	return paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
}

// A rejected key holds the sends, says so and records it; the tick after
// the key is fixed probes again, and the pass lifts the hold.
func TestExtractionGateHoldsOnARejectedKeyThenLifts(t *testing.T) {
	d := gateDirs(t)
	g := &gateServer{failedAt: "2026-11-02T08:50:00Z", completedAt: "2026-11-01T08:00:00Z",
		probeStatus: http.StatusInternalServerError, probeBody: `{"detail":"Fact extraction failed: Error code: 401 - Incorrect API key provided"}`}
	cl := g.client(t)
	hold := extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow)
	if !strings.Contains(hold, "rejects llm_api_key") || !strings.Contains(hold, "2026-11-02 09:00 UTC") {
		t.Fatalf("hold: %q", hold)
	}
	if e := state.LoadExtraction(d); !e.HeldFor(gateBank) || !e.KeyRejected {
		t.Fatalf("not recorded: %+v", e)
	}
	if got := extractionHold(d, gateBank); got != hold {
		t.Errorf("the record reads %q, the tick said %q", got, hold)
	}
	// Still rejected ten minutes later: held, since the first tick.
	if again := extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow.Add(10*time.Minute)); again != hold {
		t.Errorf("second tick: %q", again)
	}
	g.probeStatus = 0 // the key is fixed
	if got := extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow.Add(20*time.Minute)); got != "" {
		t.Fatalf("a passing probe still holds: %q", got)
	}
	if e := state.LoadExtraction(d); e.HeldFor(gateBank) || extractionHold(d, gateBank) != "" {
		t.Errorf("hold not lifted: %+v", e)
	}
	if n := g.probes.Load(); n != 3 {
		t.Errorf("%d probes, want one per tick (3)", n)
	}
}

// Another provider failure (credit, model) holds too, with its own words;
// the server refusing this PC's own key (401 on the dry run itself) or no
// answer holds nothing.
func TestExtractionGateProviderFailureKinds(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string // "" for no hold
	}{
		{"credit", http.StatusInternalServerError, `{"detail":"Fact extraction failed: Error code: 402 - Insufficient Balance"}`, "extraction LLM fails"},
		{"own key refused", http.StatusUnauthorized, `{"detail":"invalid token"}`, ""},
		{"forbidden", http.StatusForbidden, `{"detail":"read-only token"}`, ""},
		{"no fact", 0, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := gateDirs(t)
			g := &gateServer{failedAt: "2026-11-02T08:50:00Z", probeStatus: c.status, probeBody: c.body}
			hold := extractionGate(context.Background(), d, g.client(t), gateBank, "openai", gateNow)
			switch {
			case c.want == "" && hold != "":
				t.Errorf("held: %q", hold)
			case c.want != "" && !strings.Contains(hold, c.want):
				t.Errorf("hold %q, want %q", hold, c.want)
			case c.want != "" && strings.Contains(hold, "llm_api_key"):
				t.Errorf("not a key problem, yet the hold names the key: %q", hold)
			}
			if g.probes.Load() != 1 {
				t.Errorf("%d probes, want 1", g.probes.Load())
			}
		})
	}
}

// No probe, no hold while the latest finished retain completed or none
// failed; a hold already on stays when the operations cannot be read, and
// goes once a retain completes after the failure.
func TestExtractionGateReadsTheLatestRetainFirst(t *testing.T) {
	d := gateDirs(t)
	g := &gateServer{}
	cl := g.client(t)
	if hold := extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow); hold != "" {
		t.Errorf("nothing failed: %q", hold)
	}
	g.failedAt, g.completedAt = "2026-11-02T08:00:00Z", "2026-11-02T08:30:00Z"
	if hold := extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow); hold != "" {
		t.Errorf("a retain completed after the failure: %q", hold)
	}
	if g.probes.Load() != 0 {
		t.Fatalf("%d probes, want none", g.probes.Load())
	}
	held := state.Extraction{Bank: gateBank, HeldSince: gateNow.Add(-time.Hour), KeyRejected: true}
	if err := state.SaveExtraction(d, held); err != nil {
		t.Fatal(err)
	}
	g.opsDown = true
	if hold := extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow); !strings.Contains(hold, "rejects llm_api_key") {
		t.Errorf("an unreadable list dropped the hold: %q", hold)
	}
	g.opsDown = false // a retain completed after the failure (another PC, or before the hold)
	if hold := extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow); hold != "" || state.LoadExtraction(d).HeldFor(gateBank) {
		t.Errorf("a completed retain kept the hold: %q", hold)
	}
	if g.probes.Load() != 0 {
		t.Errorf("%d probes, want none", g.probes.Load())
	}
}

// Without a hold, a failure no newer than the last passing probe, or a
// probe made within the quiet hour, asks no probe again: a passing probe
// costs extraction tokens.
func TestExtractionGateProbesSparingly(t *testing.T) {
	d := gateDirs(t)
	g := &gateServer{failedAt: "2026-11-02T08:50:00Z"}
	cl := g.client(t)
	if hold := extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow); hold != "" {
		t.Fatalf("a passing probe holds: %q", hold)
	}
	if hold := extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow.Add(3*time.Hour)); hold != "" || g.probes.Load() != 1 {
		t.Errorf("the same failure probed again: %q, %d probes", hold, g.probes.Load())
	}
	g.failedAt = "2026-11-02T09:10:00Z" // a new failure, within the quiet hour
	extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow.Add(20*time.Minute))
	if g.probes.Load() != 1 {
		t.Errorf("probed within the quiet hour: %d probes", g.probes.Load())
	}
	extractionGate(context.Background(), d, cl, gateBank, "openai", gateNow.Add(61*time.Minute))
	if g.probes.Load() != 2 {
		t.Errorf("a new failure after the quiet hour was not probed: %d probes", g.probes.Load())
	}
}
