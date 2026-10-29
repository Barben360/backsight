package scripts

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The checks of the client VM's network (offline-laptop, offline-hung,
// unsent-cost, latency-*) against fakes: A is a set of shell functions
// answering from files under T, the link, the drop and netem the fake
// virsh and ssh. What they prove: every way out restores the network and
// leaves nothing on A, and each positive control fails a reader that
// could not see.

// netDriver runs after vm-bench.sh and m6-proofs.sh are sourced: A's side
// of the driver, from files of T. m6_sh logs each command to T/m6sh.calls;
// sleep is instant (with T/kill-in-sleep, the check is killed there).
const netDriver = `
M6_RESULTS=$T/results.jsonl M6_WORK=$T/work M6_ROOT=$T/root M6_SERVER_IP=192.168.122.196 M6_OFFLINE_S=0 M6_HUNG_S=60
mkdir -p "$M6_WORK"
sleep() { if [ -e "$T/kill-in-sleep" ]; then rm -f "$T/kill-in-sleep"; kill -TERM "$BASHPID"; fi; }
m6_sh() {
  printf '%s\n' "$1 $2" >> "$T/m6sh.calls"
  case $2 in
    "tar -C ~/m6-offline-hung -cf - .") tar -C "$T/fixture-hung" -cf - . ;;
    "tar -C ~/m6-offline -cf - .") tar -C "$T/fixture" -cf - . ;;
    "test -e ~/m6-offline"*"/done") [ ! -e "$T/never-done" ] ;;
    "ip -o route get"*) echo "192.168.122.196 dev enp1s0 src 192.168.122.195 uid 1000" ;;
    "ping "*) cat "$T/ping" ;;
    "date +%s") echo 1761700000 ;;
    "setsid -w bash "*m6-latency/run.sh*) cat "$T/latency.json" ;;
  esac
}
m6_put() { cat > /dev/null; }
m6_get() { printf '[server]\nurl = "http://192.168.122.196:8888"\n'; }
m6_paths() { echo '{"config_file":"/c","state":"/s"}'; }
m6_outbox() { echo 0; }
m6_live_since() { :; }
vb_wait_a() { :; }
vb_telemetry_since() { cat "$T/telemetry"; }
m6_tick() { if [ -e "$T/tick-fails" ]; then fail "jobs tick on a did not succeed"; fi; echo '{"ok":true,"data":{"status":"done"}}'; }
m6_queue_wait() { :; }
m6_wait_doc() { echo '{}'; }
m6_uuid_counts() { printf "$(cat "$T/sid")-r%s 1\n" 1 2 3; }
m6_doc() { echo '{"id":"x"}'; }
m6_doctor() { cat "$T/doctor-after"; }
m6_flush() { echo '{"ok":true,"data":{"results":[]}}'; }
m6_cursor() { echo "{\"last_uuid\":\"$(cat "$T/sid")-r3\"}"; }
`

