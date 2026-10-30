package index

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

// genSpec sizes a synthetic state directory: Months of files holding
// TelemetryPerYear and JournalPerYear lines a year, and a ledger of
// Memories ids.
type genSpec struct {
	Months           int
	TelemetryPerYear int
	JournalPerYear   int
	Memories         int
	Seed             uint64
}

// budgetSpec is the volume the budgets are measured on: 13 months, above
// a year of the author's PC (the journal is about 4 % of the telemetry
// there; 30 k lines a year is more).
var budgetSpec = genSpec{Months: 13, TelemetryPerYear: 400_000, JournalPerYear: 30_000, Memories: 5_000, Seed: 1}

// telKindMix is the share, in percent, of each telemetry kind, as on the
// author's PC: the frequent kinds first.
var telKindMix = []struct {
	kind string
	pct  int
}{
	{"prompt", 30}, {"tool", 22}, {"tool_use", 14}, {"outcome", 8}, {"reply", 8},
	{"credit_check", 5}, {"retain", 4}, {"job", 4}, {"gate", 2}, {"briefing", 1}, {"credit", 1}, {"attribution", 1},
}

func genMemID(i int) string { return fmt.Sprintf("0b4c%04x-0000-4000-8000-%012d", i, i) }

// gen writes lines for the months of g; the lines' times end at end.
type gen struct {
	rng *rand.Rand
	g   genSpec
}

func (x *gen) mem() string { return genMemID(x.rng.IntN(x.g.Memories)) }

func (x *gen) telLine(ts time.Time) map[string]any {
	n := x.rng.IntN(100)
	kind := telKindMix[0].kind
	for _, k := range telKindMix {
		if n < k.pct {
			kind = k.kind
			break
		}
		n -= k.pct
	}
	m := map[string]any{"event": kind, "ts": ts.Format(time.RFC3339Nano), "session": fmt.Sprintf("sess-%03d", x.rng.IntN(200)), "project": fmt.Sprintf("proj-%d", x.rng.IntN(8)), "binary_version": "v0.1.17"}
	switch kind {
	case "prompt":
		m["decision"] = []string{"recall", "skip"}[x.rng.IntN(2)]
		m["reason"] = promptReasons[x.rng.IntN(len(promptReasons))]
		m["plen"], m["above"], m["injected"], m["ms"], m["top"] = x.rng.IntN(500), x.rng.IntN(8), x.rng.IntN(5), x.rng.IntN(900), x.rng.Float64()
		m["ids"] = []string{x.mem(), x.mem(), x.mem()}
		m["lang"] = "en"
		m["n"] = x.rng.IntN(40)
		m["scopes"] = map[string]any{"project": map[string]any{"ok": true, "ms": 40, "raw": 6}, "global": map[string]any{"ok": true, "ms": 55, "raw": 9}}
	case "tool":
		m["name"], m["ms"], m["hits"], m["ok"] = "memory_search", x.rng.IntN(900), x.rng.IntN(10), true
	case "tool_use":
		m["name"], m["ids"], m["subagent"], m["n"] = "Edit", x.rng.IntN(3), false, x.rng.IntN(40)
		m["memory_ids"] = []string{x.mem()}
	case "outcome":
		m["ids"], m["searched"], m["credited"], m["n"] = []string{x.mem(), x.mem()}, true, "exact", x.rng.IntN(40)
	case "reply":
		m["credit"], m["searched"], m["n"] = "free", false, x.rng.IntN(40)
	case "credit_check":
		m["outcome"], m["shown"] = "declared", x.rng.IntN(6)
	case "retain":
		m["document"], m["items"], m["status"], m["ms"] = fmt.Sprintf("doc-%d", x.rng.IntN(5000)), x.rng.IntN(9), "ok", x.rng.IntN(3000)
	case "job":
		m["name"], m["status"], m["ms"] = "tick", "done", x.rng.IntN(5000)
	case "gate":
		m["decision"], m["reason"], m["n"] = "allow", "searched", x.rng.IntN(40)
	case "briefing":
		m["ids"], m["chars"], m["source"] = []string{x.mem(), x.mem()}, 900, "observations"
	case "credit":
		m["verdict"], m["ids_n"], m["ok"] = "used", 2, true
	case "attribution":
		m["turn"], m["source"] = fmt.Sprintf("turn-%d", x.rng.IntN(40)), "cwd"
	}
	return m
}

