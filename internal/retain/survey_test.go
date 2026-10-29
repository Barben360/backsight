package retain

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
)

// BenchmarkUnsent is what jobs tick pays to count the sessions waiting
// (reach.json's sessions_waiting) when the server is silent and nothing
// has a cursor: 200 transcripts of 50 records, no retain_since, read
// from disk alone. The bench's unsent-cost records it beside the tick's
// own time on a client VM (decision 0073 block 4). Run it with
// go test ./internal/retain/ -run '^$' -bench Unsent.
func BenchmarkUnsent(b *testing.B) {
	const sessions, records = 200, 50
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	b.Cleanup(srv.Close)
	c, err := hindsight.New(srv.URL, "k", 2*time.Second)
	if err != nil {
		b.Fatal(err)
	}
	root := b.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(home, "work", "bs-m6-unsent")
	dir := filepath.Join(home, ".claude", "projects", "-bs-m6-unsent")
	for _, d := range []string{cwd, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			b.Fatal(err)
		}
	}
	at := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	files := make([]string, 0, sessions)
	for s := range sessions {
		sid := fmt.Sprintf("u%03d", s)
		var sb strings.Builder
		for i := range records {
			uuid := fmt.Sprintf("%s-r%d", sid, i+1)
			ts := at.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
			if i%2 == 0 {
				fmt.Fprintf(&sb, `{"type":"user","uuid":%q,"sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
					uuid, sid, cwd, ts, fmt.Sprintf("%s turn %d: the export job of the M6 bench failed at page %d. [rec %s]", sid, i+1, i+1, uuid))
			} else {
				fmt.Fprintf(&sb, `{"type":"assistant","uuid":%q,"sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`+"\n",
					uuid, sid, cwd, ts, fmt.Sprintf("Noted for %s: the export pages by 200. [rec %s]", sid, uuid))
			}
		}
		f := filepath.Join(dir, sid+".jsonl")
		if err := os.WriteFile(f, []byte(sb.String()), 0o600); err != nil {
			b.Fatal(err)
		}
		files = append(files, f)
	}
	d := paths.Dirs{State: filepath.Join(root, "state")}
	idx, err := LoadSentIndex(d.SentIndexDir())
	if err != nil {
		b.Fatal(err)
	}
	f := &Flusher{Client: c, Bank: "claude-memory", Dirs: d, Resolver: project.New(home, []string{".claude"}),
		MachineID: "m1", Version: "bench", Now: func() time.Time { return at.Add(time.Hour) }, Index: idx}
	b.ResetTimer()
	for b.Loop() {
		// Every session counts: a survey that read nothing would be cheap
		// and wrong.
		if n := f.Unsent(files); n != sessions {
			b.Fatalf("Unsent counted %d of %d sessions", n, sessions)
		}
	}
	b.StopTimer()
	if n := calls.Load(); n != 0 {
		b.Fatalf("Unsent called the server %d time(s)", n)
	}
}