// offlineFixture is what the offline script leaves in ~/m6-offline when
// all went as it should; each case breaks one file.
func offlineFixture(t *testing.T, dir string) {
	t.Helper()
	f := filepath.Join(dir, "fixture")
	if err := os.MkdirAll(filepath.Join(f, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	var hooks []string
	hooks = append(hooks, `{"name":"start1","event":"SessionStart","ms":40,"timeout_s":10,"rc":0}`)
	for i := 1; i <= 10; i++ {
		hooks = append(hooks, fmt.Sprintf(`{"name":"p%d","event":"UserPromptSubmit","ms":25,"timeout_s":25,"rc":0}`, i))
	}
	hooks = append(hooks, `{"name":"end","event":"SessionEnd","ms":30,"timeout_s":1,"rc":0}`,
		`{"name":"start2","event":"SessionStart","ms":35,"timeout_s":10,"rc":0}`)
	files := map[string]string{
		"since":             "2026-10-29T04:00:00.000Z",
		"down-after":        "3",
		"down-rc":           "7",
		"done":              "41",
		"health-end":        "down",
		"backdated":         "2026-10-29T02:00:00Z",
		"hooks.jsonl":       strings.Join(hooks, "\n"),
		"reach-before.json": `{"last_ok":"2026-10-29T03:59:00Z","failing_since":"2026-10-29T04:00:05Z"}`,
		"out/start2":        `{"systemMessage":"⚠️ backsight · 🗂️ bs-m6-offline · memory unavailable (server unreachable)\n⚠️ memory server silent since 2026-10-29 02:00 UTC: notes and sessions wait on this PC · run ` + "`backsight doctor`" + `"}`,
		"doctor1.json":      `{"data":{"checks":[{"id":"server-reach","status":"info","summary":"last answer 2026-10-29 03:59 UTC; breaker closed; 2 note(s) queued; 1 session(s) waiting"}]}}`,
		"doctor2.json":      `{"data":{"checks":[{"id":"server-reach","status":"warn","summary":"last answer 2026-10-29 02:00 UTC; breaker open; 2 note(s) queued; 1 session(s) waiting"}]}}`,
		"store1.json":       `{"ok":true,"data":{"status":"queued","document":"note:1"}}`,
		"store2.json":       `{"ok":true,"data":{"status":"queued","document":"note:2"}}`,
		"tick1.json":        `{"ok":false,"data":{"status":"partial","ms":25,"sessions":{"retry":1}}}`,
		"tick-base.json":    `{"ok":false,"data":{"status":"partial","ms":30}}`,
		"tick-unsent.json":  `{"ok":false,"data":{"status":"partial","ms":2600}}`,
		"reach-base.json":   `{"sessions_waiting":1}`,
		"reach-unsent.json": `{"sessions_waiting":201}`,
		"unsent-files":      "200",
	}
	for name, body := range files {
		writeLines(t, filepath.Join(f, name), body)
	}
	var tel []string
	for i := 1; i <= 10; i++ {
		tel = append(tel, fmt.Sprintf(`{"ts":"2026-10-29T04:00:%02d.000Z","event":"prompt","session":"of1","n":%d,"decision":"skip"}`, i, i))
	}
	writeLines(t, filepath.Join(dir, "telemetry"), tel...)
	writeLines(t, filepath.Join(dir, "sid"), "of1")
	writeLines(t, filepath.Join(dir, "doctor-after"), `{"data":{"checks":[{"id":"server-id","status":"ok"},{"id":"server-reach","status":"info","summary":"last answer 2026-10-29 04:07 UTC; breaker closed; 0 note(s) queued; 0 session(s) waiting"}]}}`)
}

// hungFixture is what the offline script leaves in ~/m6-offline-hung when
// the drop held: three recalls ran into it (17 s each), then the breaker.
func hungFixture(t *testing.T, dir string) {
	t.Helper()
	f := filepath.Join(dir, "fixture-hung")
	if err := os.MkdirAll(filepath.Join(f, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	hooks := []string{`{"name":"start1","event":"SessionStart","ms":7900,"timeout_s":10,"rc":0}`}
	var tel []string
	for i := 1; i <= 10; i++ {
		ms, d := 30, `"decision":"skip","reason":"breaker"`
		if i <= 3 {
			ms, d = 17500, `"decision":"recall","reason":"topic_shift","scopes":{"project":{"ok":false,"ms":17000,"raw":0},"global":{"ok":false,"ms":17000,"raw":0}}`
		}
		hooks = append(hooks, fmt.Sprintf(`{"name":"p%d","event":"UserPromptSubmit","ms":%d,"timeout_s":25,"rc":0}`, i, ms))
		tel = append(tel, fmt.Sprintf(`{"ts":"2026-10-29T05:00:%02d.000Z","event":"prompt","session":"oh1","n":%d,%s}`, i, i, d))
	}
	hooks = append(hooks, `{"name":"end","event":"SessionEnd","ms":400,"timeout_s":1,"rc":0}`)
	files := map[string]string{
		"since":            "2026-10-29T05:00:00.000Z",
		"down-after":       "1",
		"down-rc":          "28",
		"done":             "95",
		"health-end":       "down",
		"waited":           "0",
		"hooks.jsonl":      strings.Join(hooks, "\n"),
		"store1.json":      `{"ok":true,"data":{"status":"queued","document":"note:h1"}}`,
		"store2.json":      `{"ok":true,"data":{"status":"queued","document":"note:h2"}}`,
		"tick1.json":       `{"ok":false,"data":{"status":"partial","ms":31000,"sessions":{"retry":1}}}`,
		"breaker-end.json": `{"failures":0,"open_until":"2026-10-29T05:06:00Z"}`,
		"steps.jsonl":      `{"name":"store1","ms":6000,"rc":0,"bound_s":60}` + "\n" + `{"name":"tick1","ms":31000,"rc":40,"bound_s":300}`,
	}
	for name, body := range files {
		writeLines(t, filepath.Join(f, name), body)
	}
	writeLines(t, filepath.Join(dir, "telemetry"), tel...)
	writeLines(t, filepath.Join(dir, "sid"), "oh1")
	writeLines(t, filepath.Join(dir, "doctor-after"), `{"data":{"checks":[{"id":"server-id","status":"ok"},{"id":"server-reach","status":"info","summary":"last answer 2026-10-29 05:09 UTC; breaker closed; 0 note(s) queued; 0 session(s) waiting"}]}}`)
}

// editFixture replaces old by new in a fixture file (it must be there).
func editFixture(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s holds no %q", path, old)
	}
	writeLines(t, path, strings.TrimSpace(strings.Replace(string(b), old, new, 1)))
}

// linkVerbs are the up/down verbs of A's link, in order.
func linkVerbs(virsh []string) []string {
	var links []string
	for _, v := range virsh {
		if strings.HasPrefix(v, "domif-setlink bs-m6-client ") {
			links = append(links, strings.Fields(v)[3])
		}
	}
	return links
}

// cleanedA: the last call to A stopped the offline script's session and
// removed what the checks leave there.
func cleanedA(t *testing.T, dir, what string) {
	t.Helper()
	calls, _ := os.ReadFile(filepath.Join(dir, "m6sh.calls"))
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	last := lines[len(lines)-1]
	for _, w := range []string{"pkill -TERM -s", what, "-bs-m6-bs-m6-offline", "-bs-m6-bs-m6-unsent", "reach.json.bs-m6"} {
		if !strings.Contains(last, w) {
			t.Errorf("the last call to A does not hold %q: %s", w, last)
		}
	}
}

// runNetCheck runs the named check through m6_check, as vb_run does, with
// the fakes dir first on PATH; it returns the check's result record and
// the virsh verbs.
func runNetCheck(t *testing.T, fakes, dir, check string) (map[string]any, []string, string) {
	t.Helper()
	script := `. "$(dirname "$1")/m6-proofs.sh"` + "\n" + netDriver + "\n" +
		fmt.Sprintf(`m6_check %s m6_check_%s < /dev/null`, check, strings.ReplaceAll(check, "-", "_"))
	_, stderr, code := vmBench(t, fakes, t.TempDir(), filepath.Join(dir, "bs-m6"), script, "T="+dir)
	if code != 0 {
		t.Fatalf("%s: exit %d, stderr:\n%s", check, code, stderr)
	}
	b, err := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		t.Fatalf("%s: no result (stderr %s)", check, stderr)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("%s: result %q: %v", check, b, err)
	}
	return rec, virshVerbs(t, benchCalls(t, fakes, "virsh")), stderr
}

// Every way out of offline-laptop brings A's link back up, stops the
// offline script's session and leaves nothing of it on A; each reader's
// control turns a blind read into a FAIL.
func TestVMBenchOfflineBringsTheLinkBack(t *testing.T) {
	cases := []struct {
		name, status, detail string
		breakIt              func(t *testing.T, dir, fakes string)
	}{
		{"all well", "PASS", "link down 0 s, the offline script done in 41 s; 13 hooks within their deadlines (slowest ms: SessionEnd 30, SessionStart 40, UserPromptSubmit 25), no hook timeout", nil},
		{"the link came back too early", "FAIL", "the link came back too early", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "fixture", "health-end"), "up")
		}},
		{"a hook past its deadline", "FAIL", "past its deadline: end (SessionEnd) 1000 ms", func(t *testing.T, dir, _ string) {
			editFixture(t, filepath.Join(dir, "fixture", "hooks.jsonl"), `"name":"end","event":"SessionEnd","ms":30`, `"name":"end","event":"SessionEnd","ms":1000`)
		}},
		{"a hook killed past its deadline", "FAIL", "past its deadline: p4 (UserPromptSubmit) 27004 ms", func(t *testing.T, dir, _ string) {
			editFixture(t, filepath.Join(dir, "fixture", "hooks.jsonl"), `{"name":"p4","event":"UserPromptSubmit","ms":25,"timeout_s":25,"rc":0}`, `{"name":"p4","event":"UserPromptSubmit","ms":27004,"timeout_s":25,"rc":124}`)
		}},
		{"a hook that failed in time", "FAIL", "a hook did not exit 0: p2 (UserPromptSubmit) rc 1", func(t *testing.T, dir, _ string) {
			editFixture(t, filepath.Join(dir, "fixture", "hooks.jsonl"), `{"name":"p2","event":"UserPromptSubmit","ms":25,"timeout_s":25,"rc":0}`, `{"name":"p2","event":"UserPromptSubmit","ms":25,"timeout_s":25,"rc":1}`)
		}},
		{"a hook timeout in telemetry", "FAIL", "hook_failure timeout", func(t *testing.T, dir, _ string) {
			b, _ := os.ReadFile(filepath.Join(dir, "telemetry"))
			writeLines(t, filepath.Join(dir, "telemetry"), strings.TrimSpace(string(b)), `{"ts":"2026-10-29T04:01:00.000Z","event":"hook_failure","hook":"user-prompt-submit","kind":"timeout"}`)
		}},
		{"telemetry read elsewhere", "FAIL", "would prove nothing", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "telemetry"), `{"ts":"2026-10-29T04:00:01.000Z","event":"prompt","session":"other","n":1}`)
		}},
		{"no banner", "FAIL", "memory server silent since 2026-10-29 02:00 UTC", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "fixture", "out", "start2"), `{"systemMessage":"⚠️ backsight · memory unavailable (server unreachable)"}`)
		}},
		{"doctor not warning", "FAIL", "want warn naming 2 queued notes", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "fixture", "doctor2.json"), `{"data":{"checks":[{"id":"server-reach","status":"info","summary":"2 note(s) queued"}]}}`)
		}},
		{"no failure recorded", "FAIL", "reach.json recorded no failure", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "fixture", "reach-before.json"), `{"last_ok":"2026-10-29T03:59:00Z"}`)
		}},
		{"the script's own error", "FAIL", "the offline script on A: hooks.json (/h) wires no SessionEnd hook", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "fixture", "error"), "hooks.json (/h) wires no SessionEnd hook for ''")
		}},
		{"/health never failed", "FAIL", "the offline script never saw /health fail", func(t *testing.T, dir, _ string) {
			if err := os.Remove(filepath.Join(dir, "fixture", "down-after")); err != nil {
				t.Fatal(err)
			}
		}},
		{"the script never ends", "FAIL", "did not end within 2 min of the link coming back", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "never-done"), "x")
		}},
		{"the tick fails once back", "FAIL", "jobs tick on a did not succeed", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "tick-fails"), "x")
		}},
		{"server-reach still silent once back", "FAIL", "server-reach on A after the link came back", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "doctor-after"), `{"data":{"checks":[{"id":"server-id","status":"ok"},{"id":"server-reach","status":"info","summary":"last answer now; it was silent from 2026-10-29 02:00 UTC; breaker closed; 0 note(s) queued; 0 session(s) waiting"}]}}`)
		}},
		// Dying while the link is down: the trap brings it back.
		{"the link refuses to come back once", "FAIL", "cannot set A's link up", func(t *testing.T, _, fakes string) {
			writeLines(t, filepath.Join(fakes, "fail-up-once"), "x")
		}},
		{"killed while the link is down", "FAIL", "exited 143", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "kill-in-sleep"), "x")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			fakes := newBenchFakes(t)
			// A virsh whose first link-up fails when <fakes>/fail-up-once exists.
			writeExec(t, filepath.Join(fakes, "virsh"), strings.Replace(benchFakes["virsh"], "[ \"$1\" = -c ] && shift 2\n",
				"[ \"$1\" = -c ] && shift 2\ncase \"$*\" in \"domif-setlink \"*\" up\") if [ -e \"$d/fail-up-once\" ]; then rm -f \"$d/fail-up-once\"; exit 1; fi ;; esac\n", 1))
			offlineFixture(t, dir)
			if c.breakIt != nil {
				c.breakIt(t, dir, fakes)
			}
			rec, virsh, stderr := runNetCheck(t, fakes, dir, "offline-laptop")
			if rec["status"] != c.status || !strings.Contains(rec["detail"].(string), c.detail) {
				t.Errorf("%s %q, want %s naming %q (stderr %s)", rec["status"], rec["detail"], c.status, c.detail, stderr)
			}
			links := linkVerbs(virsh)
			if len(links) < 2 || links[0] != "down" || links[len(links)-1] != "up" {
				t.Errorf("A's link: %q, want down first and up last", links)
			}
			// The refused up is the check's own; the trap's up follows it.
			if c.name == "the link refuses to come back once" && !slices.Equal(links, []string{"down", "up", "up"}) {
				t.Errorf("A's link: %q, want down, the refused up, then the trap's up", links)
			}
			cleanedA(t, dir, "m6-offline")
			// The timers check judges a tick after the link came back: a
			// tick while A was cut off exits 40, rightly.
			if b, err := os.ReadFile(filepath.Join(dir, "work", "timers-after")); err != nil || strings.TrimSpace(string(b)) != "1761700000" {
				t.Errorf("timers-after: %q %v, want A's time once the link is back", b, err)
			}
		})
	}
}

