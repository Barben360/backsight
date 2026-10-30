package cli

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// The runtime switch (decision 0078) between two fake runtimes, each with
// its own fake database: a dump carries the data whole, a restore
// replaces it whole. The API is fakeHindsight behind a proxy answering
// the probes' listing and recalls as the runtime that runs ranks them.

// dbData is a fake database's content: each table's rows (their key
// text), the banks and the control bank's documents' retain_params.
type dbData struct {
	Keys    map[string][]string `json:"keys"`
	Banks   []string            `json:"banks"`
	Control map[string]string   `json:"control"`
}

func sampleData() dbData {
	return dbData{
		Keys: map[string][]string{
			"documents":     {"(d1)", "(d2)", "(d3)"},
			"memory_units":  {"(m1)", "(m2)", "(m3)", "(m4)"},
			"mental_models": {"(p1)"},
			"banks":         {"(claude-memory)", "(claude-memory-control)"},
		},
		Banks:   []string{"claude-memory", "claude-memory-control"},
		Control: map[string]string{"backsight:server-id": `{"metadata": {"id": "s1"}}`, "backsight:epoch": `{"metadata": {"epoch": "2"}}`},
	}
}

type switchDB struct {
	server.Database
	rt           *switchRT
	data         dbData
	afterRestore func(*dbData) // a restore that does not give back what was dumped
}

func md5Hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

func (d *switchDB) Query(_ context.Context, sql string) (string, error) {
	switch {
	case strings.Contains(sql, "alembic_version"):
		return "abc123", nil
	case strings.Contains(sql, "pg_extension"):
		return "0.8.7", nil
	case strings.HasPrefix(sql, "/* backsight:tables */"):
		var out []map[string]any
		for _, t := range slices.Sorted(maps.Keys(d.data.Keys)) {
			out = append(out, map[string]any{"name": t, "keys": []string{"id"}})
		}
		raw, _ := json.Marshal(out)
		return string(raw), nil
	case strings.HasPrefix(sql, "/* backsight:table "):
		ks := slices.Sorted(slices.Values(d.data.Keys[strings.Fields(sql)[2]]))
		return fmt.Sprintf("%d %s", len(ks), md5Hex(strings.Join(ks, "\n"))), nil
	case strings.HasPrefix(sql, "/* backsight:keys "):
		raw, _ := json.Marshal(slices.Sorted(slices.Values(d.data.Keys[strings.Fields(sql)[2]])))
		return string(raw), nil
	case strings.HasPrefix(sql, "/* backsight:control */"):
		out := map[string]string{}
		for id, p := range d.data.Control {
			out[id] = md5Hex(p)
		}
		raw, _ := json.Marshal(out)
		return string(raw), nil
	case strings.Contains(sql, "from banks"):
		raw, _ := json.Marshal(d.data.Banks)
		return string(raw), nil
	}
	return "", fmt.Errorf("unexpected query %s", sql)
}

func (d *switchDB) counts(tables []string) map[string]int64 {
	out := map[string]int64{}
	for _, t := range tables {
		out[t] = int64(len(d.data.Keys[t]))
	}
	return out
}

func (d *switchDB) Dump(ctx context.Context, dst string, tables []string) (map[string]int64, error) {
	if err := d.rt.call(ctx, "Dump"); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(d.data)
	return d.counts(tables), os.WriteFile(dst, raw, 0o600)
}

