package scripts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/plugin"
)

// The scripts vm-bench.sh runs on A (vb_offline_script, vb_latency_script),
// run here against a fake backsight, curl and Claude Code hook command,
// with the hooks.json backsight itself generates: what they run, with
// what input, against which deadline, and what they leave.

// aFakeBacksight is A's backsight: paths under HOME; jobs tick skipped on
// its first call (another tick running), then partial, recording how many
// transcripts it found as reach.json's sessions_waiting; store queued;
// doctor empty. Every call is logged to HOME/calls.
const aFakeBacksight = `#!/bin/sh
h=$HOME
echo "$*" >> "$h/calls"
case "$*" in
  "paths --json") printf '{"ok":true,"data":{"state":"%s/state","data":"%s/data"}}\n' "$h" "$h" ;;
  "jobs tick --json")
    n=$(cat "$h/ticks" 2> /dev/null || echo 0); n=$((n + 1)); echo "$n" > "$h/ticks"
    if [ "$n" = 1 ]; then echo '{"ok":true,"data":{"status":"skipped","reason":"another tick is running"}}'; exit 0; fi
    w=$(find "$h/.claude/projects" -name '*.jsonl' | wc -l)
    jq --argjson w "$w" '.sessions_waiting = $w' "$h/state/reach.json" > "$h/state/r.tmp" && mv "$h/state/r.tmp" "$h/state/reach.json"
    printf '{"ok":false,"data":{"status":"partial","ms":%d,"call":%d}}\n' $((10 + w)) "$n"; exit 40 ;;
  store*) echo '{"ok":true,"data":{"status":"queued","document":"note:x"}}' ;;
  "doctor --json") echo '{"ok":true,"data":{"checks":[]}}' ;;
  *) echo '{"ok":false}'; exit 1 ;;
esac
`

// aFakeHook is the command hooks.json wires: it logs "<argv>\t<stdin>" to
// $HOOKLOG, sleeps $SLOW_S on the hook $SLOW_HOOK, and for a prompt writes
// a prompt telemetry event of its session to $TEL.
const aFakeHook = `#!/bin/sh
in=$(cat)
printf '%s\t%s\n' "$*" "$in" >> "$HOOKLOG"
if [ -n "${SLOW_HOOK:-}" ] && [ "$2" = "$SLOW_HOOK" ]; then sleep "$SLOW_S"; fi
if [ "$2" = user-prompt-submit ] && [ -n "${TEL:-}" ]; then
  jq -c --arg ts "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" '{ts: $ts, event: "prompt", session: .session_id, n: (.prompt_id | split("-p")[1] | tonumber), decision: "recall", ms: 5}' >> "$TEL" <<EOT
$in
EOT
fi
echo '{}'
`

// aHome is A's home for the scripts: the fakes, backsight's own
// hooks.json for the fake hook command, a state with reach.json failing.
func aHome(t *testing.T) (home, bin string) {
	t.Helper()
	root := t.TempDir()
	home, bin = filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{bin, filepath.Join(home, "state", "telemetry")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(home, "backsight"), aFakeBacksight)
	writeExec(t, filepath.Join(bin, "hook"), aFakeHook)
	writeExec(t, filepath.Join(bin, "curl"), "#!/bin/sh\nexit ${CURL_RC:-28}\n")
	if _, err := plugin.Generate(filepath.Join(home, "data", "marketplace"), filepath.Join(bin, "hook"), "0.940.0"); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(home, "state", "reach.json"), `{"last_ok":"2026-10-29T03:00:00Z","failing_since":"2026-10-29T04:00:00Z"}`)
	return home, bin
}

