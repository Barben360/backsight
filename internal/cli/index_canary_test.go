package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
)

// canaries are texts every source carries where the index must never copy
// them. Each is longer than 9 bytes: a short one would match random bytes
// of the file.
var canaries = []string{
	"CANARY-PROMPT", "CANARY-PATH", "CANARY-ERROR", "CANARY-JOB", "CANARY-REASON",
	"CANARY-ENUM", "CANARY-UNKNOWN", "CANARY-CONFIG", "CANARY-MEMORY", "CANARY NOTE",
	"CANARY-JOURNAL", "CANARY-SESSION", "CANARY-NOTE", "CANARY-STATE", "CANARY-TOML",
	"CANARY-LEDGER",
	// a folded retire's free text: its note and the project of the memory
	// it retired
	"CANARY RETIRE NOTE", "CANARY-RETIRE-PROJECT",
	// what the server holds: a listed memory's fields, a document's text,
	// the path of an imported document, an operation's error, a scanned
	// memory, a doctor check's summary; and the config's strings
	"CANARY-LISTED-TEXT", "CANARY-LISTED-CONTEXT", "CANARY-LISTED-TAG", "CANARY-LISTED-ENTITY",
	"CANARY-LISTED-META", "CANARY-LISTED-CHUNK", "CANARY-DOC-TEXT", "CANARY-DIR", "CANARY-FILE",
	"CANARY-OPERROR", "CANARY-SCAN-TEXT", "CANARY-DOCTOR", "CANARY-CFG-",
	// briefing pages: their text and their ids; a note document's id
	"CANARY-BRIEF-TEXT", "CANARY-BRIEF-CLEAN", briefFlagged, briefClean, noteDocument, noteDocumentID,
}

// The briefing pages the server holds (one the scan flags, one it does not)
// and the note document of the day lists: ids that must reach no file
// backsight keeps, however they are written.
var (
	briefFlagged   = retain.BriefingModelID("brief-ctl-3d")
	briefClean     = retain.BriefingModelID("brief-ctl-5a")
	noteDocumentID = "4f2a9c61d0b7e358"
	noteDocument   = "note:" + noteDocumentID
)

// configCanary is the prefix of the config's canary strings: in
// config.toml alone.
const configCanary = "CANARY-CFG-"

// serverCanaries are the canaries only the server holds (and the config's
// strings, which are in config.toml alone): nothing backsight writes
// outside that file may carry one.
var serverCanaries = []string{
	"CANARY-LISTED-TEXT", "CANARY-LISTED-CONTEXT", "CANARY-LISTED-TAG", "CANARY-LISTED-ENTITY",
	"CANARY-LISTED-META", "CANARY-LISTED-CHUNK", "CANARY-DOC-TEXT", "CANARY-DIR", "CANARY-FILE",
	"CANARY-OPERROR", "CANARY-SCAN-TEXT", "CANARY-BRIEF-TEXT", "CANARY-BRIEF-CLEAN", briefFlagged, briefClean, noteDocument, noteDocumentID,
}

// canaryFiles are the bytes of the index file and its companions.
func canaryFiles(t *testing.T, home string) []string {
	t.Helper()
	return dbFileBytes(t, filepath.Join(pathsOf(home).IndexDir(), "metrics.db"))
}

// ledgerFiles are the bytes of ledger.db and its companions.
func ledgerFiles(t *testing.T, home string) []string {
	t.Helper()
	return dbFileBytes(t, filepath.Join(pathsOf(home).FeedbackDir(), "ledger.db"))
}

func dbFileBytes(t *testing.T, main string) []string {
	t.Helper()
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
	// the document id a hide's note is: a word of the ledger database,
	// which keeps ids, numbers and the verdict word, never a text
	ctlHideNote = "doc-ctl-e9d41c7a"
	// a memory a folded retire counts: its row is in ledger.db, its
	// line's texts are not
	ctlRetired = "mem-retired-ctl-4c7e"
)

// noCanary fails when a canary is in the index files, or when a control
// (the two above and the extra ones) is not.
func noCanary(t *testing.T, step, home string, extra ...string) {
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
	for _, want := range append([]string{ctlMemory, ctlProject}, extra...) {
		if !strings.Contains(joined, want) {
			t.Errorf("%s: control %q not in the index file bytes", step, want)
		}
	}
	// ledger.db, built from a journal that carries the same canaries,
	// holds none of them, and holds the ids and the verdict word it keeps.
	lfiles := ledgerFiles(t, home)
	if len(lfiles) == 0 {
		t.Errorf("%s: no ledger.db to scan", step)
	}
	for _, b := range lfiles {
		for _, c := range canaries {
			if strings.Contains(b, c) {
				t.Errorf("%s: %q found in the ledger.db bytes", step, c)
			}
		}
	}
	ljoined := strings.Join(lfiles, "\x00")
	for _, want := range []string{ctlMemory, ctlRetired, ctlHideNote, "wrong"} {
		if !strings.Contains(ljoined, want) {
			t.Errorf("%s: control %q not in the ledger.db bytes", step, want)
		}
	}
}