func (d *switchDB) Restore(ctx context.Context, src string) (bool, error) {
	if err := d.rt.call(ctx, "Restore"); err != nil {
		return true, err
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	var data dbData
	if err := json.Unmarshal(raw, &data); err != nil {
		return false, err
	}
	d.data = data // the old database dropped first: nothing of it stays
	if d.afterRestore != nil {
		d.afterRestore(&d.data)
	}
	return true, nil
}

func (d *switchDB) Counts(_ context.Context, tables []string) (map[string]int64, error) {
	return d.counts(tables), nil
}
func (d *switchDB) RemoveStaleDumps(context.Context) error { return nil }
func (d *switchDB) BankIDs(context.Context) ([]string, error) {
	return slices.Clone(d.data.Banks), nil
}

// switchWorld is both runtimes, what each did, in order, and what the
// API answers.
type switchWorld struct {
	mu      sync.Mutex
	calls   []string // "<kind> <Method>", what changes something (" (cancelled)": its context was)
	reads   []string // read-only calls
	rts     map[string]*switchRT
	running string // the runtime whose API answers
	// rank is the top 10 a recall of query answers on runtime kind.
	rank        map[string]func(query string) []string
	memories    int // the bank's memories (the probes' listing)
	listTags    []string
	epochWrites int
}

type switchRT struct {
	server.Runtime
	w         *switchWorld
	kind      string
	db        *switchDB
	exists    bool
	deployed  string
	size      int64 // DataSize: the database only
	cleanSize int64 // CleanSize: everything Delete removes
	sizeErr   error // CleanSize cannot tell
	leftover  bool  // Leftover: something of it here without its data (a failed switch's downloads)
	fail      map[string]error
	hook      map[string]func(ctx context.Context) error
	apiKeys   []string // the API key each Render was given
	mayCreate bool
	missing   []string // Downloads
}

// nativeRT has what only the native runtime has: its identity and its
// creation of a first database.
type nativeRT struct{ *switchRT }

func (n nativeRT) RecordIdentity(ctx context.Context) error { return n.call(ctx, "RecordIdentity") }
func (n nativeRT) AllowCreate() server.Runtime              { n.mayCreate = true; return n }

func (r *switchRT) call(ctx context.Context, name string) error {
	r.w.mu.Lock()
	c := r.kind + " " + name
	if ctx.Err() != nil {
		c += " (cancelled)"
	}
	r.w.calls = append(r.w.calls, c)
	r.w.mu.Unlock()
	if h := r.hook[name]; h != nil {
		if err := h(ctx); err != nil {
			return err
		}
	}
	return r.fail[name]
}

func (r *switchRT) read(name string) {
	r.w.mu.Lock()
	r.w.reads = append(r.w.reads, r.kind+" "+name)
	r.w.mu.Unlock()
}

func (r *switchRT) Kind() string { return r.kind }
func (r *switchRT) Version() string {
	if r.kind == "native" {
		return "1.1.0-0123456789ab"
	}
	return "ghcr.io/vectorize-io/hindsight:1.1.0"
}
func (r *switchRT) HindsightVersion() string  { return "1.1.0" }
func (r *switchRT) Deployed() (string, error) { r.read("Deployed"); return r.deployed, nil }
func (r *switchRT) Render(sec config.Secrets, _ string) (server.Files, error) {
	r.apiKeys = append(r.apiKeys, sec.APIKey)
	return server.Files{}, r.call(context.Background(), "Render")
}
func (r *switchRT) Write(server.Files) (server.Changes, error) {
	err := r.call(context.Background(), "Write")
	if err == nil {
		r.deployed = r.Version()
	}
	return server.Changes{}, err
}
func (r *switchRT) Fetch(ctx context.Context) error    { return r.call(ctx, "Fetch") }
func (r *switchRT) FetchAll(ctx context.Context) error { return r.call(ctx, "FetchAll") }
func (r *switchRT) Downloads(context.Context) ([]string, bool, error) {
	r.read("Downloads")
	return r.missing, false, nil
}
func (r *switchRT) Download(ctx context.Context, _ []string, _ io.Writer) error {
	return r.call(ctx, "Download")
}
func (r *switchRT) Engine(context.Context) (server.Engine, error) {
	r.read("Engine")
	return server.Engine{Version: "28.1.1"}, nil
}
func (r *switchRT) Orchestrator(context.Context) (string, error) {
	r.read("Orchestrator")
	return "2.30.0", nil
}
func (r *switchRT) Start(ctx context.Context) error {
	err := r.call(ctx, "Start")
	if err == nil {
		r.w.running = r.kind
	}
	return err
}
func (r *switchRT) StartDatabase(ctx context.Context) error { return r.call(ctx, "StartDatabase") }
func (r *switchRT) StopAPI(ctx context.Context) error       { return r.call(ctx, "StopAPI") }
func (r *switchRT) Stop(ctx context.Context) error          { return r.call(ctx, "Stop") }
func (r *switchRT) StopCmd() string                         { return "stop " + r.kind }
func (r *switchRT) Delete(ctx context.Context) error        { return r.call(ctx, "Delete") }
func (r *switchRT) Status(context.Context) ([]server.Container, error) {
	return nil, nil
}
func (r *switchRT) Logs(context.Context, string, int) (string, error) { return "", nil }
func (r *switchRT) DataExists(context.Context) (bool, error) {
	r.read("DataExists")
	return r.exists, nil
}
func (r *switchRT) DataName() string                        { return "the " + r.kind + " data" }
func (r *switchRT) Database() server.Database               { return r.db }
func (r *switchRT) DataSize(context.Context) (int64, error) { r.read("DataSize"); return r.size, nil }
func (r *switchRT) Leftover(context.Context) (bool, error)  { return r.leftover || r.exists, nil }
func (r *switchRT) CleanWhat() string                       { return "everything of " + r.kind }
func (r *switchRT) CleanSize(context.Context) (int64, error) {
	r.read("CleanSize")
	return r.cleanSize, r.sizeErr
}
func (r *switchRT) VerifyDump(ctx context.Context, dump string, tables []string) (map[string]int64, error) {
	if err := r.call(ctx, "VerifyDump"); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		return nil, err
	}
	v := &switchDB{}
	if err := json.Unmarshal(raw, &v.data); err != nil {
		return nil, err
	}
	return v.counts(tables), nil
}

