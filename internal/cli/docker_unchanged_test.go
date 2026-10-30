package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
)

// The goldens of this file were recorded from 0014c66, before the native
// runtime (M7 block 1): a Docker install must keep running exactly these
// commands, writing exactly this config. A JSON envelope may only gain
// fields, by the task that adds them (preflight ruling R17).
var updateGolden = flag.Bool("update", false, "rewrite the golden files")

func cliGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *updateGolden {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s drifted (go test ./internal/cli -run %s -update, only for a change Docker installs must see):\n%s", name, t.Name(), got)
	}
}

// argvRecorder is the Docker every command of a Docker install reaches:
// it records each argv whole, has no volume until the first compose up,
// answers the pending operations of a bank rename (none), and leaves the
// rest to restore's fake server.
type argvRecorder struct {
	mu      sync.Mutex
	inner   *restoreDocker
	argv    [][]string
	volumes bool
}

func (r *argvRecorder) Run(ctx context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.argv = append(r.argv, slices.Clone(args))
	switch {
	case len(args) > 1 && args[0] == "volume" && args[1] == "inspect" && !r.volumes:
		r.mu.Unlock()
		return []byte("Error: no such volume: " + args[len(args)-1]), errors.New("exit status 1")
	case slices.Contains(args, "up"):
		r.volumes = true
	}
	r.mu.Unlock()
	if slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "from async_operations") }) {
		return []byte("0\n"), nil
	}
	return r.inner.Run(ctx, args...)
}

// randomPart: the throwaway names, passwords and staging directories a
// run draws.
var randomPart = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(backsight-verify-|verify-|/tmp/backsight-)[0-9a-f]{12}`), "${1}<rand>"},
	{regexp.MustCompile(`(/staging/[a-z]+-)[0-9-]+/`), "${1}<rand>/"},
}

// argvText is one argv per line, each argument quoted, the run's home and
// random names replaced.
func argvText(home string, argv [][]string) string {
	var b strings.Builder
	for _, a := range argv {
		q := make([]string, len(a))
		for i, s := range a {
			s = strings.ReplaceAll(s, home, "<home>")
			for _, r := range randomPart {
				s = r.re.ReplaceAllString(s, r.with)
			}
			q[i] = strconv.Quote(s)
		}
		b.WriteString(strings.Join(q, " ") + "\n")
	}
	return b.String()
}

// jsonKeys lists the key paths of a JSON value: "a.b", "a[].c".
func jsonKeys(prefix string, v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out[p] = true
			jsonKeys(p, x, out)
		}
	case []any:
		for _, x := range t {
			jsonKeys(prefix+"[]", x, out)
		}
	}
}

// dockerInstallRun is a Docker install from scratch driven through every
// command that reaches Docker, in order; it returns the argv they ran and
// the key paths of each command's JSON envelope, each under its command.
func dockerInstallRun(t *testing.T) (argv, keys string) {
	t.Helper()
	home := isolate(t)
	port := fakeHindsight(t)
	d := pathsOf(home)
	dk := &restoreDocker{lock: d.MaintenanceLock(), rows: 10, copies: map[string]int{}, running: true,
		sqlBanks: `["backsight","backsight-control","backsight-docs"]`}
	rec := &argvRecorder{inner: dk}
	old, oldPoll, oldRename := composeRunner, healthPoll, renamePoll
	composeRunner, healthPoll, renamePoll = rec, time.Millisecond, time.Millisecond
	t.Cleanup(func() { composeRunner, healthPoll, renamePoll = old, oldPoll, oldRename })
	withSys(t, "linux")
	withClaude(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	t.Setenv("LLM", "fakellmkey")
	if code, _, errOut := run(t, "secret", "set", "llm_api_key", "--from-env", "LLM"); code != output.OK {
		t.Fatal(errOut)
	}
	archive := filepath.Join(d.Data, "backups", "backsight-20261025T040000Z.tar.gz.age")

	var ab, kb strings.Builder
	step := func(args ...string) {
		t.Helper()
		start := len(rec.argv)
		code, out, errOut := run(t, append(args, "--json")...)
		if code != output.OK {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
		if strings.Contains(out, "units_changed") {
			t.Errorf("%v: a Docker install reports units: %s", args, out)
		}
		var env any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("%v: not one JSON envelope: %v\n%s", args, err, out)
		}
		set := map[string]bool{}
		jsonKeys("", env, set)
		names := make([]string, 0, len(set))
		for k := range set {
			names = append(names, k)
		}
		sort.Strings(names)
		title := "# " + strings.ReplaceAll(strings.ReplaceAll(strings.Join(args, " "), archive, "<archive>"), strconv.Itoa(port), "<port>") + "\n"
		ab.WriteString(title + argvText(home, rec.argv[start:]))
		kb.WriteString(title + strings.Join(names, "\n") + "\n")
	}
	step("server", "install", "--api-port", strconv.Itoa(port), "--ui-port", "19999")
	step("server", "render") // the files' changes, units among them
	step("server", "start")
	step("server", "stop")
	step("server", "status")
	step("backup", "create")
	step("backup", "verify", archive)
	withClock(t, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	step("restore", archive)
	pinImage(t, newImage)
	step("server", "upgrade")
	// A legacy bank, for bank rename to move.
	raw, err := os.ReadFile(d.ConfigFile())
	if err != nil || !strings.Contains(string(raw), `bank = "backsight"`) {
		t.Fatalf("config.toml names no bank: %v\n%s", err, raw)
	}
	if err := os.WriteFile(d.ConfigFile(), []byte(strings.Replace(string(raw), `bank = "backsight"`, `bank = "claude-memory"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	dk.mu.Lock()
	dk.sqlBanks = `["claude-memory","claude-memory-control","claude-memory-docs"]`
	dk.mu.Unlock()
	step("bank", "rename")
	step("uninstall", "--delete-data", "--answer", "confirm_bank="+config.DefaultBank)
	return ab.String(), kb.String()
}