// offline-hung: A's link stays up and a DROP rule blackholes the server;
// the hooks must hold their deadlines against a server that never
// answers. The rule is gone however the check ends; the controls fail a
// drop that never applied, a server that refused (not hung), and a run
// where no prompt ever tried the server.
func TestVMBenchOfflineHungDropsAndLiftsTheRule(t *testing.T) {
	cases := []struct {
		name, status, detail string
		breakIt              func(t *testing.T, dir, fakes string)
	}{
		{"all well", "PASS", "3 prompts tried the server and failed before the breaker opened (from prompt 4, open until 2026-10-29T05:06:00Z)", nil},
		{"the drop never applied", "FAIL", "the drop rule is not on A", func(t *testing.T, _, fakes string) {
			writeLines(t, filepath.Join(fakes, "ignore-drop"), "x")
		}},
		{"refused, not hung", "FAIL", "curl exit 7, not a timeout", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "fixture-hung", "down-rc"), "7")
		}},
		{"/health through at the end", "FAIL", "the drop let /health through at the offline script's end", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "fixture-hung", "health-end"), "up")
		}},
		{"no prompt tried the server", "FAIL", "no prompt of oh1 tried the server", func(t *testing.T, dir, _ string) {
			var tel []string
			for i := 1; i <= 10; i++ {
				tel = append(tel, fmt.Sprintf(`{"ts":"2026-10-29T05:00:%02d.000Z","event":"prompt","session":"oh1","n":%d,"decision":"skip","reason":"breaker"}`, i, i))
			}
			writeLines(t, filepath.Join(dir, "telemetry"), tel...)
		}},
		{"a recall got through", "FAIL", "a recall of oh1 reached the server", func(t *testing.T, dir, _ string) {
			editFixture(t, filepath.Join(dir, "telemetry"), `"project":{"ok":false`, `"project":{"ok":true`)
		}},
		{"a hook past its deadline", "FAIL", "past its deadline: p2 (UserPromptSubmit) 25000 ms", func(t *testing.T, dir, _ string) {
			editFixture(t, filepath.Join(dir, "fixture-hung", "hooks.jsonl"), `{"name":"p2","event":"UserPromptSubmit","ms":17500`, `{"name":"p2","event":"UserPromptSubmit","ms":25000`)
		}},
		{"a store not queued", "FAIL", "store 2 with the server hung", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "fixture-hung", "store2.json"), ``)
		}},
		{"the script never ends", "FAIL", "did not end within", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "never-done"), "x")
		}},
		{"the tick fails once lifted", "FAIL", "jobs tick on a did not succeed", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "tick-fails"), "x")
		}},
		// Dying while the rule is set (waiting for the script): the trap
		// lifts it.
		{"killed while the rule is set", "FAIL", "exited 143", func(t *testing.T, dir, _ string) {
			writeLines(t, filepath.Join(dir, "never-done"), "x")
			writeLines(t, filepath.Join(dir, "kill-in-sleep"), "x")
		}},
		{"the drop rule stayed", "FAIL", "the drop rule stayed on A", func(t *testing.T, _, fakes string) {
			writeLines(t, filepath.Join(fakes, "sticky-drop"), "x")
		}},
		// The breaker skipped prompt 2 while prompt 3 still tried the
		// server: the tries did not all come before it opened.
		{"a try after the breaker opened", "FAIL", "the breaker skipped prompt 2 before prompt 3 tried the server", func(t *testing.T, dir, _ string) {
			editFixture(t, filepath.Join(dir, "telemetry"), `"n":2,"decision":"recall","reason":"topic_shift","scopes":{"project":{"ok":false,"ms":17000,"raw":0},"global":{"ok":false,"ms":17000,"raw":0}}`, `"n":2,"decision":"skip","reason":"breaker"`)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			fakes := newBenchFakes(t)
			writeExec(t, filepath.Join(fakes, "ssh"), netSSH)
			hungFixture(t, dir)
			if c.breakIt != nil {
				c.breakIt(t, dir, fakes)
			}
			rec, virsh, stderr := runNetCheck(t, fakes, dir, "offline-hung")
			if rec["status"] != c.status || !strings.Contains(rec["detail"].(string), c.detail) {
				t.Errorf("%s %q, want %s naming %q (stderr %s)", rec["status"], rec["detail"], c.status, c.detail, stderr)
			}
			if _, err := os.Stat(filepath.Join(fakes, "drop")); err == nil && c.name != "the drop rule stayed" {
				t.Errorf("the drop rule stayed on A")
			}
			calls := strings.Join(benchCalls(t, fakes, "ssh"), "\n")
			if c.name == "killed while the rule is set" && !strings.Contains(calls, "s/^-A /-D /") {
				t.Errorf("the rule was never lifted after the kill: %s", calls)
			}
			if c.name != "the drop never applied" && !strings.Contains(calls, "sudo iptables -I OUTPUT -d 192.168.122.196 -m comment --comment bs-m6-hung -j DROP") {
				t.Errorf("no drop rule set: %s", calls)
			}
			if l := linkVerbs(virsh); slices.Contains(l, "down") {
				t.Errorf("offline-hung cut A's link: %q", l)
			}
			cleanedA(t, dir, "m6-offline-hung")
			if c.status == "PASS" {
				v := rec["values"].(map[string]any)
				if v["recall_attempts"] != 3.0 || v["breaker_from_prompt"] != 4.0 || v["breaker_open_until"] != "2026-10-29T05:06:00Z" || v["slowest_ms"].(map[string]any)["UserPromptSubmit"] != 17500.0 {
					t.Errorf("values %v", v)
				}
			}
		})
	}
}