// switchSetup is a local install on runtime from (its data sampleData(),
// its API answering), with the other runtime fresh: no data there.
func switchSetup(t *testing.T, from string) (home string, w *switchWorld) {
	t.Helper()
	home = devInstall(t, true)
	w = &switchWorld{rts: map[string]*switchRT{}, running: from, memories: 25, rank: map[string]func(string) []string{}}
	for _, k := range []string{"docker", "native"} {
		rt := &switchRT{w: w, kind: k, fail: map[string]error{}, hook: map[string]func(context.Context) error{}}
		rt.db = &switchDB{rt: rt, data: dbData{Keys: map[string][]string{}}}
		w.rts[k] = rt
		w.rank[k] = sourceRanking
	}
	src := w.rts[from]
	src.db.data, src.exists, src.deployed = sampleData(), true, src.Version()
	old := runtimeWith
	runtimeWith = func(cfg config.Config, _ bool) server.Runtime {
		rt := w.rts[cfg.RuntimeKind()]
		if rt.kind == "native" {
			return nativeRT{rt}
		}
		return rt
	}
	oldPoll, oldProbes := healthPoll, switchProbes
	healthPoll = 10 * time.Millisecond
	switchProbes.userManager = func(context.Context) error { return nil }
	switchProbes.linger = func() (string, error) { return "yes", nil }
	switchProbes.freeBytes = func(string) (uint64, error) { return 50e9, nil }
	switchProbes.goos, switchProbes.goarch = "linux", "amd64"
	t.Cleanup(func() { runtimeWith, healthPoll, switchProbes = old, oldPoll, oldProbes })

	port := proxyHindsight(t, fakeHindsight(t), func(rw http.ResponseWriter, r *http.Request) bool {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if r.Method != http.MethodGet && (strings.Contains(r.URL.Path, "backsight:epoch") || strings.Contains(string(body), "backsight:epoch")) {
			w.mu.Lock()
			w.epochWrites++
			w.mu.Unlock()
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/memories/list"):
			w.mu.Lock()
			w.listTags = append(w.listTags, r.URL.Query().Get("tags"))
			w.mu.Unlock()
			var items []string
			if _, tagged := r.URL.Query()["tags"]; !tagged { // a tag filter matches none of them
				for i := range w.memories {
					items = append(items, fmt.Sprintf(`{"id":"m%02d","text":"memory text %02d","fact_type":"world","updated_at":"2026-10-24T09:00:00Z"}`, i, i))
				}
			}
			_, _ = rw.Write([]byte(`{"items":[` + strings.Join(items, ",") + `]}`))
			return true
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories/recall"):
			var req struct{ Query string }
			_ = json.Unmarshal(body, &req)
			var hits []string
			for _, id := range w.rank[w.running](req.Query) {
				hits = append(hits, `{"id":"`+id+`","text":"x","type":"world"}`)
			}
			_, _ = rw.Write([]byte(`{"results":[` + strings.Join(hits, ",") + `]}`))
			return true
		}
		return false
	})
	writeRuntimeConfig(t, home, from, port)
	return home, w
}

// sourceRanking: memory text NN finds mNN first, then the nine after it.
func sourceRanking(query string) []string {
	n, _ := strconv.Atoi(strings.TrimPrefix(query, "memory text "))
	var out []string
	for i := range 10 {
		out = append(out, fmt.Sprintf("m%02d", (n+i)%25))
	}
	return out
}