// Every command a Docker install runs reaches Docker with the argv it ran
// before the native runtime, and its JSON envelope reports no units.
func TestDockerCommandsRunTheSameArgv(t *testing.T) {
	argv, keys := dockerInstallRun(t)
	cliGolden(t, "docker-argv.golden", []byte(argv))
	cliGolden(t, "docker-json-keys.golden", []byte(keys))
}

// setup's config.toml for the default answers, without a runtime answer:
// byte for byte what a Docker install was given before.
func TestSetupConfigUnchangedWithoutRuntime(t *testing.T) {
	qs, extra, err := setup.Resolve(setup.Inputs{Answers: map[string]string{"provider": "deepseek", "consent": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if missing := setup.Defaults(qs); len(missing) > 0 {
		t.Fatalf("no default for %v", missing)
	}
	answers := map[string]string{}
	for k, v := range extra {
		answers[k] = v
	}
	for _, q := range qs {
		answers[q.ID] = q.Value
	}
	cliGolden(t, "setup-config.golden.toml", setupConfig(answers))
}

// doctor's checks on a Docker install, by id and in order (preflight
// ruling R16): the native runtime adds none to a Docker install.
func TestDoctorChecksOfADockerInstallAreUnchanged(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the golden is a Linux machine's")
	}
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withCompose(t)
	withSys(t, "linux")
	withClaude(t)
	if code, out, errOut := run(t, "server", "render"); code != output.OK {
		t.Fatalf("render: exit %d\n%s%s", code, out, errOut)
	}
	_, out, _ := run(t, "doctor", "--json")
	var env struct {
		Data doctor.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	ids := make([]string, len(env.Data.Checks))
	for i, c := range env.Data.Checks {
		ids[i] = c.ID
	}
	cliGolden(t, "doctor-checks-docker.golden", []byte(strings.Join(ids, "\n")+"\n"))
}
