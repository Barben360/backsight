package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server/nativelock"
)

// fakeSys records each systemctl, journalctl or systemd-run call; Out
// answers it (nil: no output, success).
type fakeSys struct {
	Calls [][]string
	Out   func(c []string) ([]byte, error)
}

func (f *fakeSys) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	c := append([]string{name}, args...)
	f.Calls = append(f.Calls, c)
	if f.Out != nil {
		return f.Out(c)
	}
	return nil, nil
}

func (f *fakeSys) lines() []string {
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

// withFreePort makes freePort answer ports in turn and records each call's
// from and skip list.
func withFreePort(t *testing.T, ports ...int) *[][]int {
	t.Helper()
	old := freePort
	var calls [][]int
	freePort = func(from int, skip ...int) (int, error) {
		calls = append(calls, append([]int{from}, skip...))
		if len(ports) == 0 {
			return 0, errors.New("no port left in the test")
		}
		p := ports[0]
		ports = ports[1:]
		return p, nil
	}
	t.Cleanup(func() { freePort = old })
	return &calls
}

// nativeFixture is a native runtime on temporary directories, with sec in
// its secrets.toml and fakes for the system and the tools.
func nativeFixture(t *testing.T, sec config.Secrets) (Native, *fakeSys, *fakeTools) {
	t.Helper()
	base := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(base, "data"), Config: filepath.Join(base, "config"), State: filepath.Join(base, "state")}
	cfg := config.Default(d, "/home/u")
	cfg.Server.Runtime = "native"
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { *s = sec; return nil }); err != nil {
		t.Fatal(err)
	}
	sys, tools := &fakeSys{}, &fakeTools{}
	n := Native{Cfg: cfg, Dirs: d, UnitDir: filepath.Join(base, "units"),
		Env: map[string]string{"BACKSIGHT_HOME": base, "XDG_STATE_HOME": filepath.Join(base, "xs")},
		Sys: sys, Tools: tools, Proc: "/proc"}
	withFreePort(t, 8889)
	withAPIHealth(t, func(context.Context, string) (string, error) { return "connected", nil })
	tools.Out = dbToolsOut("on")
	return n, sys, tools
}

// withAPIHealth makes apiHealth answer with check, recording each URL.
func withAPIHealth(t *testing.T, check func(ctx context.Context, url string) (string, error)) *[]string {
	t.Helper()
	old := apiHealth
	var urls []string
	apiHealth = func(ctx context.Context, url string) (string, error) {
		urls = append(urls, url)
		return check(ctx, url)
	}
	t.Cleanup(func() { apiHealth = old })
	return &urls
}

// installed renders and writes n's files, beside a database whose
// identity is recorded (an install past its first start).
func installed(t *testing.T, n Native, sec config.Secrets) Files {
	t.Helper()
	f := rendered(t, n, sec)
	recordInstance(t, n, "7702339718603615110")
	return f
}

// rendered renders and writes n's files, no database yet.
func rendered(t *testing.T, n Native, sec config.Secrets) Files {
	t.Helper()
	f, err := n.Render(sec, "box")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Write(f); err != nil {
		t.Fatal(err)
	}
	return f
}

// envKeys lists the KEY of each KEY=value line of an env file, in order.
func envFileKeys(raw []byte) []string {
	var keys []string
	for _, l := range strings.Split(string(raw), "\n") {
		if k, _, ok := strings.Cut(l, "="); ok && !strings.HasPrefix(l, "#") {
			keys = append(keys, k)
		}
	}
	return keys
}

func envValue(raw []byte, key string) string {
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return strings.Trim(v, "'")
		}
	}
	return ""
}