// unsent-cost records both ticks, their difference, BenchmarkUnsent and
// a follow-up above 2 000 ms only; a tick that did not count the 200
// transcripts measured nothing and fails.
func TestVMBenchUnsentCostIsRecordedWithItsControl(t *testing.T) {
	for _, c := range []struct {
		name, waiting, unsent, status, detail string
		diff                                  float64
		follow                                bool
	}{
		{"counted, slow", `{"sessions_waiting":201}`, "2600", "RECORDED", "+2570 ms", 2570, true},
		{"counted, at the threshold", `{"sessions_waiting":201}`, "2030", "RECORDED", "+2000 ms", 2000, false},
		{"not counted", `{"sessions_waiting":1}`, "2600", "FAIL", "did not survey them", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			fakes := newBenchFakes(t)
			offlineFixture(t, dir)
			writeLines(t, filepath.Join(dir, "fixture", "reach-unsent.json"), c.waiting)
			writeLines(t, filepath.Join(dir, "fixture", "tick-unsent.json"), `{"ok":false,"data":{"status":"partial","ms":`+c.unsent+`}}`)
			if err := os.MkdirAll(filepath.Join(dir, "root"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeExec(t, filepath.Join(fakes, "go"), "#!/bin/sh\necho \"$*\" >> \"$(dirname \"$0\")/go.calls\"\nprintf 'BenchmarkUnsent-24 \\t 3\\t  48244988 ns/op\\nPASS\\n'\n")
			rec, virsh, stderr := runNetCheck(t, fakes, dir, "unsent-cost")
			if rec["status"] != c.status || !strings.Contains(rec["detail"].(string), c.detail) {
				t.Fatalf("%s %q, want %s naming %q (stderr %s)", rec["status"], rec["detail"], c.status, c.detail, stderr)
			}
			if l := linkVerbs(virsh); len(l) == 0 || l[len(l)-1] != "up" {
				t.Errorf("A's link not brought back: %q", l)
			}
			if c.status != "RECORDED" {
				return
			}
			v := rec["values"].(map[string]any)
			if v["tick_ms"] != 30.0 || v["diff_ms"] != c.diff || v["benchmark_ns_op"] != 48244988.0 || v["follow_up"] != c.follow {
				t.Errorf("values %v", v)
			}
			if named := strings.Contains(rec["detail"].(string), "block 5 follow-up"); named != c.follow {
				t.Errorf("follow-up named %v, want %v: %q", named, c.follow, rec["detail"])
			}
			if calls := benchCalls(t, fakes, "go"); len(calls) != 1 || !strings.Contains(calls[0], "-bench ^BenchmarkUnsent$") {
				t.Errorf("go calls %q", calls)
			}
		})
	}
}

// netSSH is the bench's ssh to bs-m6-client as root's sudo sees it: a
// netem qdisc kept in <fakes>/netem (none unless <fakes>/ignore-replace),
// the drop rule in <fakes>/drop (none unless <fakes>/ignore-drop).
const netSSH = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/ssh.calls"
case "$4" in
  *"qdisc replace"*) [ -e "$d/ignore-replace" ] || echo "qdisc netem 8001: dev enp1s0 root refcnt 2 limit 1000 delay $(printf '%s' "$4" | sed -n 's/.*delay \([0-9]*ms\).*/\1/p')" > "$d/netem" ;;
  *"qdisc del"*) rm -f "$d/netem" ;;
  *"qdisc show"*) cat "$d/netem" 2> /dev/null || echo "qdisc fq_codel 0: dev enp1s0 root refcnt 2" ;;
  *"iptables -I OUTPUT"*) [ -e "$d/ignore-drop" ] || echo "-A OUTPUT -d 192.168.122.196/32 -m comment --comment bs-m6-hung -j DROP" > "$d/drop" ;;
  *'s/^-A /-D /'*) if [ -e "$d/sticky-drop" ]; then exit 1; fi; rm -f "$d/drop" ;;
  *"iptables -S OUTPUT"*) echo "-P OUTPUT ACCEPT"; cat "$d/drop" 2> /dev/null ;;