func writeRuntimeConfig(t *testing.T, home, runtime string, port int) {
	t.Helper()
	line := ""
	if runtime == "native" {
		line = "runtime = \"native\"\n"
	}
	raw := "[server]\n" + line + "url = \"http://127.0.0.1:" + strconv.Itoa(port) + "\"\nui_port = 19999\n"
	if err := os.WriteFile(pathsOf(home).ConfigFile(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

type switchEnvelope struct {
	OK   bool `json:"ok"`
	Data struct {
		From, To   string
		Plan       bool     `json:"plan"`
		Steps      []step   `json:"steps"`
		Backup     string   `json:"backup"`
		Gate       []string `json:"gate_differences"`
		SourceKept string   `json:"source_kept"`
		Warnings   []string `json:"warnings"`
		Probes     *struct {
			Queries    int     `json:"queries"`
			Overlap    float64 `json:"overlap_at_10"`
			OwnAtRank1 int     `json:"own_at_rank_1"`
		} `json:"probes"`
	} `json:"data"`
	Error *output.Error `json:"error"`
}

func runSwitchCmd(t *testing.T, ctx context.Context, args ...string) (int, switchEnvelope, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := ExecuteContext(ctx, append([]string{"server", "runtime"}, args...), &out, &errb)
	var env switchEnvelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("exit %d, not JSON: %v\n%s\n%s", code, err, out.String(), errb.String())
	}
	return code, env, out.String() + errb.String()
}

func switchState(t *testing.T, home string) *state.RuntimeSwitch {
	t.Helper()
	st, err := state.Load(pathsOf(home).StateFile())
	if err != nil {
		t.Fatal(err)
	}
	return st.RuntimeSwitch
}

var consent = []string{"--answer", "consent=yes", "--non-interactive", "--json"}

func TestSwitchDockerToNative(t *testing.T) {
	home, w := switchSetup(t, "docker")
	d := pathsOf(home)
	before, _ := os.ReadFile(d.ConfigFile())
	code, env, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, all)
	}
	want := []string{"native Fetch", "docker StopAPI", "docker Dump", "native VerifyDump", "docker Stop", "native Render", "native Write",
		"native StartDatabase", "native Restore", "native RecordIdentity", "native Start"}
	if !slices.Equal(w.calls, want) {
		t.Errorf("calls\n%q\nwant\n%q", w.calls, want)
	}
	if !w.rts["native"].mayCreate {
		t.Error("the native target may not create its first database (ruling R35)")
	}
	// The data went over whole.
	if !slices.Equal(w.rts["native"].db.data.Keys["documents"], sampleData().Keys["documents"]) {
		t.Errorf("native holds %v", w.rts["native"].db.data)
	}
	// config.toml says native, and nothing else changed.
	after, _ := os.ReadFile(d.ConfigFile())
	if !strings.Contains(string(after), `runtime = "native"`) || strings.Replace(string(after), "runtime = \"native\"\n", "", 1) != string(before) {
		t.Errorf("config.toml\n%s\nbefore\n%s", after, before)
	}
	if rs := switchState(t, home); rs == nil || rs.Step != "done" || rs.From != "docker" || rs.To != "native" || rs.Overlap != 1 || rs.Backup != env.Data.Backup {
		t.Errorf("runtime_switch %+v", rs)
	}
	// No restore epoch, no temporary key: the target runs on the server's
	// own key (decision 0078).
	sec, _ := config.LoadSecrets(d.SecretsFile())
	if w.epochWrites != 0 {
		t.Errorf("%d epoch writes", w.epochWrites)
	}
	for _, k := range w.rts["native"].apiKeys {
		if k != sec.APIKey {
			t.Errorf("the target rendered with another API key %q", k)
		}
	}
	if p := env.Data.Probes; p == nil || p.Queries != 20 || p.Overlap != 1 || p.OwnAtRank1 != 20 {
		t.Errorf("probes %+v", p)
	}
	if !slices.Contains(w.listTags, "") || len(env.Data.Warnings) != 0 {
		t.Errorf("listing tags %q, warnings %q", w.listTags, env.Data.Warnings)
	}
	if _, err := os.Stat(env.Data.Backup); err != nil || len(env.Data.Gate) != 0 {
		t.Errorf("backup %q: %v; gate %q", env.Data.Backup, err, env.Data.Gate)
	}
	if !strings.Contains(env.Data.SourceKept, "the docker data") { // DataName: the database the switch left behind
		t.Errorf("source kept %q", env.Data.SourceKept)
	}
	raw, err := os.ReadFile(filepath.Join(d.State, "logs", "runtime-switch.jsonl"))
	if err != nil || bytes.Count(raw, []byte("\n")) != 1 || !strings.Contains(string(raw), `"to":"native"`) {
		t.Errorf("runtime-switch.jsonl: %v\n%s", err, raw)
	}
	// stats shows the runtime and the last switch.
	code, out, _ := run(t, "stats", "--json")
	var stats struct {
		Data struct {
			Runtime       string `json:"runtime"`
			RuntimeSwitch struct {
				From, To, Step string
				At             time.Time
				Overlap        float64
			} `json:"runtime_switch"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &stats)
	if sw := stats.Data.RuntimeSwitch; code != output.OK || stats.Data.Runtime != "native" || sw.From != "docker" || sw.To != "native" || sw.Step != "done" || sw.Overlap != 1 || sw.At.IsZero() {
		t.Errorf("stats: exit %d\n%s", code, out)
	}
	if code, out, _ := run(t, "stats"); code != output.OK || !strings.Contains(out, "Runtime      native (switched from docker on ") {
		t.Errorf("stats text: exit %d\n%s", code, out)
	}
}

// Switching back over the old data restores over it: the current memory
// goes over, the old one never comes back.
func TestSwitchNativeToDockerRestoresOverOldData(t *testing.T) {
	_, w := switchSetup(t, "native")
	dk := w.rts["docker"]
	dk.exists = true
	dk.db.data = sampleData()
	dk.db.data.Keys["documents"] = []string{"(old1)", "(old2)"} // what it held before the first switch
	code, _, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "docker"}, consent...)...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, all)
	}
	if !slices.Contains(w.calls, "docker Restore") || slices.Contains(w.calls, "docker RecordIdentity") {
		t.Errorf("calls %q", w.calls)
	}
	if got := dk.db.data.Keys["documents"]; !slices.Equal(got, sampleData().Keys["documents"]) {
		t.Errorf("docker holds %v: the old data came back", got)
	}
	if dk.mayCreate {
		t.Error("a Docker target was asked to create")
	}
}

// failedSwitch checks a switch that failed after the source's API stopped:
// exit 20 naming the step, the target stopped then the source started,
// config.toml as it was, the backup kept, runtime_switch rolled back.
func failedSwitch(t *testing.T, home string, w *switchWorld, before []byte, code int, env switchEnvelope, step string) {
	t.Helper()
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "failed at "+step) ||
		!strings.Contains(env.Error.Message, "docker runs again as before") {
		t.Fatalf("exit %d: %+v", code, env.Error)
	}
	if n := len(w.calls); n < 2 || w.calls[n-2] != "native Stop" || w.calls[n-1] != "docker Start" {
		t.Errorf("the rollback: calls %q", w.calls)
	}
	if after, _ := os.ReadFile(pathsOf(home).ConfigFile()); !bytes.Equal(after, before) {
		t.Errorf("config.toml changed:\n%s", after)
	}
	if env.Data.Backup == "" || !strings.Contains(env.Error.Message, env.Data.Backup) {
		t.Errorf("backup %q not named: %s", env.Data.Backup, env.Error.Message)
	}
	if _, err := os.Stat(env.Data.Backup); err != nil {
		t.Errorf("the backup is gone: %v", err)
	}
	if rs := switchState(t, home); rs == nil || rs.Step != "rolled-back" {
		t.Errorf("runtime_switch %+v", rs)
	}
}

func TestSwitchGateFailureRollsBack(t *testing.T) {
	home, w := switchSetup(t, "docker")
	before, _ := os.ReadFile(pathsOf(home).ConfigFile())
	w.rts["native"].db.afterRestore = func(d *dbData) { d.Keys["documents"][1] = "(d9)" } // same counts, one key other
	code, env, _ := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...)
	failedSwitch(t, home, w, before, code, env, "gate")
	if !slices.Equal(env.Data.Gate, []string{"documents: keys differ"}) {
		t.Errorf("gate %q", env.Data.Gate)
	}
	if slices.Contains(w.calls, "native Start") {
		t.Errorf("the target's API started on data that differs: %q", w.calls)
	}
}

// RF 3: a Ctrl-C mid-switch puts the source back as it was.
func TestSwitchInterruptedPutsTheSourceBack(t *testing.T) {
	home, w := switchSetup(t, "docker")
	before, _ := os.ReadFile(pathsOf(home).ConfigFile())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.rts["native"].hook["Restore"] = func(ctx context.Context) error { cancel(); return context.Canceled }
	code, env, _ := runSwitchCmd(t, ctx, append([]string{"switch", "native"}, consent...)...)
	failedSwitch(t, home, w, before, code, env, "restore")
	// The rollback ran to its end: not on the cancelled context.
	if n := len(w.calls); strings.Contains(w.calls[n-1], "cancelled") || strings.Contains(w.calls[n-2], "cancelled") {
		t.Errorf("the rollback ran on the cancelled context: %q", w.calls)
	}
}

// RF 3: a switch killed part way leaves runtime_switch running with a
// dead owner; its rerun first puts the source back, then switches.
func TestSwitchRerunAfterAKillStartsFromTheSource(t *testing.T) {
	home, w := switchSetup(t, "docker")
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip(err)
	}
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.RuntimeSwitch = &state.RuntimeSwitch{From: "docker", To: "native", Step: "running", PID: cmd.Process.Pid, Since: time.Now().Add(-time.Hour), At: time.Now().Add(-time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.running, w.rts["native"].deployed = "native", w.rts["native"].Version() // killed after the target started
	code, _, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, all)
	}
	want := []string{"native Fetch", "native Stop", "docker Start", "docker StopAPI", "docker Dump", "native VerifyDump", "docker Stop",
		"native Render", "native Write", "native StartDatabase", "native Restore", "native RecordIdentity", "native Start"}
	if !slices.Equal(w.calls, want) {
		t.Errorf("calls\n%q\nwant\n%q", w.calls, want)
	}
	if rs := switchState(t, home); rs == nil || rs.Step != "done" || rs.PID != os.Getpid() {
		t.Errorf("runtime_switch %+v", rs)
	}
}

// lastLifecycle is the last start or stop of kind's runtime: what decides
// whether it starts at boot (with linger, native's units enabled by Start,
// disabled by Stop; Docker's containers up, or stopped and kept down).
func lastLifecycle(w *switchWorld, kind string) string {
	last := ""
	for _, c := range w.calls {
		k, m, _ := strings.Cut(c, " ")
		if k == kind && (m == "Start" || m == "Stop" || m == "StartDatabase" || m == "StopAPI") {
			last = m
		}
	}
	return last
}

// RF 2: whatever happens, exactly one runtime is left to start at boot.
func TestSwitchLeavesOneRuntimeToStartAtBoot(t *testing.T) {
	t.Run("docker to native, passed", func(t *testing.T) {
		_, w := switchSetup(t, "docker")
		if code, _, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...); code != output.OK {
			t.Fatalf("exit %d\n%s", code, all)
		}
		if n, dk := lastLifecycle(w, "native"), lastLifecycle(w, "docker"); n != "Start" || dk != "Stop" {
			t.Errorf("native %s (enable --now), docker %s (compose stop): %q", n, dk, w.calls)
		}
	})
	t.Run("docker to native, failed", func(t *testing.T) {
		_, w := switchSetup(t, "docker")
		w.rts["native"].fail["Start"] = fmt.Errorf("fsync is off")
		if code, _, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...); code != output.Blocked {
			t.Fatalf("exit %d\n%s", code, all)
		}
		if n, dk := lastLifecycle(w, "native"), lastLifecycle(w, "docker"); n != "Stop" || dk != "Start" {
			t.Errorf("native %s (disable --now), docker %s (up): %q", n, dk, w.calls)
		}
	})
	t.Run("native to docker, passed", func(t *testing.T) {
		_, w := switchSetup(t, "native")
		if code, _, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "docker"}, consent...)...); code != output.OK {
			t.Fatalf("exit %d\n%s", code, all)
		}
		if n, dk := lastLifecycle(w, "native"), lastLifecycle(w, "docker"); n != "Stop" || dk != "Start" {
			t.Errorf("native %s (disable --now), docker %s (up): %q", n, dk, w.calls)
		}
	})
}

// Recall is reported, not gated: another reranker may order near-ties
// otherwise on identical data.
func TestSwitchRecallWarningIsNotAFailure(t *testing.T) {
	home, w := switchSetup(t, "docker")
	w.rank["native"] = func(q string) []string {
		top := sourceRanking(q)
		return append(top[:8], "x1", "x2") // 8 of 10 in common
	}
	code, env, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, all)
	}
	if p := env.Data.Probes; p == nil || p.Overlap < 0.799 || p.Overlap > 0.801 {
		t.Errorf("probes %+v", p)
	}
	if w := strings.Join(env.Data.Warnings, "\n"); !strings.Contains(w, "recall differs after the switch (overlap@10 0.80)") || !strings.Contains(w, "the gate passed") {
		t.Errorf("warnings %q", env.Data.Warnings)
	}
	if rs := switchState(t, home); rs == nil || rs.Step != "done" {
		t.Errorf("runtime_switch %+v", rs)
	}
}

// R14: no probe on a bank that holds memories is said, never silent.
func TestSwitchWithoutProbesWarns(t *testing.T) {
	_, w := switchSetup(t, "docker")
	w.memories = 0
	code, env, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, all)
	}
	if w := strings.Join(env.Data.Warnings, "\n"); !strings.Contains(w, "no recent memory to probe recall with") || !strings.Contains(w, "4 memory units") {
		t.Errorf("warnings %q", env.Data.Warnings)
	}
}

func TestSwitchRefusals(t *testing.T) {
	for _, c := range []struct {
		name  string
		from  string
		args  []string
		setup func(t *testing.T, home string, w *switchWorld)
		code  int
		says  string
	}{
		{name: "plan", from: "docker", args: []string{"switch", "native", "--plan", "--json"}, code: output.OK},
		{name: "plan to docker", from: "native", args: []string{"switch", "docker", "--plan", "--json"}, code: output.OK},
		{name: "linger off", from: "docker", args: append([]string{"switch", "native"}, consent...), code: output.NeedsUser, says: "loginctl enable-linger",
			setup: func(*testing.T, string, *switchWorld) {
				switchProbes.linger = func() (string, error) { return "no", nil }
			}},
		{name: "no consent", from: "docker", args: []string{"switch", "native", "--non-interactive", "--json"}, code: output.NeedsUser,
			says: "--answer consent=yes"},
		{name: "same runtime", from: "docker", args: append([]string{"switch", "docker"}, consent...), code: output.OK},
		{name: "remote", from: "docker", args: append([]string{"switch", "native"}, consent...), code: output.Usage, says: "client of the server",
			setup: func(t *testing.T, home string, _ *switchWorld) {
				cfg := pathsOf(home).ConfigFile()
				raw, _ := os.ReadFile(cfg)
				_ = os.WriteFile(cfg, bytes.Replace(raw, []byte("[server]\n"), []byte("[server]\nmode = \"remote\"\n"), 1), 0o600)
			}},
		{name: "server-only host", from: "docker", args: append([]string{"switch", "native"}, consent...), code: output.Usage, says: config.NativeServerOnly,
			setup: func(t *testing.T, home string, _ *switchWorld) {
				cfg := pathsOf(home).ConfigFile()
				raw, _ := os.ReadFile(cfg)
				_ = os.WriteFile(cfg, bytes.Replace(raw, []byte("[server]\n"), []byte("[server]\nrole = \"server\"\n"), 1), 0o600)
			}},
		{name: "another platform", from: "docker", args: append([]string{"switch", "native"}, consent...), code: output.Blocked, says: config.NativeRefusal,
			setup: func(*testing.T, string, *switchWorld) { switchProbes.goos = "darwin" }},
		{name: "unknown runtime", from: "docker", args: append([]string{"switch", "podman"}, consent...), code: output.Usage, says: "docker or native"},
		{name: "a live switch", from: "docker", args: append([]string{"switch", "native"}, consent...), code: output.Blocked, says: "already running",
			setup: func(t *testing.T, home string, _ *switchWorld) {
				_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
					s.RuntimeSwitch = &state.RuntimeSwitch{From: "docker", To: "native", Step: "running", PID: liveProcess(t), Since: time.Now()}
					return nil
				})
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, w := switchSetup(t, c.from)
			if c.setup != nil {
				c.setup(t, home, w)
			}
			tree := treeOf(t, filepath.Join(home, "backsight"))
			code, env, all := runSwitchCmd(t, context.Background(), c.args...)
			if code != c.code {
				t.Fatalf("exit %d, want %d\n%s", code, c.code, all)
			}
			if c.says != "" && !strings.Contains(all, c.says) {
				t.Errorf("no %q in\n%s", c.says, all)
			}
			if len(w.calls) != 0 {
				t.Errorf("changed something: %q", w.calls)
			}
			if after := treeOf(t, filepath.Join(home, "backsight")); !maps.Equal(after, tree) {
				t.Errorf("files changed")
			}
			if c.name == "plan" {
				if !env.Data.Plan || len(env.Data.Steps) < 10 {
					t.Errorf("plan %+v", env.Data)
				}
				for _, s := range env.Data.Steps {
					if s.Status != "planned" {
						t.Errorf("step %+v", s)
					}
				}
				if !slices.Contains(w.reads, "native Downloads") {
					t.Errorf("the plan did not look at the downloads: %q", w.reads)
				}
			}
			if c.name == "same runtime" && !strings.Contains(all, "already on docker") {
				t.Errorf("%s", all)
			}
		})
	}
}

type cleanEnvelope struct {
	Data struct {
		Runtime     string `json:"runtime"`
		What        string `json:"what"`
		Bytes       int64  `json:"bytes"`
		SizeUnknown bool   `json:"size_unknown"`
		Plan        bool   `json:"plan"`
		Declined    bool   `json:"declined"`
		Deleted     bool   `json:"deleted"`
	} `json:"data"`
	Error *output.Error `json:"error"`
}

func cleanCmd(t *testing.T, args ...string) (int, cleanEnvelope, string) {
	t.Helper()
	code, out, errOut := run(t, append([]string{"server", "runtime", "clean"}, args...)...)
	var env cleanEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("exit %d, not JSON: %v\n%s%s", code, err, out, errOut)
	}
	return code, env, out + errOut
}

func TestRuntimeCleanDeletesOnlyTheOtherRuntime(t *testing.T) {
	home, w := switchSetup(t, "native") // on native after a switch
	dk := w.rts["docker"]
	dk.exists, dk.size, dk.cleanSize, dk.deployed = true, 1000, 123456789, dk.Version()

	// It shows everything Delete removes and what all of it weighs
	// (ruling R43), not the database alone.
	code, env, all := cleanCmd(t, "--plan", "--json")
	if code != output.OK || !env.Data.Plan || env.Data.Runtime != "docker" || env.Data.What != "everything of docker" || env.Data.Bytes != 123456789 || env.Data.Deleted {
		t.Fatalf("plan: exit %d %+v\n%s", code, env.Data, all)
	}
	if code, out, _ := run(t, "server", "runtime", "clean", "--plan"); code != output.OK || !strings.Contains(out, "would delete everything of docker (117.7 MB)") {
		t.Errorf("plan text: exit %d\n%s", code, out)
	}
	dk.sizeErr = fmt.Errorf("docker system df: timeout")
	if code, out, _ := run(t, "server", "runtime", "clean", "--plan"); code != output.OK || !strings.Contains(out, "(size unknown)") || strings.Contains(out, "0 bytes") {
		t.Errorf("unknown size: exit %d\n%s", code, out)
	}
	if _, env, _ := cleanCmd(t, "--plan", "--json"); !env.Data.SizeUnknown {
		t.Errorf("unknown size: %+v", env.Data)
	}
	dk.sizeErr = nil
	if code, _, all := cleanCmd(t, "--non-interactive", "--json"); code != output.NeedsUser || !strings.Contains(all, "consent=yes") {
		t.Fatalf("no consent: exit %d\n%s", code, all)
	}
	if len(w.calls) != 0 {
		t.Fatalf("deleted before asking: %q", w.calls)
	}
	code, env, all = cleanCmd(t, append([]string{}, consent...)...)
	if code != output.OK || !env.Data.Deleted {
		t.Fatalf("exit %d %+v\n%s", code, env.Data, all)
	}
	if !slices.Equal(w.calls, []string{"docker Delete"}) {
		t.Errorf("calls %q", w.calls)
	}

	// Nothing there: nothing to clean.
	w.calls = nil
	dk.exists, dk.deployed = false, ""
	if code, out, _ := run(t, "server", "runtime", "clean", "--answer", "consent=yes", "--non-interactive"); code != output.OK || !strings.Contains(out, "nothing to clean") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if len(w.calls) != 0 {
		t.Errorf("calls %q", w.calls)
	}
	// Leftovers without data (a failed switch's downloads) are something
	// to clean.
	dk.leftover = true
	if code, out, _ := run(t, "server", "runtime", "clean", "--plan"); code != output.OK || !strings.Contains(out, "would delete everything of docker") {
		t.Errorf("leftovers: exit %d\n%s", code, out)
	}

	// Never while a switch runs or was cut off.
	dk.exists = true
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.RuntimeSwitch = &state.RuntimeSwitch{From: "native", To: "docker", Step: "running", PID: 1, Since: time.Now()}
		return nil
	})
	if code, _, all := cleanCmd(t, consent...); code != output.Blocked || len(w.calls) != 0 {
		t.Errorf("a switch running: exit %d, calls %q\n%s", code, w.calls, all)
	}
}

// Task 12's comparison: the keys of a file of keys missing now are named.
func TestFingerprintKeysCompare(t *testing.T) {
	home, w := switchSetup(t, "docker")
	keys := filepath.Join(home, "keys-0")
	code, out, errOut := run(t, "server", "runtime", "fingerprint", "--keys", keys, "--json")
	var fp struct {
		Data server.Fingerprint `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &fp); code != output.OK || err != nil || fp.Data.Tables["documents"].Rows != 3 || len(fp.Data.Control) != 2 {
		t.Fatalf("exit %d %v\n%s%s", code, err, out, errOut)
	}
	if fi, err := os.Stat(keys); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("keys file: %v %v", fi, err)
	}
	if code, out, errOut := run(t, "server", "runtime", "fingerprint", "--compare", keys); code != output.OK {
		t.Fatalf("nothing gone: exit %d\n%s%s", code, out, errOut)
	}
	w.rts["docker"].db.data.Keys["documents"] = []string{"(d1)", "(d3)", "(d4)"}
	code, out, errOut = run(t, "server", "runtime", "fingerprint", "--compare", keys)
	if code != output.Blocked || !strings.Contains(out+errOut, "documents (d2)") || strings.Contains(out+errOut, "(d4)") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

// A failure before the target's files exist puts the source back without
// touching the target: a Docker target without compose.yaml has nothing
// to stop, and stopping it would fail the rollback.
func TestSwitchFailureBeforeTheTargetLeavesItAlone(t *testing.T) {
	home, w := switchSetup(t, "native")
	before, _ := os.ReadFile(pathsOf(home).ConfigFile())
	w.rts["docker"].fail["VerifyDump"] = fmt.Errorf("pg_restore: bad dump")
	code, env, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "docker"}, consent...)...)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "failed at verify") || !strings.Contains(env.Error.Message, "native runs again as before") {
		t.Fatalf("exit %d\n%s", code, all)
	}
	want := []string{"native StopAPI", "native Dump", "docker VerifyDump", "native Start"}
	if !slices.Equal(w.calls, want) {
		t.Errorf("calls\n%q\nwant\n%q", w.calls, want)
	}
	if after, _ := os.ReadFile(pathsOf(home).ConfigFile()); !bytes.Equal(after, before) {
		t.Errorf("config.toml changed:\n%s", after)
	}
}