// runAScript writes the script vm-bench.sh's function prints into dir and
// runs it as the bench does (its own session, setsid -w bash), with HOME,
// the fakes first on PATH and env; it returns stderr and the exit code.
func runAScript(t *testing.T, home, bin, fn, dir string, args []string, env ...string) (string, string, int) {
	t.Helper()
	gen := exec.Command("bash", "-c", `. "$1"; `+fn, "vb", mustAbs(t, "vm-bench.sh"))
	script, err := gen.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), script, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("setsid", append([]string{"-w", "bash", filepath.Join(dir, "run.sh")}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "HOOKLOG="+filepath.Join(home, "hook.log"))
	cmd.Env = append(cmd.Env, env...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return o.String(), e.String(), ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String(), 0
}

type hookRun struct {
	Name, Event string
	MS          int
	Timeout     int `json:"timeout_s"`
	RC          int
}

func readHookRuns(t *testing.T, path string) []hookRun {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []hookRun
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var h hookRun
		if err := json.Unmarshal([]byte(l), &h); err != nil {
			t.Fatalf("hooks.jsonl line %q: %v", l, err)
		}
		out = append(out, h)
	}
	return out
}

// hookLog: each hook command's argv and the JSON it read.
func hookLog(t *testing.T, home string) (argv []string, in []map[string]string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "hook.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		a, j, _ := strings.Cut(l, "\t")
		var m map[string]string
		if err := json.Unmarshal([]byte(j), &m); err != nil {
			t.Fatalf("hook input %q: %v", j, err)
		}
		argv, in = append(argv, a), append(in, m)
	}
	return argv, in
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func offlinePrompts() []string {
	var p []string
	for _, w := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"} {
		p = append(p, "the prompt number "+w+" of the offline laptop, it's long enough to recall")
	}
	return p
}

// The offline laptop script: the hooks Claude Code would run, from
// backsight's own hooks.json, with their deadlines; a skipped tick tried
// again; reach.json backdated 2 h; 200 transcripts surveyed then removed;
// /health still failing at the end.
func TestVMBenchOfflineScriptOnA(t *testing.T) {
	home, bin := aHome(t)
	d := filepath.Join(home, "m6-offline")
	prompts := offlinePrompts()
	_, stderr, code := runAScript(t, home, bin, "vb_offline_script", d, append([]string{"laptop", "http://srv:8888", "360", "bs-m6-offline", "of1"}, prompts...))
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(d, "error")); err == nil {
		t.Fatalf("error: %s", readFile(t, filepath.Join(d, "error")))
	}
	runs := readHookRuns(t, filepath.Join(d, "hooks.jsonl"))
	var names []string
	for i, r := range runs {
		names = append(names, r.Name)
		want := map[string]int{"SessionStart": 10, "UserPromptSubmit": 25, "SessionEnd": 1}[r.Event]
		if r.Timeout != want || r.RC != 0 {
			t.Errorf("hook %d %+v: want timeout %d s, rc 0", i, r, want)
		}
	}
	if strings.Join(names, " ") != "start1 p1 p2 p3 p4 p5 p6 p7 p8 p9 p10 end start2" {
		t.Errorf("hooks %q", names)
	}
	argv, in := hookLog(t, home)
	if len(in) != 13 {
		t.Fatalf("%d hook commands ran, want 13", len(in))
	}
	for i, want := range map[int][4]string{
		0: {"hook session-start", "of1", "SessionStart", "startup"}, 1: {"hook user-prompt-submit", "of1", "UserPromptSubmit", ""},
		11: {"hook session-end", "of1", "SessionEnd", ""}, 12: {"hook session-start", "of2", "SessionStart", "startup"},
	} {
		if argv[i] != want[0] || in[i]["session_id"] != want[1] || in[i]["hook_event_name"] != want[2] || in[i]["source"] != want[3] ||
			in[i]["cwd"] != filepath.Join(home, "work", "bs-m6-offline") || in[i]["transcript_path"] != filepath.Join(home, ".claude", "projects", "-bs-m6-bs-m6-offline", want[1]+".jsonl") {
			t.Errorf("hook %d: %s %v, want %v", i, argv[i], in[i], want)
		}
	}
	for i, p := range prompts {
		if in[i+1]["prompt"] != p || in[i+1]["prompt_id"] != "of1-p"+strings.TrimPrefix(names[i+1], "p") {
			t.Errorf("prompt %d: %v", i+1, in[i+1])
		}
	}
	// The skipped tick was tried again: tick1 is the second call's.
	if tick1 := readFile(t, filepath.Join(d, "tick1.json")); !strings.Contains(tick1, `"call":2`) {
		t.Errorf("tick1 %s, want the retry after the skipped one", tick1)
	}
	var reach map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(home, "state", "reach.json"))), &reach); err != nil {
		t.Fatal(err)
	}
	back := readFile(t, filepath.Join(d, "backdated"))
	if at, err := time.Parse(time.RFC3339, back); err != nil || time.Since(at) < 119*time.Minute || time.Since(at) > 121*time.Minute ||
		reach["failing_since"] != back || reach["last_ok"] != back {
		t.Errorf("backdated %q, reach.json %v", back, reach)
	}
	if n := readFile(t, filepath.Join(d, "unsent-files")); n != "200" {
		t.Errorf("unsent files %s", n)
	}
	var base, unsent struct {
		Waiting int `json:"sessions_waiting"`
	}
	_ = json.Unmarshal([]byte(readFile(t, filepath.Join(d, "reach-base.json"))), &base)
	_ = json.Unmarshal([]byte(readFile(t, filepath.Join(d, "reach-unsent.json"))), &unsent)
	if unsent.Waiting != base.Waiting+200 {
		t.Errorf("sessions waiting %d then %d", base.Waiting, unsent.Waiting)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "projects", "-bs-m6-bs-m6-unsent")); err == nil {
		t.Error("the 200 transcripts stayed")
	}
	recs := strings.Split(readFile(t, filepath.Join(home, ".claude", "projects", "-bs-m6-bs-m6-offline", "of1.jsonl")), "\n")
	if len(recs) != 3 || !strings.Contains(recs[2], `"uuid":"of1-r3"`) {
		t.Errorf("of1's records: %q", recs)
	}
	for f, want := range map[string]string{"health-end": "down", "down-rc": "28", "down-after": "0"} {
		if got := readFile(t, filepath.Join(d, f)); got != want {
			t.Errorf("%s %q, want %q", f, got, want)
		}
	}
	steps := readFile(t, filepath.Join(d, "steps.jsonl"))
	for _, w := range []string{`"name":"store1"`, `"bound_s":60`, `"name":"tick1"`, `"bound_s":300`, `"name":"doctor1"`, `"bound_s":120`} {
		if !strings.Contains(steps, w) {
			t.Errorf("steps.jsonl lacks %s:\n%s", w, steps)
		}
	}
	readFile(t, filepath.Join(d, "done"))
}

