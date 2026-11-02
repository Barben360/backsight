package indexgen

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

// The collection's daily volumes, as on the author's PC (a bank of about
// 11 000 memories after a year): memories created a day, retain
// operations (one per session sent), doctor checks.
const (
	memoriesPerDay = 30
	checksPerRun   = 20
	scanEvery      = 7 // days between two secrets scans
)

// The collection families (internal/metricsfile's; the index's tests
// check they agree).
const (
	familyServerDaily   = "server-daily"
	familyDoctorHistory = "doctor-history"
	familyMemoryDocs    = "memory-docs"
	familyScanHistory   = "scan-history"
)

// CollectionFamilies lists them, for that check.
var CollectionFamilies = []string{familyServerDaily, familyDoctorHistory, familyMemoryDocs, familyScanHistory}

// llmOps are the operations of the server's LLM stats.
var llmOps = []string{"all", "retain", "consolidation", "refresh_mental_model"}

// collWriter appends collection lines to their month files, the files
// kept open until close.
type collWriter struct {
	t     testing.TB
	dir   string
	files map[string]*os.File
	bufs  map[string]*bufio.Writer
}

func (w *collWriter) line(family, kind string, ts time.Time, fields map[string]any) {
	w.t.Helper()
	fields["schema"] = 1
	fields["ts"] = ts.UTC().Format("2006-01-02T15:04:05.000Z")
	fields["kind"] = kind
	fields["v"] = "v10"
	fields["binary_version"] = "v0.1.17"
	path := filepath.Join(w.dir, family+"-"+ts.UTC().Format("2006-01")+".jsonl")
	b, ok := w.bufs[path]
	if !ok {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			w.t.Fatal(err)
		}
		w.files[path], b = f, bufio.NewWriterSize(f, 1<<16)
		w.bufs[path] = b
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := b.Write(append(raw, '\n')); err != nil {
		w.t.Fatal(err)
	}
}

func (w *collWriter) close() {
	w.t.Helper()
	for path, b := range w.bufs {
		if err := b.Flush(); err != nil {
			w.t.Fatal(err)
		}
		if err := w.files[path].Close(); err != nil {
			w.t.Fatal(err)
		}
	}
}