// canaryState writes every source with its canaries on a fresh install and
// returns its home.
func canaryState(t *testing.T) string {
	t.Helper()
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
		// two settings records that differ: a marker, whose key may only be a numeric setting and whose canary key must not come through
		`{"event":"config","ts":"` + at.Add(-2*time.Minute).Format(time.RFC3339Nano) + `","sha256":"` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `","recall.min_score":0.5,"CANARY-KEY":1}`,
		`{"event":"config","ts":"` + at.Add(-time.Minute).Format(time.RFC3339Nano) + `","sha256":"` + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + `","recall.min_score":0.6,"CANARY-KEY":2}`,
		`{"event":"prompt","ts":"` + at.Add(-3*time.Minute).Format(time.RFC3339Nano) + `","session":"s1","project":"` + ctlProject + `","decision":"skip","binary_version":"v0.0.1"}`,
		``,
	}, "\n")
	writeTelemetryRaw(t, d.TelemetryDir(), month, tel)

	jr := strings.Join([]string{
		`{"schema":1,"seq":1,"ts":"` + ts + `","kind":"shows","session":"s1","turn":"t1","project":"` + ctlProject + `","path":"prompt","items":[{"id":"` + mem + `","rank":1,"text":"CANARY-MEMORY"},{"id":"mem-allowed-2","rank":2},{"id":"mem-allowed-3","rank":3}]}`,
		`{"schema":1,"seq":2,"ts":"` + ts + `","kind":"hide","session":"s1","ids":["` + mem + `"],"verdict":"wrong","note":"` + ctlHideNote + `"}`,
		// a retire the ledger folds (it counts the memory wrong), its texts
		// kept out of ledger.db
		`{"schema":1,"seq":5,"ts":"` + ts + `","kind":"retire","session":"s1","project":"` + ctlProject + `","ids":["` + ctlRetired + `"],"verdict":"wrong","outcome":"invalidated","note":"CANARY RETIRE NOTE","memory_project":"CANARY-RETIRE-PROJECT"}`,
		// a note's text on a line the ledger never folds
		`{"schema":1,"seq":4,"ts":"` + ts + `","kind":"stale","session":"s1","ids":["mem-allowed-2"],"note":"CANARY NOTE"}`,
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
	// ledger.db, folded from that journal as the tick would
	w, err := ledgerdb.OpenWriter(context.Background(), ledgerdb.Options{Dirs: d, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Fold(context.Background(), feedback.Params{}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
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
	return home
}

// Free text of every source stays out of the index file: built through the
// job's own path, rebuilt, vacuumed, and read back by `stats index --json`.
func TestIndexHoldsNoCanary(t *testing.T) {
	home := canaryState(t)
	d := pathsOf(home)
	// Before any checkpoint: an ingest through CatchUp alone, the pages
	// still in the -wal (the index stays open while the files are read).
	pre, err := index.Open(context.Background(), indexOptions(d, home, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre.CatchUp(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(pre.Path() + "-wal"); err != nil || st.Size() == 0 {
		t.Fatalf("the -wal holds nothing before the checkpoint: %v", err)
	}
	noCanary(t, "before the checkpoint", home)
	if err := pre.Close(); err != nil {
		t.Fatal(err)
	}

	if got := tickIndex(context.Background(), d, home); !strings.HasPrefix(got, "caught up") {
		t.Fatalf("tick: %s", got)
	}
	noCanary(t, "after the tick", home)

	// The collection: what the job and the scan write, ingested.
	canaryServer(t, home)
	canaryCollect(t, home)
	controls := []string{ctlListed, ctlSessionA}
	noCanary(t, "after the collection", home, controls...)
	noCanaryInMetrics(t, home)
	noServerTextOnDisk(t, "after the collection", home)
	theCollectedTablesFilled(t, "after the collection", home)
	configEventHoldsNoString(t, home)

	// ledger.db folded afresh from the journal: the rebuild's file holds
	// no canary either
	if code, out, errOut := run(t, "feedback", "rebuild", "--json"); code != output.OK {
		t.Fatalf("feedback rebuild: exit %d\n%s%s", code, out, errOut)
	}
	noCanary(t, "after the ledger rebuild", home, controls...)

	if code, out, errOut := run(t, "stats", "index", "rebuild", "--yes"); code != output.OK {
		t.Fatalf("rebuild: exit %d\n%s%s", code, out, errOut)
	}
	noCanary(t, "after the rebuild", home, controls...)
	theCollectedTablesFilled(t, "after the rebuild", home)

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
	noCanary(t, "after VACUUM", home, controls...)

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

// What the collection fixture holds that the checks look for: the ids of
// the memories and sessions the server lists (they must reach the record:
// they are controls), and the ids that must not reach the scan history.
const (
	ctlListed   = "mem-listed-ctl-5e8a" // a memory of a session document, one per day
	ctlSessionA = "s-ctl-2b7e"          // a session whose retain completed, with units
	ctlSessionB = "s-ctl-9d41"          // completed, the server gives no count
	ctlSessionC = "s-ctl-c3f0"          // failed
	ctlScanned  = "mem-scan-ctl-77b1"   // the memory the scan flags
)

// fixtureIDs are every memory id of the fixture.
// (and the ids of the listed memories and of the briefing pages, by prefix).
var fixtureIDs = []string{ctlMemory, ctlRetired, "mem-allowed-2", "mem-allowed-3", ctlScanned, ctlListed, "mem-import-ctl", "mem-note-ctl", "mem-obs-ctl", briefFlagged, briefClean}

// canaryConfig writes config.toml: the fake server's address, backups and
// the update source local, and a canary in every string setting that takes
// any text (a setting with a closed set or a shape keeps its default, the
// config's own validation decides). It returns how many strings carry one.
func canaryConfig(t *testing.T, home string, port int) int {
	t.Helper()
	d := pathsOf(home)
	cfg := config.Default(d, home)
	cfg.Server.URL = "http://127.0.0.1:" + strconv.Itoa(port)
	cfg.Server.UIPort = 19999
	cfg.Backup.Enabled = false
	n := 0
	sections := reflect.ValueOf(&cfg).Elem()
	for i := range sections.NumField() {
		sec := sections.Field(i)
		if sec.Kind() != reflect.Struct {
			continue
		}
		for j := range sec.NumField() {
			f := sec.Field(j)
			name := "CANARY-CFG-" + sections.Type().Field(i).Name + "-" + sec.Type().Field(j).Name
			switch sections.Type().Field(i).Name + "." + sec.Type().Field(j).Name {
			case "Server.URL", "Server.Bank":
				continue // the server this test talks to
			case "LLM.Currency":
				continue // the cost views show it by design: the unit of the amounts
			case "Update.Source":
				continue // a failed update check names its source in state.json and the usage log, as it always has
			}
			var old reflect.Value
			switch f.Kind() {
			case reflect.String:
				old = reflect.ValueOf(f.String())
				f.SetString(name)
			case reflect.Slice:
				if f.Type().Elem().Kind() != reflect.String {
					continue
				}
				old = reflect.ValueOf(f.Interface())
				f.Set(reflect.Append(f, reflect.ValueOf(name)))
			default:
				continue
			}
			if err := cfg.Validate(); err != nil && f.Kind() == reflect.String {
				// a path setting wants an absolute path: the canary under the home
				f.SetString(filepath.Join(home, name))
			}
			if err := cfg.Validate(); err != nil {
				f.Set(old)
				continue
			}
			n++
		}
	}
	// the tripwire of the first fixture: a model name no ingest path reads
	cfg.LLM.Model = "CANARY-TOML"
	raw, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(raw, "config.toml", d, home); err != nil {
		t.Fatalf("the canary config does not load: %v", err)
	}
	if err := os.WriteFile(d.ConfigFile(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return n
}

// canaryServer serves the fixture's server: the day lists of the two days
// before today (a session document, an import document whose id is a path
// of canary parts, a note, an observation without a document), three retain
// operations of sessions this PC sent, a memory the scan flags, and canary
// text in every field a read could decode; and points the install at it,
// with a canary in every string setting.
func canaryServer(t *testing.T, home string) {
	t.Helper()
	port := fakeHindsight(t)
	if n := canaryConfig(t, home, port); n < 4 {
		t.Fatalf("only %d string settings carry a canary", n)
	} else {
		t.Logf("%d string settings carry a canary", n)
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	for k := 2; k >= 1; k-- {
		at := day.AddDate(0, 0, -k).Add(12 * time.Hour)
		suffix := "-" + strconv.Itoa(k)
		fakeCreated = append(fakeCreated,
			fakeMem{ID: ctlListed + suffix, FactType: "world", Document: "conversation:" + ctlSessionA, Created: at},
			fakeMem{ID: "mem-import-ctl" + suffix, FactType: "world", Document: "import:CANARY-DIR/CANARY-FILE.md", Created: at.Add(time.Minute)},
			fakeMem{ID: "mem-note-ctl" + suffix, FactType: "experience", Document: noteDocument, Created: at.Add(2 * time.Minute)},
			fakeMem{ID: "mem-obs-ctl" + suffix, FactType: "observation", Created: at.Add(3 * time.Minute)})
	}
	projectEntities[ctlProject] = "[]"
	sent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	op := func(id, status, sid string) string {
		return `{"id":"` + id + `","operation_type":"retain","status":"` + status + `","document_id":"conversation:` + sid + `","created_at":"` + sent +
			`","updated_at":"` + sent + `","retry_count":1,"error_message":"CANARY-OPERROR"}`
	}
	fakeOperations = []string{op("op-ctl-a", "completed", ctlSessionA), op("op-ctl-b", "completed", ctlSessionB), op("op-ctl-c", "failed", ctlSessionC)}
	documentUnits["conversation:"+ctlSessionA] = 3
	documentNoUnits["conversation:"+ctlSessionB] = true
	// a memory whose text is a secret's shape: assembled, not a literal
	// secret for gitleaks
	listAnswer["world"] = `[{"id":"` + ctlScanned + `","text":"CANARY-SCAN-TEXT PGPASSWORD=Zz9y` + `X8wV7u","fact_type":"world","tags":["project:` + ctlProject + `"]}]`

	// briefing pages: one quotes a secret's shape (the scan flags it), one
	// does not; both carry canary text
	mentalModels[briefFlagged] = `{"id":"` + briefFlagged + `","content":"CANARY-BRIEF-TEXT PGPASSWORD=Zz9y` + `X8wV7u","tags":["project:brief-ctl-3d"]}`
	mentalModels[briefClean] = `{"id":"` + briefClean + `","content":"CANARY-BRIEF-CLEAN","tags":["project:brief-ctl-5a"]}`

	// this PC sent the three sessions' documents an hour ago
	d := pathsOf(home)
	at := time.Now().UTC().Add(-time.Hour)
	f, err := os.OpenFile(filepath.Join(d.TelemetryDir(), "events-"+monthOf(at)+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, sid := range []string{ctlSessionA, ctlSessionB, ctlSessionC} {
		line := `{"event":"retain","ts":"` + at.Format(time.RFC3339Nano) + `","session":"` + sid + `","document":"conversation:` + sid + `","items":1,"status":"ok","ms":3}` + "\n"
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
}

// canaryCollect runs the collection the way the daily job does: the index
// built and caught up, `jobs maintain` (the doctor, the server's daily
// counts, the memory documents, the config event), `memory scan`, and the
// index caught up with what they wrote. The doctor's one check carries a
// canary in its summary.
func canaryCollect(t *testing.T, home string) {
	t.Helper()
	d := pathsOf(home)
	if got := tickIndex(context.Background(), d, home); !strings.HasPrefix(got, "caught up") {
		t.Fatalf("tick before the collection: %s", got)
	}
	fixedMaintainDoctor(t, doctor.Check{ID: "server", Status: doctor.Warn, Summary: "CANARY-DOCTOR", Detail: "CANARY-DOCTOR", Fix: "backsight doctor"})
	code, out, errOut := run(t, "jobs", "maintain", "--json")
	if code != output.OK {
		t.Fatalf("jobs maintain: exit %d\n%s%s", code, out, errOut)
	}
	var env struct {
		Data struct {
			DoctorHistory string `json:"doctor_history"`
			ServerDaily   string `json:"server_daily"`
			MemoryDocs    string `json:"memory_docs"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.Data.DoctorHistory, "1 check") || !strings.HasPrefix(env.Data.ServerDaily, "server daily: snapshot") ||
		!strings.Contains(env.Data.MemoryDocs, "day(s) listed") || !strings.Contains(env.Data.MemoryDocs, "3 operation(s)") {
		t.Fatalf("the collection did not run: %+v", env.Data)
	}
	if code, out, errOut := run(t, "memory", "scan"); code != output.OK || !strings.Contains(out, ctlScanned) || !strings.Contains(out, briefFlagged+" (briefing page") || strings.Contains(out, briefClean) {
		t.Fatalf("memory scan: exit %d\n%s%s", code, out, errOut)
	}
	if got := tickIndex(context.Background(), d, home); !strings.HasPrefix(got, "caught up") {
		t.Fatalf("tick after the collection: %s", got)
	}
}

// metricsFilesOf is every file under <state>/metrics by name.
func metricsFilesOf(t *testing.T, home string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(pathsOf(home).MetricsDir(), func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		out[filepath.Base(p)] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// noCanaryInMetrics fails when a canary is in a metrics file, a fixture's
// memory id is in the scan history, or a control is not in the record: an
// empty or unwritten record cannot pass.
func noCanaryInMetrics(t *testing.T, home string) {
	t.Helper()
	files := metricsFilesOf(t, home)
	var all []string
	for name, b := range files {
		all = append(all, b)
		for _, c := range canaries {
			if strings.Contains(b, c) {
				t.Errorf("%q in metrics/%s", c, name)
			}
		}
		if strings.HasPrefix(name, "scan-history-") {
			for _, id := range fixtureIDs {
				if strings.Contains(b, id) {
					t.Errorf("the memory id %q in metrics/%s", id, name)
				}
			}
		}
	}
	joined := strings.Join(all, "\n")
	for _, ctl := range []string{ctlListed, ctlSessionA, `"doc_kind":"import"`, `"doc_kind":"note"`, `"doc_kind":"none"`, `"kind":"run"`, `"kind":"snapshot"`, `"kind":"scan"`} {
		if !strings.Contains(joined, ctl) {
			t.Errorf("control %s not in the metrics files:\n%s", ctl, joined)
		}
	}
	for _, family := range []string{"server-daily-", "doctor-history-", "memory-docs-", "scan-history-"} {
		found := false
		for name := range files {
			found = found || strings.HasPrefix(name, family)
		}
		if !found {
			t.Errorf("no %s file written", family)
		}
	}
}

// noServerTextOnDisk fails when a canary only the server holds (or the
// import document's path) is in any file under the install's home: what
// backsight keeps never copies it. The config's canaries are in config.toml
// alone: any other file holding one copied a setting's text.
func noServerTextOnDisk(t *testing.T, step, home string) {
	t.Helper()
	files := 0
	err := filepath.WalkDir(home, func(p string, de fs.DirEntry, err error) error {
		if err != nil || !de.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if p == pathsOf(home).ConfigFile() {
			return nil
		}
		files++
		for _, c := range append([]string{configCanary}, serverCanaries...) {
			if strings.Contains(string(b), c) {
				t.Errorf("%s: %q in %s", step, c, strings.TrimPrefix(p, home))
			}
		}
		return nil
	})
	if err != nil || files == 0 {
		t.Fatalf("%s: scanned %d files: %v", step, files, err)
	}
}

// indexRows counts a table's rows in the index file.
func indexRows(t *testing.T, home, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(pathsOf(home).IndexDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// theCollectedTablesFilled: the index holds what the record wrote, so a
// scan of it is a scan of the collected data.
func theCollectedTablesFilled(t *testing.T, step, home string) {
	t.Helper()
	for _, table := range []string{"server_snapshots", "doctor_runs", "memory_docs", "memory_days", "doc_ops", "scans"} {
		if indexRows(t, home, table) == 0 {
			t.Errorf("%s: no row in %s", step, table)
		}
	}
}

// configEventHoldsNoString: the config events the job wrote (the ones with
// a hash) carry no string but the common fields (the event's name, its
// time, the binary's version, the empty session, project and instruction
// set of a job's event) and the hash.
func configEventHoldsNoString(t *testing.T, home string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(pathsOf(home).TelemetryDir(), "events-*.jsonl"))
	seen := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) != nil || ev["event"] != "config" || ev["sha256"] == nil {
				continue
			}
			seen++
			for k, v := range ev {
				str, isString := v.(string)
				switch {
				case !isString, k == "event", k == "ts", k == "sha256":
				case (k == "session" || k == "project" || k == "v") && str == "":
				case k == "binary_version" && str == buildinfo.Version:
				default:
					t.Errorf("config event: string value under %q: %q", k, str)
				}
			}
			if h, _ := ev["sha256"].(string); len(h) != 64 {
				t.Errorf("config event: sha256 %q", h)
			}
		}
	}
	if seen == 0 {
		t.Error("no config event was written by the job")
	}
}