// A hook that outlives its deadline is killed past it and recorded late
// (its rc 124), the script going on; the bench's reader FAILs naming it.
func TestVMBenchOfflineScriptRecordsALateHook(t *testing.T) {
	home, bin := aHome(t)
	d := filepath.Join(home, "m6-offline")
	_, stderr, code := runAScript(t, home, bin, "vb_offline_script", d, append([]string{"hung", "http://srv:8888", "360", "bs-m6-offline", "oh1"}, offlinePrompts()...),
		"SLOW_HOOK=session-end", "SLOW_S=6")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	runs := readHookRuns(t, filepath.Join(d, "hooks.jsonl"))
	end := runs[len(runs)-1]
	if end.Name != "end" || end.RC != 124 || end.MS < 1000 || end.MS > 5500 {
		t.Errorf("the late hook %+v: want rc 124, killed between its deadline (1 s) and 3 s more", end)
	}
	if readFile(t, filepath.Join(d, "health-end")) != "down" {
		t.Error("the script stopped at the late hook")
	}
	// The hung mode runs no backdate and no unsent survey.
	for _, f := range []string{"backdated", "unsent-files", "out/start2"} {
		if _, err := os.Stat(filepath.Join(d, f)); err == nil {
			t.Errorf("the hung mode wrote %s", f)
		}
	}
	readFile(t, filepath.Join(d, "breaker-end.json"))
	out, errs, code := vmBenchArgs(t, `vb_hooks_in_time "`+filepath.Join(d, "hooks.jsonl")+`"`)
	if code == 0 || !strings.Contains(errs, "past its deadline: end (SessionEnd)") {
		t.Errorf("the reader on a late hook: exit %d, %s %s", code, out, errs)
	}
}