// unitEnvironmentKeys are the keys a unit's Environment= lines set.
func unitEnvironmentKeys(unit []byte) []string {
	var keys []string
	for _, l := range strings.Split(string(unit), "\n") {
		if v, ok := strings.CutPrefix(l, "Environment="); ok {
			k, _, _ := strings.Cut(strings.Trim(v, `"`), "=")
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func unitLine(unit []byte, key string) string {
	for _, l := range strings.Split(string(unit), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v
		}
	}
	return ""
}

// The golden files pin what a native install runs, and the assertions
// name each E29 requirement they carry.
func TestNativeRenderMeetsTheRequirements(t *testing.T) {
	withFreePort(t, 8889)
	cfg := config.Default(paths.Dirs{Data: "/srv/bs/data"}, "/home/u")
	cfg.Server.Runtime = "native"
	n := Native{Cfg: cfg, Dirs: paths.Dirs{Data: "/srv/bs/data"}, UnitDir: "/srv/units",
		Env: map[string]string{"BACKSIGHT_HOME": "/srv/bs"}}.WithVersion("1.1.0-0123456789ab").(Native)
	f, err := n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	if f.Compose != nil {
		t.Error("a native render has no compose.yaml")
	}
	golden(t, "native-golden/server.env", f.Env)
	golden(t, "native-golden/backsight-db.service", f.Units["backsight-db.service"])
	golden(t, "native-golden/backsight-server.service", f.Units["backsight-server.service"])
	if len(f.Units) != 2 {
		t.Errorf("units %v, want the db and the server", slices.Sorted(func(yield func(string) bool) {
			for k := range f.Units {
				if !yield(k) {
					return
				}
			}
		}))
	}

	root := "/srv/bs/data/server/native"
	env := f.Env
	for k, want := range map[string]string{
		"HINDSIGHT_API_HOST":                              "127.0.0.1",
		"HINDSIGHT_API_PORT":                              "8888",
		"HINDSIGHT_API_DATABASE_URL":                      "postgresql://hindsight:fakedbpassword@127.0.0.1:8889/hindsight", // req 6
		"HINDSIGHT_API_RERANKER_PROVIDER":                 "onnx",                                                           // req 8
		"HINDSIGHT_API_RERANKER_ONNX_MODEL_ID":            "cross-encoder/mmarco-mMiniLMv2-L12-H384-v1",
		"HINDSIGHT_API_RERANKER_MAX_CANDIDATES":           "30",
		"HINDSIGHT_API_RERANKER_MAX_TOKENS_PER_CANDIDATE": "128",
		"HOME":              root + "/home", // req 6
		"HF_HOME":           root + "/hf",
		"BACKSIGHT_PG_PORT": "8889",
	} {
		if got := envValue(env, k); got != want {
			t.Errorf("server.env %s = %q, want %q", k, got, want)
		}
	}
	// Ruling R7: each key once, Docker's local reranker gone.
	keys := envFileKeys(env)
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Errorf("server.env sets %s twice", k)
		}
		seen[k] = true
		if strings.HasPrefix(k, "HINDSIGHT_CP_") || k == "BACKSIGHT_DB_PASSWORD" || strings.Contains(k, "RERANKER_LOCAL") {
			t.Errorf("server.env sets Docker's %s", k)
		}
	}

	db, srv := f.Units["backsight-db.service"], f.Units["backsight-server.service"]
	for name, u := range map[string][]byte{"db": db, "server": srv} {
		if !strings.HasPrefix(string(u), "# Generated by backsight: do not edit.\n") {
			t.Errorf("%s unit does not start with the generated header", name)
		}
		if unitLine(u, "Restart") != "on-failure" || unitLine(u, "WantedBy") != "default.target" {
			t.Errorf("%s unit: Restart=%q WantedBy=%q", name, unitLine(u, "Restart"), unitLine(u, "WantedBy"))
		}
	}
	// Req 3: everything Hindsight starts dies with its unit; the db unit
	// stops postgres with SIGINT, then the rest of its cgroup (ruling R20).
	if unitLine(srv, "KillMode") != "control-group" {
		t.Errorf("server unit KillMode=%q", unitLine(srv, "KillMode"))
	}
	if unitLine(db, "KillMode") != "mixed" || unitLine(db, "KillSignal") != "SIGINT" || unitLine(db, "Type") != "simple" {
		t.Errorf("db unit KillMode=%q KillSignal=%q Type=%q", unitLine(db, "KillMode"), unitLine(db, "KillSignal"), unitLine(db, "Type"))
	}
	if got := unitLine(db, "TimeoutStartSec"); got != "300" {
		t.Errorf("db TimeoutStartSec=%q, want 300 (native-check, crash recovery and readiness)", got)
	}
	if unitLine(db, "ExecStop") != "" {
		t.Error("the db unit has an ExecStop: postgres is stopped by its signal")
	}
	if got := unitLine(db, "ExecStartPre"); got != "/srv/bs/data/bin/current server native-check" {
		t.Errorf("db ExecStartPre = %q, want the shim's native-check", got)
	}
	inst := root + "/home/.pg0/installation/18.1.0"
	start := unitLine(db, "ExecStart")
	for _, want := range []string{inst + "/bin/postgres -D " + root + "/home/.pg0/instances/hindsight/data -p 8889 ",
		"-c fsync=on", "-c listen_addresses=127.0.0.1", "-k " + root + "/home/.pg0/run", // req 1, loopback
		"-c shared_buffers=256MB", "-c work_mem=64MB", "-c maintenance_work_mem=512MB", "-c effective_cache_size=1GB",
		"-c max_parallel_maintenance_workers=4", "-c timezone=UTC", "-c log_timezone=UTC"} { // ruling R23
		if !strings.Contains(start, want) {
			t.Errorf("db ExecStart %q lacks %q", start, want)
		}
	}
	if got := unitLine(db, "ExecStartPost"); !strings.Contains(got, "kill -0 $$MAINPID") || !strings.Contains(got, inst+"/bin/pg_isready -q -h "+root+"/home/.pg0/run -p 8889") {
		t.Errorf("db ExecStartPost = %q, want the pg_isready loop on $$MAINPID", got)
	}
	if !strings.Contains(string(db), "\nEnvironment=LD_LIBRARY_PATH="+inst+"/lib\n") || !strings.Contains(string(db), "\nEnvironment=HOME="+root+"/home\n") {
		t.Error("db unit lacks HOME or LD_LIBRARY_PATH")
	}
	if !strings.Contains(string(db), "\nEnvironment=BACKSIGHT_HOME=/srv/bs\n") {
		t.Error("db unit lacks the directories native-check reads its config with")
	}
	if unitLine(srv, "Requires") != "backsight-db.service" || unitLine(srv, "After") != "backsight-db.service" {
		t.Errorf("server unit Requires=%q After=%q", unitLine(srv, "Requires"), unitLine(srv, "After"))
	}
	for k, want := range map[string]string{"EnvironmentFile": root + "/server.env", "WorkingDirectory": root,
		"ExecStart": root + "/venv/1.1.0-0123456789ab/bin/hindsight-api", "TimeoutStopSec": "60"} {
		if got := unitLine(srv, k); got != want {
			t.Errorf("server unit %s=%q, want %q", k, got, want)
		}
	}
	if !strings.Contains(string(db), "Description=backsight memory database (pg0 "+nativelock.PG0+")") {
		t.Error("db unit Description does not name pg0's version")
	}
}