// writeCollection writes the four collection families over the months of
// s: one jobs maintain run a day at 01:30 UTC from the second day of the
// first month to end, each writing the day's snapshot, the previous UTC
// day's server counts, a doctor run, and the previous day's memory list
// (its memories, then its listed line) with the retain operations of its
// sessions; the first run also writes the complete line of the day
// before the first, so the record covers the bank; a scan every week.
// Its own random stream leaves the telemetry and journal lines as they
// are without it.
func writeCollection(t testing.TB, d paths.Dirs, s Spec, end time.Time) {
	t.Helper()
	if err := os.MkdirAll(d.MetricsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(s.Seed, s.Seed^0x636f6c6c))
	w := &collWriter{t: t, dir: d.MetricsDir(), files: map[string]*os.File{}, bufs: map[string]*bufio.Writer{}}
	defer w.close()
	first := monthSpan(s, end)[0][0]
	memory, world, experience, observation := 0, int64(0), int64(0), int64(0)
	for day, n := first, 0; ; day, n = day.AddDate(0, 0, 1), n+1 {
		run := day.AddDate(0, 0, 1).Add(90 * time.Minute)
		if !run.Before(end) {
			return
		}
		at := func(i int) time.Time { return run.Add(time.Duration(i) * time.Millisecond) }
		i := 0
		next := func() time.Time { i++; return at(i) }
		if n == 0 {
			w.line(familyMemoryDocs, "listed", next(), map[string]any{"day": day.AddDate(0, 0, -1).Format(time.DateOnly), "n": 0, "complete": true})
		}
		// The day's memories, then its listed line.
		var nw, ne, no int64
		for k := 0; k < memoriesPerDay; k++ {
			m := map[string]any{"memory": MemID(memory), "outcome": "found", "created_day": day.Format(time.DateOnly)}
			memory++
			switch r := rng.IntN(10); {
			case r < 6:
				sid := session(day, rng.IntN(sessionsPerDay))
				m["doc_kind"], m["document"], m["session"], m["fact_type"] = "session", "conversation:"+sid, sid, []string{"world", "experience"}[rng.IntN(2)]
			case r < 7:
				m["doc_kind"], m["fact_type"] = "note", "world"
			default:
				m["doc_kind"], m["fact_type"] = "none", "observation"
			}
			switch m["fact_type"] {
			case "world":
				nw++
			case "experience":
				ne++
			default:
				no++
			}
			w.line(familyMemoryDocs, "memory", next(), m)
		}
		w.line(familyMemoryDocs, "listed", next(), map[string]any{"day": day.Format(time.DateOnly), "n": memoriesPerDay})
		for k := 0; k < sessionsPerDay; k++ {
			sid := session(day, k)
			created := day.Add(time.Duration(8+k/2) * time.Hour)
			op := map[string]any{"operation": fmt.Sprintf("op-%s-%03d", day.Format("20060102"), k), "document": "conversation:" + sid, "session": sid,
				"created_at": created.Format(time.RFC3339), "updated_at": created.Add(time.Duration(30+rng.IntN(600)) * time.Second).Format(time.RFC3339), "retries": 0}
			if rng.IntN(30) == 0 {
				op["status"], op["retries"] = "failed", 1+rng.IntN(3)
			} else {
				op["status"], op["units"] = "completed", rng.IntN(9)
			}
			w.line(familyMemoryDocs, "op", next(), op)
		}
		world, experience, observation = world+nw, experience+ne, observation+no
		w.line(familyServerDaily, "day", next(), map[string]any{"bucket": day.Format(time.RFC3339),
			"new": map[string]any{"world": nw, "experience": ne, "observation": no}, "llm": llmDays(rng)})
		projects := map[string]any{}
		for p := 0; p < 8; p++ {
			projects[fmt.Sprintf("proj-%d", p)] = int64(memory) / int64(8+p)
		}
		w.line(familyServerDaily, "snapshot", next(), map[string]any{"day": run.Format(time.DateOnly), "ok": true,
			"world": world, "experience": experience, "observation": observation, "documents": memory / 3, "links": memory * 4, "observations": observation,
			"ops":                   map[string]any{"pending": rng.IntN(3), "processing": rng.IntN(2), "completed": 15 * (n + 1), "failed": n / 30, "other": 0},
			"consolidation_pending": rng.IntN(20), "consolidation_failed": 0, "last_consolidated_at": run.Add(-time.Hour).Format(time.RFC3339), "projects": projects})
		checks := map[string]any{}
		for c := 0; c < checksPerRun; c++ {
			level := "ok"
			if rng.IntN(25) == 0 {
				level = []string{"info", "warn", "error"}[rng.IntN(3)]
			}
			checks[fmt.Sprintf("check_%02d", c)] = level
		}
		w.line(familyDoctorHistory, "run", next(), map[string]any{"checks": checks})
		if n%scanEvery == 0 {
			w.line(familyScanHistory, "scan", next(), map[string]any{"count": rng.IntN(2), "live": memory, "forgotten": memory / 50,
				"pages": 1 + memory/500, "ms": 2000 + rng.IntN(3000)})
		}
	}
}

// llmDays is a day's LLM use per operation, as the server's stats give it.
func llmDays(rng *rand.Rand) map[string]any {
	out := map[string]any{}
	for _, op := range llmOps {
		req := 5 + rng.IntN(40)
		errs := rng.IntN(3)
		out[op] = map[string]any{"input": req * 4000, "output": req * 600, "cached": req * 1000, "thoughts": req * 100, "requests": req,
			"statuses": map[string]any{"success": req - min(errs, req), "error": min(errs, req)}}
	}
	return out
}