// vmBenchArgs sources vm-bench.sh and runs script, fail printing its message.
func vmBenchArgs(t *testing.T, script string) (string, string, int) {
	t.Helper()
	return vmBench(t, newBenchFakes(t), t.TempDir(), filepath.Join(t.TempDir(), "bs-m6"), script)
}

// The watchdog: past its bound the script's whole session is stopped,
// the bound said in error, done still written.
func TestVMBenchOfflineScriptStopsAtItsBound(t *testing.T) {
	home, bin := aHome(t)
	d := filepath.Join(home, "m6-offline")
	start := time.Now()
	_, _, _ = runAScript(t, home, bin, "vb_offline_script", d, append([]string{"laptop", "http://srv:8888", "2", "bs-m6-offline", "of1"}, offlinePrompts()...),
		"SLOW_HOOK=session-start", "SLOW_S=9")
	if el := time.Since(start); el > 7*time.Second {
		t.Errorf("the script ran %s past a 2 s bound", el)
	}
	if e := readFile(t, filepath.Join(d, "error")); !strings.Contains(e, "passed its bound of 2 s") {
		t.Errorf("error %q", e)
	}
	readFile(t, filepath.Join(d, "done"))
	if _, err := os.Stat(filepath.Join(d, "health-end")); err == nil {
		t.Error("the script went on past its bound")
	}
}

// The latency script: it waits for an open breaker, then a startup and
// one prompt hook a prompt of session lt<d>, and answers with their wall
// times, the session's prompt events since its start and only the hook
// timeouts since then.
func TestVMBenchLatencyScriptOnA(t *testing.T) {
	home, bin := aHome(t)
	tel := filepath.Join(home, "state", "telemetry", "events-"+time.Now().UTC().Format("2006-01")+".jsonl")
	writeLines(t, tel,
		`{"ts":"2020-01-01T00:00:00.000Z","event":"hook_failure","hook":"user-prompt-submit","kind":"timeout"}`,
		`{"ts":"2020-01-01T00:00:00.000Z","event":"prompt","session":"lt20","n":7,"decision":"skip"}`,
		`not json`)
	writeLines(t, filepath.Join(home, "state", "breaker.json"), `{"failures":0,"open_until":"`+time.Now().UTC().Add(1500*time.Millisecond).Format(time.RFC3339Nano)+`"}`)
	var prompts []string
	for i := 0; i < 40; i++ {
		prompts = append(prompts, "a latency prompt long enough to be recalled by the hook, number "+string(rune('A'+i)))
	}
	out, stderr, code := runAScript(t, home, bin, "vb_latency_script", filepath.Join(home, "m6-latency"),
		append([]string{"20", "bs-m6-latency", "600"}, prompts...), "TEL="+tel)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	var res struct {
		Waited   int       `json:"waited_s"`
		Hooks    []hookRun `json:"hooks"`
		Prompts  []map[string]any
		Failures []map[string]any
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("answer %q: %v", out, err)
	}
	if res.Waited < 1 {
		t.Errorf("waited %d s for a breaker open 1.5 s more", res.Waited)
	}
	if len(res.Hooks) != 41 || res.Hooks[0].Name != "start" || res.Hooks[0].Timeout != 10 || res.Hooks[40].Name != "p40" || res.Hooks[40].Timeout != 25 {
		t.Errorf("hooks %+v", res.Hooks)
	}
	if len(res.Prompts) != 40 || res.Prompts[0]["n"] != 1.0 || res.Prompts[39]["n"] != 40.0 {
		t.Errorf("prompts %v", res.Prompts)
	}
	if len(res.Failures) != 0 {
		t.Errorf("failures from before the start counted: %v", res.Failures)
	}
	_, in := hookLog(t, home)
	if in[1]["session_id"] != "lt20" || in[1]["prompt"] != prompts[0] || in[0]["source"] != "startup" {
		t.Errorf("inputs %v %v", in[0], in[1])
	}
}