// A path with a space, a quote, a % or a $ stays one word in a unit.
func TestNativeUnitsQuoteTheirPaths(t *testing.T) {
	withFreePort(t, 8889)
	cfg := config.Default(paths.Dirs{Data: "/srv/my data"}, "/home/u")
	cfg.Server.ComposeDir = `/srv/my data/100% "$x"`
	n := Native{Cfg: cfg, Dirs: paths.Dirs{Data: "/srv/my data"}, UnitDir: "/srv/units"}
	f, err := n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	srv := f.Units["backsight-server.service"]
	// systemd expands $ in an Exec line's arguments, not in its program.
	if got, want := unitLine(srv, "ExecStart"), `"/srv/my data/100%% \"$x\"/native/venv/`+n.Version()+`/bin/hindsight-api"`; got != want {
		t.Errorf("ExecStart=%s, want %s", got, want)
	}
	if got, want := unitLine(f.Units["backsight-db.service"], "ExecStart"), `"/srv/my data/100%% \"$x\"/native/home/.pg0/installation/18.1.0/bin/postgres" -D "/srv/my data/100%% \"$$x\"/native/home/.pg0/instances/hindsight/data" -p `; !strings.HasPrefix(got, want) {
		t.Errorf("db ExecStart=%s, want it to start %s", got, want)
	}
	if v, err := (Native{Cfg: cfg, UnitDir: t.TempDir()}).deployedFrom(srv); err != nil || v != n.Version() {
		t.Errorf("Deployed of a quoted path: %q %v", v, err)
	}
	if got, want := unitLine(f.Units["backsight-db.service"], "ExecStartPre"), `"/srv/my data/bin/current" server native-check`; got != want {
		t.Errorf("ExecStartPre=%s, want %s", got, want)
	}
	// A path setting takes the rest of the line as is: no quotes, only
	// specifiers escaped.
	if got, want := unitLine(srv, "EnvironmentFile"), `/srv/my data/100%% "$x"/native/server.env`; got != want {
		t.Errorf("EnvironmentFile=%s, want %s", got, want)
	}
	if got, want := unitLine(srv, "WorkingDirectory"), `/srv/my data/100%% "$x"/native`; got != want {
		t.Errorf("WorkingDirectory=%s, want %s", got, want)
	}
	cfg.Server.ComposeDir = "/srv/a\nb"
	if _, err := (Native{Cfg: cfg, UnitDir: "/srv/units"}).Render(secrets, "box"); err == nil {
		t.Error("a newline in a path must be refused: a unit file cannot carry it")
	}
}