// RF 3: killed after config.toml named the target, the switch had passed:
// its rerun says so and stops or starts nothing.
func TestSwitchRerunAfterAKillPastTheCommit(t *testing.T) {
	home, w := switchSetup(t, "native")
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip(err)
	}
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.RuntimeSwitch = &state.RuntimeSwitch{From: "docker", To: "native", Step: "running", PID: cmd.Process.Pid, Since: time.Now().Add(-time.Hour)}
		return nil
	})
	code, _, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...)
	if code != output.OK || !strings.Contains(all, "already on native") {
		t.Fatalf("exit %d\n%s", code, all)
	}
	if len(w.calls) != 0 {
		t.Errorf("calls %q", w.calls)
	}
	if rs := switchState(t, home); rs == nil || rs.Step != "done" {
		t.Errorf("runtime_switch %+v", rs)
	}
}

// A rollback that cannot start the source again says how to get back, and
// leaves runtime_switch running for doctor.
func TestSwitchFailedRollbackKeepsTheRecord(t *testing.T) {
	home, w := switchSetup(t, "docker")
	w.rts["native"].db.afterRestore = func(d *dbData) { d.Keys["documents"] = d.Keys["documents"][:1] }
	w.rts["docker"].hook["Start"] = func(context.Context) error { return fmt.Errorf("port 8888 is taken") }
	code, env, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...)
	if code != output.Blocked || env.Error == nil {
		t.Fatalf("exit %d\n%s", code, all)
	}
	msg := env.Error.Message
	for _, want := range []string{"failed at gate", "putting docker back failed", "port 8888 is taken", "stop native", "backsight server start", env.Data.Backup} {
		if !strings.Contains(msg, want) {
			t.Errorf("no %q in %q", want, msg)
		}
	}
	if rs := switchState(t, home); rs == nil || rs.Step != "running" {
		t.Errorf("runtime_switch %+v", rs)
	}
	if after, _ := os.ReadFile(pathsOf(home).ConfigFile()); strings.Contains(string(after), "native") {
		t.Errorf("config.toml changed:\n%s", after)
	}
}