func (x *gen) journalLine(ts time.Time, seq int) map[string]any {
	m := map[string]any{"schema": 1, "seq": seq, "ts": ts.Format(time.RFC3339Nano),
		"session": fmt.Sprintf("sess-%03d", x.rng.IntN(200)), "turn": fmt.Sprintf("turn-%d", x.rng.IntN(40)), "project": "proj-1"}
	switch n := x.rng.IntN(10); {
	case n < 6:
		m["kind"], m["path"] = "shows", "prompt"
		m["items"] = []map[string]any{{"id": x.mem(), "rank": 1, "reranker": x.rng.Float64()}, {"id": x.mem(), "rank": 2, "reranker": x.rng.Float64()}, {"id": x.mem(), "rank": 3, "reranker": x.rng.Float64()}}
	case n < 8:
		m["kind"], m["ids"], m["verdict"] = "credit", []string{x.mem()}, "used"
	case n < 9:
		m["kind"], m["n"] = "turn", x.rng.IntN(40)
	default:
		m["kind"], m["outcome"], m["stop_check"] = "stop", "declared", "ok"
	}
	return m
}

// monthSpan is the months of g, oldest first, as [start, end) pairs, the
// last one cut at end.
func monthSpan(g genSpec, end time.Time) [][2]time.Time {
	first := time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -(g.Months - 1), 0)
	var out [][2]time.Time
	for i := 0; i < g.Months; i++ {
		s := first.AddDate(0, i, 0)
		e := s.AddDate(0, 1, 0)
		if e.After(end) {
			e = end
		}
		out = append(out, [2]time.Time{s, e})
	}
	return out
}

// genState fills stateDir with Months of telemetry and journal files, and
// the ledger.json of g.Memories memories. The last line is before genEnd.
func genState(t testing.TB, stateDir string, g genSpec) {
	t.Helper()
	d := paths.Dirs{State: stateDir}
	x := &gen{rng: rand.New(rand.NewPCG(g.Seed, g.Seed)), g: g}
	end := genEnd()
	for _, dir := range []string{d.TelemetryDir(), d.FeedbackDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	seq := 0
	for _, sp := range monthSpan(g, end) {
		nT, nJ := g.TelemetryPerYear/12, g.JournalPerYear/12
		for _, f := range []struct {
			path string
			n    int
			line func(ts time.Time) map[string]any
		}{
			{filepath.Join(d.TelemetryDir(), "events-"+sp[0].Format("2006-01")+".jsonl"), nT, x.telLine},
			{filepath.Join(d.FeedbackDir(), "journal-"+sp[0].Format("2006-01")+".jsonl"), nJ, func(ts time.Time) map[string]any { seq++; return x.journalLine(ts, seq) }},
		} {
			fh, err := os.Create(f.path)
			if err != nil {
				t.Fatal(err)
			}
			w := bufio.NewWriterSize(fh, 1<<20)
			enc := json.NewEncoder(w)
			step := sp[1].Sub(sp[0]) / time.Duration(f.n+1)
			for i := 0; i < f.n; i++ {
				if err := enc.Encode(f.line(sp[0].Add(step * time.Duration(i+1)))); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if err := fh.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	mems := map[string]any{}
	for i := 0; i < g.Memories; i++ {
		mems[genMemID(i)] = map[string]any{"shown": 1 + i%20, "credits": float64(i%7) / 2, "credits_at": end.Format(time.RFC3339), "last_shown": end.Format(time.RFC3339)}
	}
	raw, err := json.Marshal(map[string]any{"schema": 1, "memories": mems})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.FeedbackDir(), "ledger.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// genEnd is when the generated lines end: 20 minutes before genNow, so a
// 15-minute catch-up appends lines after them.
func genEnd() time.Time { return genNow.Add(-20 * time.Minute) }

// genNow is the fixed "now" of the generated state.
var genNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// appendRecent appends nTel telemetry and nJ journal lines timed within
// the last 15 minutes before genNow, to the newest month files.
func appendRecent(t testing.TB, stateDir string, g genSpec, nTel, nJ int) {
	t.Helper()
	d := paths.Dirs{State: stateDir}
	x := &gen{rng: rand.New(rand.NewPCG(g.Seed+1, g.Seed)), g: g}
	month := genNow.Format("2006-01")
	for _, f := range []struct {
		path string
		n    int
		line func(ts time.Time, i int) map[string]any
	}{
		{filepath.Join(d.TelemetryDir(), "events-"+month+".jsonl"), nTel, func(ts time.Time, _ int) map[string]any { return x.telLine(ts) }},
		{filepath.Join(d.FeedbackDir(), "journal-"+month+".jsonl"), nJ, func(ts time.Time, i int) map[string]any { return x.journalLine(ts, 1_000_000+i) }},
	} {
		fh, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		enc := json.NewEncoder(fh)
		for i := 0; i < f.n; i++ {
			if err := enc.Encode(f.line(genNow.Add(-15*time.Minute+time.Duration(i)*time.Second), i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := fh.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