esac
exit 0
`

// latencyOut is the latency script's answer: a startup and 40 prompts,
// prompt i taking ms+i on the wall and 500+i in telemetry when recall(i),
// else outlier times (24 000 ms on the wall, 1 ms in telemetry) that the
// percentiles must leave out.
func latencyOut(t *testing.T, dir string, ms int, recall func(i int) bool, failures string) {
	t.Helper()
	hooks := []string{`{"name":"start","event":"SessionStart","ms":50,"timeout_s":10,"rc":0}`}
	var prompts []string
	for i := 1; i <= 40; i++ {
		wall, tms, d := ms+i, 500+i, "recall"
		if !recall(i) {
			wall, tms, d = 24000, 1, "skip"
		}
		hooks = append(hooks, fmt.Sprintf(`{"name":"p%d","event":"UserPromptSubmit","ms":%d,"timeout_s":25,"rc":0}`, i, wall))
		prompts = append(prompts, fmt.Sprintf(`{"n":%d,"decision":%q,"reason":"topic_shift","ms":%d}`, i, d, tms))
	}
	writeLines(t, filepath.Join(dir, "latency.json"), fmt.Sprintf(`{"waited_s":0,"hooks":[%s],"prompts":[%s],"failures":%s}`,
		strings.Join(hooks, ","), strings.Join(prompts, ","), failures))
}

// latency-<d>ms: netem is set as asked and gone however the check ends,
// with the latency files on A; the RTT control fails a delay that did not
// apply, too few recalls fail as nothing measured, a hook past its
// deadline or timing out fails, and the percentiles are of the recall
// prompts alone.
func TestVMBenchLatencyLeavesNoNetem(t *testing.T) {
	all := func(int) bool { return true }
	first := func(n int) func(int) bool { return func(i int) bool { return i <= n } }
	for _, c := range []struct {
		name, check, ping, status, detail string
		ms                                int
		recall                            func(int) bool
		failures                          string
		ignoreReplace                     bool
	}{
		{"20 ms", "latency-20ms", "time=20.4 ms\ntime=20.6 ms\ntime=20.5 ms", "PASS", "40 recall prompts of 40: wall p50 620 p95 638 ms, prompt.ms p50 520 p95 538 ms", 600, all, "[]", false},
		// 27 recalls (every i but the multiples of 3), skip prompts slow on
		// the wall and fast in telemetry: p50 is the 14th (i = 20), p95 the
		// 26th (i = 38).
		{"recall prompts only", "latency-20ms", "time=20.4 ms", "PASS", "27 recall prompts of 40: wall p50 620 p95 638 ms, prompt.ms p50 520 p95 538 ms", 600, func(i int) bool { return i%3 != 0 }, "[]", false},
		{"no delay", "latency-0ms", "time=0.4 ms", "PASS", "netem 0 ms (median RTT 0.4 ms)", 600, all, "[]", false},
		{"netem not applied", "latency-80ms", "time=0.5 ms", "FAIL", "netem delay 80ms is not on A's enp1s0", 600, all, "[]", true},
		{"packets not delayed", "latency-80ms", "time=0.5 ms\ntime=0.6 ms\ntime=79.0 ms", "FAIL", "median RTT to the server 0.6 ms, below the 80 ms netem delay", 600, all, "[]", false},
		{"too few recalls", "latency-20ms", "time=20.4 ms", "FAIL", "19 of 40 prompts recalled", 600, first(19), "[]", false},
		{"a hook past its deadline", "latency-80ms", "time=80.4 ms", "FAIL", "past its deadline: p40 (UserPromptSubmit) 25000 ms", 24960, all, "[]", false},
		{"a hook timeout", "latency-20ms", "time=20.4 ms", "FAIL", "hook timeouts in A's telemetry", 600, all, `[{"event":"hook_failure","kind":"timeout"}]`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			fakes := newBenchFakes(t)
			writeExec(t, filepath.Join(fakes, "ssh"), netSSH)
			if c.ignoreReplace {
				writeLines(t, filepath.Join(fakes, "ignore-replace"), "x")
			}
			writeLines(t, filepath.Join(dir, "ping"), strings.Split(c.ping, "\n")...)
			latencyOut(t, dir, c.ms, c.recall, c.failures)
			rec, _, stderr := runNetCheck(t, fakes, dir, c.check)
			if rec["status"] != c.status || !strings.Contains(rec["detail"].(string), c.detail) {
				t.Errorf("%s %q, want %s naming %q (stderr %s)", rec["status"], rec["detail"], c.status, c.detail, stderr)
			}
			if _, err := os.Stat(filepath.Join(fakes, "netem")); err == nil {
				t.Errorf("netem stayed on A after the check")
			}
			calls := strings.Join(benchCalls(t, fakes, "ssh"), "\n")
			want := strings.TrimSuffix(strings.TrimPrefix(c.check, "latency-"), "ms")
			if replaced := strings.Contains(calls, "sudo tc qdisc replace dev enp1s0 root netem delay "+want+"ms"); replaced != (want != "0") {
				t.Errorf("netem delay %sms set: %v, ssh calls:\n%s", want, replaced, calls)
			}
			a, _ := os.ReadFile(filepath.Join(dir, "m6sh.calls"))
			lines := strings.Split(strings.TrimSpace(string(a)), "\n")
			last := lines[len(lines)-1]
			if !strings.Contains(last, "rm -rf ~/m6-latency ~/m6-latency.sh") {
				t.Errorf("the latency files stayed on A: last call %q", last)
			}
			if c.name == "20 ms" {
				stopsTheLatencySession(t, strings.TrimPrefix(last, "a "))
			}
		})
	}
}

// stopsTheLatencySession runs the latency check's cleanup on A (cmd) in a
// home whose ~/m6-latency/pgid names a session still running (a script
// that outlived its ssh): the session must be stopped.
func stopsTheLatencySession(t *testing.T, cmd string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "m6-latency"), 0o700); err != nil {
		t.Fatal(err)
	}
	left := exec.Command("setsid", "sleep", "60")
	if err := left.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = left.Wait(); close(done) }()
	t.Cleanup(func() { _ = left.Process.Kill(); <-done })
	// Until setsid has made it a session leader, the cleanup's pkill -s
	// would find nothing.
	for deadline := time.Now().Add(5 * time.Second); !sessionLeader(left.Process.Pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is not a session leader after 5 s", left.Process.Pid)
		}
	}
	writeLines(t, filepath.Join(home, "m6-latency", "pgid"), strconv.Itoa(left.Process.Pid))
	run := exec.Command("sh", "-c", cmd)
	run.Env = append(os.Environ(), "HOME="+home)
	if out, err := run.CombinedOutput(); err != nil {
		t.Logf("cleanup: %v %s", err, out)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("the latency script's session still runs after the cleanup: %s", cmd)
	}
	if _, err := os.Stat(filepath.Join(home, "m6-latency")); err == nil {
		t.Error("~/m6-latency stayed")
	}
}

// sessionLeader says whether pid leads its own session: its session id,
// the fourth field after the command name in /proc/<pid>/stat, is pid.
func sessionLeader(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(raw)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	return len(f) > 3 && f[3] == strconv.Itoa(pid)
}

// The run's end removes a netem or a drop rule a check could not (a run
// stopped by hand).
func TestVMBenchRunEndRemovesNetemAndTheDrop(t *testing.T) {
	fakes := newBenchFakes(t)
	writeExec(t, filepath.Join(fakes, "ssh"), netSSH)
	writeLines(t, filepath.Join(fakes, "netem"), "qdisc netem 8001: dev enp1s0 root refcnt 2 limit 1000 delay 80ms")
	writeLines(t, filepath.Join(fakes, "drop"), "-A OUTPUT -d 192.168.122.196/32 -m comment --comment bs-m6-hung -j DROP")
	bench := filepath.Join(t.TempDir(), "bs-m6")
	if err := os.MkdirAll(filepath.Join(bench, "client-b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := vmBench(t, fakes, t.TempDir(), bench, `vb_run_end`); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, f := range []string{"netem", "drop"} {
		if _, err := os.Stat(filepath.Join(fakes, f)); err == nil {
			t.Errorf("the run's end left the %s on A: ssh calls %q", f, benchCalls(t, fakes, "ssh"))
		}
	}
}