// A switch carries the memory, never a Hindsight upgrade past server
// upgrade: another release on the source is refused before anything.
func TestSwitchRefusesAnotherHindsightRelease(t *testing.T) {
	home, w := switchSetup(t, "docker")
	w.rts["docker"].deployed = "ghcr.io/vectorize-io/hindsight:1.0.9"
	tree := treeOf(t, filepath.Join(home, "backsight"))
	code, _, all := runSwitchCmd(t, context.Background(), append([]string{"switch", "native"}, consent...)...)
	if code != output.Blocked || !strings.Contains(all, "Hindsight 1.0.9") || !strings.Contains(all, "backsight server upgrade") {
		t.Fatalf("exit %d\n%s", code, all)
	}
	if len(w.calls) != 0 || !maps.Equal(treeOf(t, filepath.Join(home, "backsight")), tree) {
		t.Errorf("changed something: %q", w.calls)
	}
	// The same release with another lock or digest is the same release.
	w.rts["docker"].deployed = "ghcr.io/vectorize-io/hindsight:1.1.0@sha256:" + strings.Repeat("a", 64)
	if code, _, all := runSwitchCmd(t, context.Background(), "switch", "native", "--plan", "--json"); code != output.OK {
		t.Errorf("same release: exit %d\n%s", code, all)
	}
}