// RF 1: no secret on any argv, in a unit, or in an error; server.env 0600
// and home/ 0700 hold them.
func TestNativeNeverPutsASecretOnArgvOrInAUnit(t *testing.T) {
	sec := config.Secrets{DBPassword: "dbpw-XYZ", APIKey: "api-XYZ", LLMAPIKey: "llm-XYZ", UIKey: "uikey-XYZ"}
	n, sys, tools := nativeFixture(t, sec)
	sys.Out = func(c []string) ([]byte, error) {
		if slices.Contains(c, "show") {
			return []byte("ActiveState=active\nSubState=running\n\nActiveState=active\nSubState=running\n"), nil
		}
		return nil, nil
	}
	f := installed(t, n, sec)
	ctx := context.Background()
	secretValues := []string{"dbpw-XYZ", "api-XYZ", "llm-XYZ"}
	if !strings.Contains(string(f.Env), "dbpw-XYZ") || !strings.Contains(string(f.Env), "api-XYZ") || !strings.Contains(string(f.Env), "llm-XYZ") {
		t.Fatal("server.env does not hold the secrets: the test proves nothing")
	}
	for name := range f.Units {
		raw, err := os.ReadFile(filepath.Join(n.UnitDir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secretValues {
			if strings.Contains(string(raw), s) {
				t.Errorf("unit %s holds %s", name, s)
			}
		}
	}
	if st, err := os.Stat(n.EnvFile()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("server.env: %v", err)
	}
	if st, err := os.Stat(n.Home()); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("home/: %v %v", st.Mode(), err)
	}

	for _, step := range []func() error{
		func() error { return n.Start(ctx) },
		func() error { return n.StopAPI(ctx) },
		func() error { return n.StartDatabase(ctx) },
		func() error { _, err := n.Status(ctx); return err },
		func() error { _, err := n.Logs(ctx, "hindsight", 50); return err },
		func() error { _, err := n.RenameBank(ctx, "a", "b", true); return err },
		func() error { return n.createInstance(ctx) },
		func() error { return n.Stop(ctx) },
		func() error { _, err := n.Engine(ctx); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range sys.Calls {
		for _, s := range secretValues {
			if strings.Contains(strings.Join(c, " "), s) {
				t.Errorf("%v has %s on its argv", c, s)
			}
		}
	}
	var psqlEnv, stdin bool
	for _, c := range tools.Calls {
		for _, s := range secretValues {
			if strings.Contains(c.Path+" "+strings.Join(c.Args, " "), s) {
				t.Errorf("%s %v has %s on its argv", c.Path, c.Args, s)
			}
		}
		if filepath.Base(c.Path) == "psql" && slices.Contains(c.Env, "PGPASSWORD=dbpw-XYZ") {
			psqlEnv = true
		}
		if strings.Contains(string(c.Stdin), "PASSWORD 'dbpw-XYZ'") {
			stdin = true
		}
	}
	if !psqlEnv || !stdin {
		t.Errorf("the password must reach psql through PGPASSWORD (%v) and the instance through stdin (%v)", psqlEnv, stdin)
	}

	// A failing command that echoes a secret gives an error without it.
	sys.Out = func(c []string) ([]byte, error) {
		return []byte("Job failed: postgresql://hindsight:dbpw-XYZ@127.0.0.1 api-XYZ"), errors.New("exit status 1")
	}
	err := n.StartDatabase(ctx)
	if err == nil || strings.Contains(err.Error(), "dbpw-XYZ") || strings.Contains(err.Error(), "api-XYZ") || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("StartDatabase: %v", err)
	}
	tools.Fail = func(toolCall) error { return errors.New("hindsight-admin: llm-XYZ refused") }
	if _, err := n.RenameBank(ctx, "a", "b", false); err == nil || strings.Contains(err.Error(), "llm-XYZ") {
		t.Errorf("RenameBank: %v", err)
	}
}

// RF 4: the user's Python settings reach neither the units nor server.env.
func TestNativeUnitsCarryOnlyTheirOwnEnvironment(t *testing.T) {
	t.Setenv("PYTHONPATH", "/evil/py")
	t.Setenv("VIRTUAL_ENV", "/evil/venv")
	t.Setenv("UV_INDEX_URL", "https://evil.example/simple")
	n, _, _ := nativeFixture(t, secrets)
	f, err := n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := unitEnvironmentKeys(f.Units["backsight-db.service"]), []string{"BACKSIGHT_HOME", "HOME", "LD_LIBRARY_PATH", "XDG_STATE_HOME"}; !reflect.DeepEqual(got, want) {
		t.Errorf("db unit Environment= keys %v, want %v", got, want)
	}
	if got := unitEnvironmentKeys(f.Units["backsight-server.service"]); len(got) != 0 {
		t.Errorf("server unit Environment= keys %v, want none (its environment is server.env)", got)
	}
	// Ruling R31: what the user manager imported (systemctl --user
	// import-environment) is unset in both units; the server, which sets
	// no loader path of its own, also drops LD_LIBRARY_PATH.
	for name, u := range f.Units {
		unset := strings.Fields(unitLine(u, "UnsetEnvironment"))
		for _, k := range []string{"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONUSERBASE", "PYTHONNOUSERSITE", "VIRTUAL_ENV",
			"CONDA_PREFIX", "PIP_INDEX_URL", "PIP_EXTRA_INDEX_URL", "PIP_REQUIRE_VIRTUALENV", "UV_INDEX_URL", "UV_EXTRA_INDEX_URL",
			"HF_HUB_OFFLINE", "HF_ENDPOINT", "HF_HUB_CACHE", "TRANSFORMERS_CACHE", "SENTENCE_TRANSFORMERS_HOME", "PGHOST", "PGPORT",
			"PGUSER", "PGDATABASE", "PGPASSFILE", "PGSERVICEFILE", "LD_PRELOAD"} {
			if !slices.Contains(unset, k) {
				t.Errorf("%s does not unset %s: UnsetEnvironment=%v", name, k, unset)
			}
		}
		if got := slices.Contains(unset, "LD_LIBRARY_PATH"); got != (name == "backsight-server.service") {
			t.Errorf("%s unsets LD_LIBRARY_PATH: %v (only the server unit, which sets none)", name, got)
		}
	}
	for _, u := range f.Units {
		for _, v := range []string{"/evil/", "evil.example"} {
			if strings.Contains(string(u), v) {
				t.Errorf("a unit carries the user's %s", v)
			}
		}
	}
	for _, k := range []string{"PYTHONPATH", "VIRTUAL_ENV", "UV_INDEX_URL"} {
		if slices.Contains(envFileKeys(f.Env), k) {
			t.Errorf("server.env sets %s", k)
		}
	}
	if strings.Contains(string(f.Env), "evil") {
		t.Error("server.env carries the user's environment")
	}
}

// The database's port is chosen once and kept: the instance and its
// clients know it.
func TestNativePgPortIsStable(t *testing.T) {
	n, _, _ := nativeFixture(t, secrets)
	calls := withFreePort(t, 8889, 9000)
	f := installed(t, n, secrets)
	if got := envValue(f.Env, "BACKSIGHT_PG_PORT"); got != "8889" {
		t.Fatalf("first render: port %s", got)
	}
	f2, err := n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(f2.Env, "BACKSIGHT_PG_PORT"); got != "8889" || !strings.Contains(string(f2.Units["backsight-db.service"]), " -p 8889 ") {
		t.Errorf("re-render: port %s, want 8889 kept", got)
	}
	if len(*calls) != 1 {
		t.Fatalf("freePort calls %v, want one", *calls)
	}
	if c := (*calls)[0]; c[0] != 8889 || !slices.Contains(c[1:], 8888) || !slices.Contains(c[1:], 9999) {
		t.Errorf("freePort(%d, %v): want from the API port + 1, skipping the API and UI ports", c[0], c[1:])
	}
}

// RF 2: only Start enables the units; Stop disables both; StopAPI and
// StartDatabase leave the other unit alone.
func TestNativeStartEnablesStopDisables(t *testing.T) {
	ctx := context.Background()
	n, sys, tools := nativeFixture(t, secrets)
	installed(t, n, secrets)
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl --user daemon-reload", "systemctl --user enable --now backsight-db.service", "systemctl --user enable --now backsight-server.service"}
	if got := sys.lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("Start ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.ContainsFunc(tools.Calls, func(c toolCall) bool { return filepath.Base(c.Path) == "psql" && slices.Contains(c.Args, "select 1") }) {
		t.Errorf("Start did not wait for the database to answer: %v", tools.Calls)
	}

	// Ruling R1: server.env alone changed, the running server restarts.
	n.Cfg.LLM.Model = "deepseek-other"
	f, err := n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := n.Write(f)
	if err != nil || !ch.Env || ch.Units {
		t.Fatalf("Write: %+v %v", ch, err)
	}
	if !exists(filepath.Join(n.Root(), "restart-pending")) {
		t.Fatal("a server.env change leaves no restart-pending")
	}
	sys.Calls = nil
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	want = []string{"systemctl --user daemon-reload", "systemctl --user enable --now backsight-db.service",
		"systemctl --user enable backsight-server.service", "systemctl --user restart backsight-server.service"}
	if got := sys.lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("Start after a server.env change ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if exists(filepath.Join(n.Root(), "restart-pending")) {
		t.Error("restart-pending stays after the restart")
	}

	// A db unit change restarts the database too.
	n.Env["XDG_DATA_HOME"] = "/elsewhere"
	f, err = n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	if ch, err := n.Write(f); err != nil || !ch.Units || ch.Env {
		t.Fatalf("Write: %+v %v", ch, err)
	}
	sys.Calls = nil
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	want = []string{"systemctl --user daemon-reload", "systemctl --user enable backsight-db.service", "systemctl --user restart backsight-db.service",
		"systemctl --user enable backsight-server.service", "systemctl --user restart backsight-server.service"}
	if got := sys.lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("Start after a db unit change ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A failed restart keeps the marker for the next Start.
	n.Cfg.LLM.Model = "deepseek-third"
	f, _ = n.Render(secrets, "box")
	if _, err := n.Write(f); err != nil {
		t.Fatal(err)
	}
	sys.Out = func(c []string) ([]byte, error) {
		if slices.Contains(c, "restart") {
			return []byte("Job failed"), errors.New("exit status 1")
		}
		return nil, nil
	}
	if err := n.Start(ctx); err == nil {
		t.Fatal("a failed restart must fail Start")
	}
	if !exists(filepath.Join(n.Root(), "restart-pending")) {
		t.Error("a failed restart dropped restart-pending")
	}
	sys.Out = nil

	sys.Calls = nil
	if err := n.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := sys.lines(), []string{"systemctl --user disable --now backsight-server.service backsight-db.service"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Stop ran %v, want %v", got, want)
	}
	if got := n.StopCmd(); got != "systemctl --user disable --now backsight-server.service backsight-db.service" {
		t.Errorf("StopCmd = %q", got)
	}
	sys.Calls = nil
	if err := n.StopAPI(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := sys.lines(), []string{"systemctl --user stop backsight-server.service"}; !reflect.DeepEqual(got, want) {
		t.Errorf("StopAPI ran %v, want %v", got, want)
	}
	sys.Calls = nil
	if err := n.StartDatabase(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := sys.lines(), []string{"systemctl --user daemon-reload", "systemctl --user start backsight-db.service"}; !reflect.DeepEqual(got, want) {
		t.Errorf("StartDatabase ran %v, want %v", got, want)
	}
}

// Start fails when the database never answers, within its bound.
func TestNativeStartWaitsForTheDatabase(t *testing.T) {
	ctx := context.Background()
	n, sys, tools := nativeFixture(t, secrets)
	installed(t, n, secrets)
	oldWait, oldPoll := dbWait, dbPoll
	dbWait, dbPoll = 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() { dbWait, dbPoll = oldWait, oldPoll })
	tools.Fail = func(c toolCall) error {
		if filepath.Base(c.Path) == "psql" {
			return errors.New("connection refused")
		}
		return nil
	}
	err := n.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Start: %v", err)
	}
	for _, l := range sys.lines() {
		if strings.Contains(l, "backsight-server.service") {
			t.Errorf("the server started before its database answered: %s", l)
		}
	}
}

func TestNativeStatusMapsUnitStates(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		show   string
		db, hs string
	}{
		{"ActiveState=active\nSubState=running\n\nActiveState=active\nSubState=running\n", "running", "running"},
		{"ActiveState=active\nSubState=running\n\nActiveState=activating\nSubState=auto-restart\n", "running", "restarting"},
		{"ActiveState=failed\nSubState=failed\n\nActiveState=inactive\nSubState=dead\n", "exited", "stopped"},
		{"ActiveState=activating\nSubState=start-post\n\nActiveState=deactivating\nSubState=stop-sigterm\n", "starting", "stopping"},
	} {
		n, sys, _ := nativeFixture(t, secrets)
		installed(t, n, secrets)
		sys.Out = func([]string) ([]byte, error) { return []byte(tc.show), nil }
		got, err := n.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := []Container{{Service: "db", State: tc.db}, {Service: "hindsight", State: tc.hs}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %+v, want %+v", tc.show, got, want)
		}
		if l := sys.lines(); len(l) != 1 || l[0] != "systemctl --user show -p ActiveState,SubState backsight-db.service backsight-server.service" {
			t.Errorf("Status ran %v", l)
		}
	}
	n, sys, _ := nativeFixture(t, secrets)
	if got, err := n.Status(ctx); err != nil || len(got) != 0 {
		t.Errorf("never installed: %v %v, want none", got, err)
	}
	if len(sys.Calls) != 0 {
		t.Errorf("Status without units ran %v", sys.lines())
	}
}

func TestNativeLogsReadTheJournal(t *testing.T) {
	n, sys, _ := nativeFixture(t, secrets)
	if _, err := n.Logs(context.Background(), "db", 40); err != nil {
		t.Fatal(err)
	}
	if got := sys.lines(); len(got) != 1 || got[0] != "journalctl --user -u backsight-db.service -n 40 --no-pager -o cat" {
		t.Errorf("Logs ran %v", got)
	}
	if _, err := n.Logs(context.Background(), "nginx", 40); err == nil {
		t.Error("Logs of an unknown service must fail")
	}
}

func TestNativeDeployedReadsTheUnit(t *testing.T) {
	n, _, _ := nativeFixture(t, secrets)
	if v, err := n.Deployed(); err != nil || v != "" {
		t.Errorf("never installed: %q %v", v, err)
	}
	old := n.WithVersion("1.0.9-00000000abcd").(Native)
	installed(t, old, secrets)
	if v, err := n.Deployed(); err != nil || v != "1.0.9-00000000abcd" {
		t.Errorf("Deployed = %q %v, want the unit's venv", v, err)
	}
	if n.Version() == "1.0.9-00000000abcd" {
		t.Error("Deployed must read the unit, not Version")
	}
}

func TestNativeDeleteStopsBeforeRemoving(t *testing.T) {
	ctx := context.Background()
	n, sys, _ := nativeFixture(t, secrets)
	installed(t, n, secrets)
	touch(t, filepath.Join(n.Home(), ".pg0", "instances", "hindsight", "data", "PG_VERSION"), 0o600)
	units := []string{filepath.Join(n.UnitDir, "backsight-db.service"), filepath.Join(n.UnitDir, "backsight-server.service")}
	var stoppedWithEverything bool
	sys.Out = func(c []string) ([]byte, error) {
		if slices.Contains(c, "disable") {
			stoppedWithEverything = exists(n.Root()) && exists(units[0]) && exists(units[1]) && exists(filepath.Join(n.Home(), ".pg0"))
		}
		return nil, nil
	}
	if err := n.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if !stoppedWithEverything {
		t.Error("the units were not disabled while their files and data still existed")
	}
	got := sys.lines()
	want := []string{"systemctl --user disable --now backsight-server.service backsight-db.service", "systemctl --user daemon-reload"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Delete ran %v, want %v", got, want)
	}
	for _, p := range append(units, n.Root()) {
		if exists(p) {
			t.Errorf("%s stays after Delete", p)
		}
	}
	journal, err := os.ReadFile(n.Dirs.JournalFile())
	if err != nil || !strings.Contains(string(journal), "backsight-server.service") {
		t.Errorf("the unit files were not removed through the config editor: %v", err)
	}
	// Nothing installed: nothing to stop, and no error.
	n2, sys2, _ := nativeFixture(t, secrets)
	if err := n2.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	for _, l := range sys2.lines() {
		if strings.Contains(l, "disable") {
			t.Errorf("Delete without units ran %s", l)
		}
	}
}

func TestNativeWriteOnlyWhatChanged(t *testing.T) {
	n, _, _ := nativeFixture(t, secrets)
	f, err := n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := n.Diff(f)
	if err != nil || !ch.Env || !ch.Units || ch.Compose {
		t.Fatalf("Diff before Write: %+v %v", ch, err)
	}
	if exists(n.EnvFile()) || exists(n.UnitDir) {
		t.Fatal("Diff wrote")
	}
	if ch, err = n.Write(f); err != nil || !ch.Env || !ch.Units {
		t.Fatalf("Write: %+v %v", ch, err)
	}
	if exists(filepath.Join(n.Root(), "restart-pending")) {
		t.Error("a first install leaves restart-pending: nothing was running")
	}
	if ch, err = n.Write(f); err != nil || ch.Env || ch.Units {
		t.Errorf("Write again: %+v %v, want no change", ch, err)
	}
	for name, want := range f.Units {
		got, err := os.ReadFile(filepath.Join(n.UnitDir, name))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if st, err := os.Stat(filepath.Join(n.Home(), ".pg0", "run")); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("postgres's socket directory: %v", err)
	}
}

func TestNativeIsARuntime(t *testing.T) {
	var rt Runtime = Native{}
	if rt.Kind() != "native" {
		t.Error(rt.Kind())
	}
	w := rt.WithVersion("1.0.9-00000000abcd")
	if w.Version() != "1.0.9-00000000abcd" || w.HindsightVersion() != "1.0.9" {
		t.Errorf("WithVersion: %q %q", w.Version(), w.HindsightVersion())
	}
	if rt.HindsightVersion() != nativelock.Hindsight() {
		t.Errorf("HindsightVersion = %q", rt.HindsightVersion())
	}
	n, _, _ := nativeFixture(t, secrets)
	if got := n.SecretFiles(); !reflect.DeepEqual(got, []string{n.EnvFile()}) {
		t.Errorf("SecretFiles = %v", got)
	}
	if ok, err := n.DataExists(context.Background()); ok || err != nil {
		t.Errorf("DataExists without an instance: %v %v", ok, err)
	}
	touch(t, filepath.Join(n.Home(), ".pg0", "instances", "hindsight", "data", "PG_VERSION"), 0o600)
	if ok, err := n.DataExists(context.Background()); !ok || err != nil {
		t.Errorf("DataExists: %v %v", ok, err)
	}
	if got := n.DataRemoveCmd(); got != "systemctl --user disable --now backsight-server.service backsight-db.service && rm -rf "+n.Home() {
		t.Errorf("DataRemoveCmd = %q", got)
	}
	r := n.Root()
	if got := n.ImagesRemoveCmd(); got != "rm -rf "+r+"/python "+r+"/venv "+r+"/hf "+r+"/uv" {
		t.Errorf("ImagesRemoveCmd = %q", got)
	}
	if got, err := n.Orchestrator(context.Background()); got != "" || err != nil {
		t.Errorf("Orchestrator without uv: %q %v", got, err)
	}
	installedUV(t, n)
	if got, _ := n.Orchestrator(context.Background()); got != "uv "+nativelock.UVVersion {
		t.Errorf("Orchestrator = %q", got)
	}
}

func TestNativeEngineIsTheUserManager(t *testing.T) {
	n, sys, _ := nativeFixture(t, secrets)
	sys.Out = func(c []string) ([]byte, error) {
		if slices.Contains(c, "--version") {
			return []byte("systemd 257 (257.5-1-arch)\n+PAM +AUDIT\n"), nil
		}
		return nil, nil
	}
	e, err := n.Engine(context.Background())
	if err != nil || e.Version != "systemd 257 (257.5-1-arch)" {
		t.Errorf("Engine = %+v %v", e, err)
	}
	sys.Out = func(c []string) ([]byte, error) {
		if slices.Contains(c, "show-environment") {
			return []byte("Failed to connect to bus: No medium found\n"), errors.New("exit status 1")
		}
		return nil, nil
	}
	if _, err := n.Engine(context.Background()); err == nil || !strings.Contains(err.Error(), "No medium found") {
		t.Errorf("Engine without a user manager: %v", err)
	}
}

// RenameBank runs the deployed venv's hindsight-admin with the server's
// own settings, the database's URL among them.
func TestNativeRenameBankRunsTheVenvsAdmin(t *testing.T) {
	n, _, tools := nativeFixture(t, secrets)
	installed(t, n, secrets)
	if _, err := n.RenameBank(context.Background(), "old", "new", true); err != nil {
		t.Fatal(err)
	}
	c := tools.Calls[len(tools.Calls)-1]
	if c.Path != filepath.Join(n.VenvDir(n.Version()), "bin", "hindsight-admin") {
		t.Errorf("path %s", c.Path)
	}
	if want := []string{"rename-bank", "--from", "old", "--to", "new", "--dry-run"}; !reflect.DeepEqual(c.Args, want) {
		t.Errorf("args %v, want %v", c.Args, want)
	}
	if !slices.Contains(c.Env, "HINDSIGHT_API_DATABASE_URL=postgresql://hindsight:fakedbpassword@127.0.0.1:8889/hindsight") ||
		!slices.Contains(c.Env, "PATH=/usr/bin:/bin") {
		t.Errorf("env %v", c.Env)
	}
}

// Ruling R21: pg0 creates the instance with its own default password in a
// transient unit, TMPDIR under the private home, then the real password
// goes in through psql's stdin.
func TestNativeCreatesTheInstanceWithThePasswordOnStdin(t *testing.T) {
	n, _, tools := nativeFixture(t, config.Secrets{DBPassword: "dbpw-XYZ", APIKey: "a", UIKey: "u", LLMAPIKey: "l"})
	installed(t, n, config.Secrets{DBPassword: "dbpw-XYZ", APIKey: "a", UIKey: "u", LLMAPIKey: "l"})
	tmp := filepath.Join(n.Home(), "tmp")
	var tmpMode os.FileMode
	tools.Effect = func(toolCall) {
		if st, err := os.Stat(tmp); err == nil {
			tmpMode = st.Mode().Perm()
		}
	}
	if err := n.createInstance(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := tools.Calls[len(tools.Calls)-1]
	if c.Path != "systemd-run" {
		t.Errorf("path %s", c.Path)
	}
	argv := strings.Join(c.Args, " ")
	for _, want := range []string{"--user --wait --collect --pipe --quiet --unit backsight-db-init -p KillMode=control-group",
		"-E HOME=" + n.Home(), "-E PGPASSWORD=postgres", "-E TMPDIR=" + tmp,
		"start --name hindsight --port 8889 -u hindsight -n hindsight -c fsync=on",
		"psql -h 127.0.0.1 -p 8889 -U postgres -d postgres -v ON_ERROR_STOP=1 -q -f -", "stop --name hindsight"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "dbpw-XYZ") {
		t.Error("the password is on systemd-run's argv")
	}
	if want := "SET log_min_error_statement = panic;\nALTER ROLE hindsight PASSWORD 'dbpw-XYZ';\nALTER ROLE postgres PASSWORD 'dbpw-XYZ';\n"; string(c.Stdin) != want {
		t.Errorf("stdin %q, want %q", c.Stdin, want)
	}
	if tmpMode != 0o700 {
		t.Errorf("TMPDIR mode %v during the creation, want 0700", tmpMode)
	}
	if exists(tmp) {
		t.Error("TMPDIR stays after the creation")
	}
}

// Every method returns the construction error.
func TestNativeErrIsReturned(t *testing.T) {
	boom := errors.New("no home directory")
	n := Native{Err: boom}
	ctx := context.Background()
	if _, err := n.Render(secrets, "box"); !errors.Is(err, boom) {
		t.Errorf("Render: %v", err)
	}
	if _, err := n.Write(Files{}); !errors.Is(err, boom) {
		t.Errorf("Write: %v", err)
	}
	for name, err := range map[string]error{"Start": n.Start(ctx), "Stop": n.Stop(ctx), "StopAPI": n.StopAPI(ctx),
		"StartDatabase": n.StartDatabase(ctx), "Delete": n.Delete(ctx)} {
		if !errors.Is(err, boom) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := n.Status(ctx); !errors.Is(err, boom) {
		t.Errorf("Status: %v", err)
	}
}

// Setting up again over a kept native server adopts its keys from
// server.env: the database password from its URL, the API key; it has no
// UI key (ruling R26).
func TestKeptSecretsFromTheNativeEnvironment(t *testing.T) {
	sec := config.Secrets{DBPassword: "pw-with%and-ZZZZ", APIKey: "fakeapikey", UIKey: "fakeuikey", LLMAPIKey: "fakellmkey"}
	n, _, _ := nativeFixture(t, sec)
	installed(t, n, sec)
	got, err := KeptSecretsIn(n.SecretFiles()[0])
	if want := map[string]string{"db_password": sec.DBPassword, "api_key": sec.APIKey}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("%v %v, want %v", got, err, want)
	}
}

// Ruling R30: Start returns a running server, as Docker's compose up
// --wait does: the API answers /health with its database connected.
func TestNativeStartWaitsForTheAPI(t *testing.T) {
	ctx := context.Background()
	n, sys, _ := nativeFixture(t, secrets)
	installed(t, n, secrets)
	oldWait, oldPoll := apiWait, apiPoll
	apiWait, apiPoll = 30*time.Millisecond, time.Millisecond
	t.Cleanup(func() { apiWait, apiPoll = oldWait, oldPoll })

	answers := []string{"", "disconnected", "connected"}
	urls := withAPIHealth(t, func(context.Context, string) (string, error) {
		a := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		if a == "" {
			return "", errors.New("connection refused")
		}
		return a, nil
	})
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*urls) != 3 || (*urls)[0] != "http://127.0.0.1:8888" {
		t.Errorf("health asked %v, want three times on 127.0.0.1's API port", *urls)
	}

	// The API never connects: an error naming the journal, the marker kept.
	n.Cfg.LLM.Model = "deepseek-other"
	f, _ := n.Render(secrets, "box")
	if _, err := n.Write(f); err != nil {
		t.Fatal(err)
	}
	withAPIHealth(t, func(context.Context, string) (string, error) { return "disconnected", nil })
	sys.Calls = nil
	err := n.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "journalctl --user -u backsight-server.service") {
		t.Fatalf("Start: %v", err)
	}
	if !slices.Contains(sys.lines(), "systemctl --user restart backsight-server.service") {
		t.Errorf("Start did not restart: %v", sys.lines())
	}
	if !exists(filepath.Join(n.Root(), "restart-pending")) {
		t.Error("restart-pending removed though the API never answered")
	}
	withAPIHealth(t, func(context.Context, string) (string, error) { return "connected", nil })
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(n.Root(), "restart-pending")) {
		t.Error("restart-pending stays once the API answered")
	}
}

