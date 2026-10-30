package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
)

// canaries are texts every source carries where the index must never copy
// them. Each is longer than 9 bytes: a short one would match random bytes
// of the file.
var canaries = []string{
	"CANARY-PROMPT", "CANARY-PATH", "CANARY-ERROR", "CANARY-JOB", "CANARY-REASON",
	"CANARY-ENUM", "CANARY-UNKNOWN", "CANARY-CONFIG", "CANARY-MEMORY", "CANARY NOTE",
	"CANARY-JOURNAL", "CANARY-SESSION", "CANARY-NOTE", "CANARY-STATE", "CANARY-TOML",
	"CANARY-LEDGER",
}

func canaryFiles(t *testing.T, home string) []string {
	t.Helper()
	main := filepath.Join(pathsOf(home).IndexDir(), "metrics.db")
	var out []string
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		b, err := os.ReadFile(main + suffix)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

// The controls are values the index must hold: an empty or unread index
// cannot pass the scan. Neither is a word of the schema.
const (
	ctlMemory  = "mem-ctl-91c2d4"
	ctlProject = "proj-ctl-7f3a"
)

// noCanary fails when a canary is in the index files, or when a control
// is not.
func noCanary(t *testing.T, step, home string) {
	t.Helper()
	files := canaryFiles(t, home)
	if len(files) == 0 {
		t.Errorf("%s: no index file to scan", step)
	}
	for _, b := range files {
		for _, c := range canaries {
			if strings.Contains(b, c) {
				t.Errorf("%s: %q found in the index file bytes", step, c)
			}
		}
	}
	joined := strings.Join(files, "\x00")
	for _, want := range []string{ctlMemory, ctlProject} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s: control %q not in the index file bytes", step, want)
		}
	}
}

// Free text of every source stays out of the index file: built through the
// job's own path, rebuilt, vacuumed, and read back by `stats index --json`.
func TestIndexHoldsNoCanary(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	at := time.Now().UTC().Add(-time.Hour)
	ts := at.Format(time.RFC3339Nano)
	month := monthOf(at)
	const mem = ctlMemory

	tel := strings.Join([]string{
		`{"event":"prompt","ts":"` + ts + `","session":"s1","project":"` + ctlProject + `","decision":"inject","prompt":"CANARY-PROMPT","cwd":"/home/u/CANARY-PATH/proj","reason":"CANARY-ENUM","ids":["` + mem + `"]}`,
		`{"event":"feedback_error","ts":"` + ts + `","session":"s1","reason":"CANARY-REASON","error":"CANARY-ERROR"}`,
		`{"event":"job","ts":"` + ts + `","name":"tick","status":"ok","ms":5,"details":{"shared_error":"CANARY-JOB"}}`,
		`{"event":"replace_unconfirmed","ts":"` + ts + `","session":"s1","reason":"CANARY-REASON"}`,
		`{"event":"mystery_kind","ts":"` + ts + `","session":"s1","text":"CANARY-UNKNOWN"}`,
		`{"event":"config","ts":"` + ts + `","model":"CANARY-CONFIG","top_k":5}`,
		``,
	}, "\n")
	writeTelemetryRaw(t, d.TelemetryDir(), month, tel)

	jr := strings.Join([]string{
		`{"schema":1,"seq":1,"ts":"` + ts + `","kind":"shows","session":"s1","turn":"t1","project":"` + ctlProject + `","path":"prompt","items":[{"id":"` + mem + `","rank":1,"text":"CANARY-MEMORY"},{"id":"mem-allowed-2","rank":2},{"id":"mem-allowed-3","rank":3}]}`,
		`{"schema":1,"seq":2,"ts":"` + ts + `","kind":"hide","session":"s1","ids":["` + mem + `"],"verdict":"wrong","note":"CANARY NOTE"}`,
		`{"schema":1,"seq":3,"ts":"` + ts + `","kind":"surprise","session":"s1","text":"CANARY-JOURNAL"}`,
		``,
	}, "\n")
	writeTelemetryRaw(t, d.FeedbackDir(), "", "")
	if err := os.WriteFile(filepath.Join(d.FeedbackDir(), "journal-"+month+".jsonl"), []byte(jr), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := `{"schema":1,"memories":{"` + mem + `":{"shown":1,"hidden":{"since":"` + ts + `","verdict":"wrong","note":"CANARY-LEDGER"}}}}`
	if err := os.WriteFile(filepath.Join(d.FeedbackDir(), "ledger.json"), []byte(ledger), 0o600); err != nil {
		t.Fatal(err)
	}

	writeTelemetryRaw(t, d.SessionsDir(), "", "")
	// Tripwires, not coverage: no ingest path reads sessions/*.json or
	// config.toml today, so these two canaries cannot reach the index now.
	// They fail the test if a future change starts reading those files
	// and copies their text.
	sess := `{"injections":[{"id":"` + mem + `","text":"CANARY-SESSION"}],"note":"CANARY-SESSION"}`
	if err := os.WriteFile(filepath.Join(d.SessionsDir(), "s1.json"), []byte(sess), 0o600); err != nil {
		t.Fatal(err)
	}
	note := `{"operation_id":"op1","document_id":"doc1","kind":"fact","project":"` + ctlProject + `","content":"CANARY-NOTE","created_at":"` + ts + `"}` + "\n"
	for _, p := range []string{notesOutbox(d).Path, notesOutbox(d).QuarantinePath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(note), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state := `{"update_check":{"checked_at":"` + ts + `","source":"github","installed":"v0.1.0","summary":"CANARY-STATE","error":"CANARY-STATE"}}`
	if err := os.WriteFile(d.StateFile(), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[llm]\nmodel = \"CANARY-TOML\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Before any checkpoint: an ingest through CatchUp alone, the pages
	// still in the -wal (the index stays open while the files are read).
	pre, err := index.Open(context.Background(), indexOptions(d, home, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre.CatchUp(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	noCanary(t, "before the checkpoint", home)
	if err := pre.Close(); err != nil {
		t.Fatal(err)
	}

	if got := tickIndex(context.Background(), d, home); !strings.HasPrefix(got, "caught up") {
		t.Fatalf("tick: %s", got)
	}
	noCanary(t, "after the tick", home)

	if code, out, errOut := run(t, "stats", "index", "rebuild", "--yes"); code != output.OK {
		t.Fatalf("rebuild: exit %d\n%s%s", code, out, errOut)
	}
	noCanary(t, "after the rebuild", home)

	db, err := sql.Open("sqlite", filepath.Join(d.IndexDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("VACUUM"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	noCanary(t, "after VACUUM", home)

	code, out, errOut := run(t, "stats", "index", "--json")
	if code != output.OK {
		t.Fatalf("stats index: exit %d\n%s%s", code, out, errOut)
	}
	for _, c := range canaries {
		if strings.Contains(out+errOut, c) {
			t.Errorf("%q in `stats index --json`", c)
		}
	}
}

// writeTelemetryRaw writes dir/events-<month>.jsonl (month "": only the
// directory is made).
func writeTelemetryRaw(t *testing.T, dir, month, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if month == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "events-"+month+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