// Ruling R32: Ollama on host.docker.internal is this PC on native.
func TestNativeRendersDockersHostAsLoopback(t *testing.T) {
	cfg := dockerGoldenConfigs()["private-ollama"]
	n, _, _ := nativeFixture(t, secrets)
	n.Cfg.LLM = cfg.LLM
	f, err := n.Render(secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(f.Env, "HINDSIGHT_API_LLM_BASE_URL"); got != "http://127.0.0.1:11434" {
		t.Errorf("base URL %q, want http://127.0.0.1:11434", got)
	}
	if strings.Contains(string(f.Env), "host.docker.internal") {
		t.Error("server.env names host.docker.internal")
	}
	n.Cfg.LLM.BaseURL = "https://ollama.example:11434/v1"
	withFreePort(t, 8889)
	if f, err := n.Render(secrets, "box"); err != nil || envValue(f.Env, "HINDSIGHT_API_LLM_BASE_URL") != "https://ollama.example:11434/v1" {
		t.Errorf("another host was rewritten: %v", err)
	}
}

// Security: a failed password change leaves no instance on pg0's default
// password, and postgres never logs the failed statement.
func TestNativeCreateInstanceRemovesItOnFailure(t *testing.T) {
	sec := config.Secrets{DBPassword: "dbpw-XYZ", APIKey: "a", UIKey: "u", LLMAPIKey: "l"}
	n, _, tools := nativeFixture(t, sec)
	installed(t, n, sec)
	inst := filepath.Dir(n.DataDir())
	tools.Fail = func(toolCall) error {
		touch(t, filepath.Join(n.DataDir(), "PG_VERSION"), 0o600) // pg0 created it, then ALTER failed
		return errors.New("exit status 3: ERROR: syntax error")
	}
	err := n.createInstance(context.Background())
	if err == nil {
		t.Fatal("createInstance succeeded")
	}
	if exists(inst) {
		t.Errorf("%s stays on pg0's default password", inst)
	}
	if !exists(n.Home()) {
		t.Error("home/ removed with the instance")
	}
	c := tools.Calls[len(tools.Calls)-1]
	if !strings.HasPrefix(string(c.Stdin), "SET log_min_error_statement = panic;\n") {
		t.Errorf("stdin %q does not silence the statement log first", c.Stdin)
	}
}
