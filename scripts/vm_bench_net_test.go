package scripts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The checks of the client VM's network (offline-laptop, unsent-cost,
// latency-*) against fakes: A is a set of shell functions answering from
// files under T, the link and netem the fake virsh and ssh. What they
// prove: every way out restores the network, and each positive control
// fails a reader that could not see.

// netDriver runs after vm-bench.sh and m6-proofs.sh are sourced: A's side
// of the driver, from files of T. m6_sh logs each command to T/m6sh.calls.
const netDriver = `
M6_RESULTS=$T/results.jsonl M6_WORK=$T/work M6_ROOT=$T/root M6_SERVER_IP=192.168.122.196 M6_OFFLINE_S=0
mkdir -p "$M6_WORK"
m6_sh() {
  printf '%s\n' "$1 $2" >> "$T/m6sh.calls"
  case $2 in
    *"tar -C"*) tar -C "$T/fixture" -cf - . ;;
    "ip -o route get"*) echo "192.168.122.196 dev enp1s0 src 192.168.122.195 uid 1000" ;;
    "ping "*) cat "$T/ping" ;;
    "bash "*m6-latency*) cat "$T/latency.json" ;;
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
m6_uuid_counts() { printf 'of1-r%s 1\n' 1 2 3; }
m6_doc() { echo '{"id":"x"}'; }
m6_doctor() { cat "$T/doctor-after"; }
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
	writeLines(t, filepath.Join(dir, "doctor-after"), `{"data":{"checks":[{"id":"server-id","status":"ok"},{"id":"server-reach","status":"info","summary":"last answer 2026-10-29 04:07 UTC; breaker closed; 0 note(s) queued; 0 session(s) waiting"}]}}`)
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
// offline script and removes unsent-cost's transcripts; each reader's
// control turns a blind read into a FAIL.
func TestVMBenchOfflineBringsTheLinkBack(t *testing.T) {
	cases := []struct {
		name, status, detail string
		breakIt              func(t *testing.T, dir string)
	}{
		{"all well", "PASS", "link down 0 s, the offline script done in 41 s; 13 hooks within their deadlines (slowest ms: SessionEnd 30, SessionStart 40, UserPromptSubmit 25), no hook timeout", nil},
		{"the link came back too early", "FAIL", "the link came back too early", func(t *testing.T, dir string) {
			writeLines(t, filepath.Join(dir, "fixture", "health-end"), "up")
		}},
		{"a hook past its deadline", "FAIL", "past its deadline: end (SessionEnd) 1000 ms", func(t *testing.T, dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, "fixture", "hooks.jsonl"))
			s := strings.Replace(string(b), `"name":"end","event":"SessionEnd","ms":30`, `"name":"end","event":"SessionEnd","ms":1000`, 1)
			writeLines(t, filepath.Join(dir, "fixture", "hooks.jsonl"), strings.TrimSpace(s))
		}},
		{"a hook timeout in telemetry", "FAIL", "hook_failure timeout", func(t *testing.T, dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, "telemetry"))
			writeLines(t, filepath.Join(dir, "telemetry"), strings.TrimSpace(string(b)), `{"ts":"2026-10-29T04:01:00.000Z","event":"hook_failure","hook":"user-prompt-submit","kind":"timeout"}`)
		}},
		{"telemetry read elsewhere", "FAIL", "would prove nothing", func(t *testing.T, dir string) {
			writeLines(t, filepath.Join(dir, "telemetry"), `{"ts":"2026-10-29T04:00:01.000Z","event":"prompt","session":"other","n":1}`)
		}},
		{"no banner", "FAIL", "memory server silent since 2026-10-29 02:00 UTC", func(t *testing.T, dir string) {
			writeLines(t, filepath.Join(dir, "fixture", "out", "start2"), `{"systemMessage":"⚠️ backsight · memory unavailable (server unreachable)"}`)
		}},
		{"doctor not warning", "FAIL", "want warn naming 2 queued notes", func(t *testing.T, dir string) {
			writeLines(t, filepath.Join(dir, "fixture", "doctor2.json"), `{"data":{"checks":[{"id":"server-reach","status":"info","summary":"2 note(s) queued"}]}}`)
		}},
		{"no failure recorded", "FAIL", "reach.json recorded no failure", func(t *testing.T, dir string) {
			writeLines(t, filepath.Join(dir, "fixture", "reach-before.json"), `{"last_ok":"2026-10-29T03:59:00Z"}`)
		}},
		{"the tick fails once back", "FAIL", "jobs tick on a did not succeed", func(t *testing.T, dir string) {
			writeLines(t, filepath.Join(dir, "tick-fails"), "x")
		}},
		{"server-reach still silent once back", "FAIL", "server-reach on A after the link came back", func(t *testing.T, dir string) {
			writeLines(t, filepath.Join(dir, "doctor-after"), `{"data":{"checks":[{"id":"server-id","status":"ok"},{"id":"server-reach","status":"info","summary":"last answer now; it was silent from 2026-10-29 02:00 UTC; breaker closed; 0 note(s) queued; 0 session(s) waiting"}]}}`)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			fakes := newBenchFakes(t)
			offlineFixture(t, dir)
			if c.breakIt != nil {
				c.breakIt(t, dir)
			}
			rec, virsh, stderr := runNetCheck(t, fakes, dir, "offline-laptop")
			if rec["status"] != c.status || !strings.Contains(rec["detail"].(string), c.detail) {
				t.Errorf("%s %q, want %s naming %q (stderr %s)", rec["status"], rec["detail"], c.status, c.detail, stderr)
			}
			var links []string
			for _, v := range virsh {
				if strings.HasPrefix(v, "domif-setlink bs-m6-client ") {
					links = append(links, strings.Fields(v)[3])
				}
			}
			if len(links) < 2 || links[0] != "down" || links[len(links)-1] != "up" {
				t.Errorf("A's link: %q, want down first and up last", links)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "m6sh.calls"))
			last := strings.LastIndex(string(calls), "a pkill -f \"m6-offline/run.sh\"")
			if last < 0 || !strings.Contains(string(calls)[last:], "-bs-m6-bs-m6-unsent") {
				t.Errorf("the offline script was not stopped nor its transcripts removed at the end:\n%s", calls)
			}
		})
	}
}

// unsent-cost records both ticks, their difference, BenchmarkUnsent and
// a follow-up above 2 000 ms; a tick that did not count the 200
// transcripts measured nothing and fails.
func TestVMBenchUnsentCostIsRecordedWithItsControl(t *testing.T) {
	for _, c := range []struct {
		name, waiting, status, detail string
		follow                        bool
	}{
		{"counted", `{"sessions_waiting":201}`, "RECORDED", "+2570 ms", true},
		{"not counted", `{"sessions_waiting":1}`, "FAIL", "did not survey them", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			fakes := newBenchFakes(t)
			offlineFixture(t, dir)
			writeLines(t, filepath.Join(dir, "fixture", "reach-unsent.json"), c.waiting)
			if err := os.MkdirAll(filepath.Join(dir, "root"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeExec(t, filepath.Join(fakes, "go"), "#!/bin/sh\necho \"$*\" >> \"$(dirname \"$0\")/go.calls\"\nprintf 'BenchmarkUnsent-24 \\t 3\\t  48244988 ns/op\\nPASS\\n'\n")
			rec, virsh, stderr := runNetCheck(t, fakes, dir, "unsent-cost")
			if rec["status"] != c.status || !strings.Contains(rec["detail"].(string), c.detail) {
				t.Fatalf("%s %q, want %s naming %q (stderr %s)", rec["status"], rec["detail"], c.status, c.detail, stderr)
			}
			if !strings.Contains(strings.Join(virsh, "\n"), "domif-setlink bs-m6-client 52:54:00:b6:06:02 up") {
				t.Errorf("A's link not brought back: %q", virsh)
			}
			if c.status != "RECORDED" {
				return
			}
			v := rec["values"].(map[string]any)
			if v["tick_ms"] != 30.0 || v["tick_unsent_ms"] != 2600.0 || v["diff_ms"] != 2570.0 || v["benchmark_ns_op"] != 48244988.0 || v["follow_up"] != c.follow {
				t.Errorf("values %v", v)
			}
			if !strings.Contains(rec["detail"].(string), "block 5 follow-up") {
				t.Errorf("no follow-up named above 2 000 ms: %q", rec["detail"])
			}
			if calls := benchCalls(t, fakes, "go"); len(calls) != 1 || !strings.Contains(calls[0], "-bench ^BenchmarkUnsent$") {
				t.Errorf("go calls %q", calls)
			}
		})
	}
}

// netSSH is the bench's ssh to bs-m6-client as root's sudo sees it: a
// netem qdisc kept in <fakes>/netem (none unless <fakes>/ignore-replace).
const netSSH = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/ssh.calls"
case "$4" in
  *"qdisc replace"*) [ -e "$d/ignore-replace" ] || echo "qdisc netem 8001: dev enp1s0 root refcnt 2 limit 1000 delay $(printf '%s' "$4" | sed -n 's/.*delay \([0-9]*ms\).*/\1/p')" > "$d/netem" ;;
  *"qdisc del"*) rm -f "$d/netem" ;;
  *"qdisc show"*) cat "$d/netem" 2> /dev/null || echo "qdisc fq_codel 0: dev enp1s0 root refcnt 2" ;;
esac
exit 0
`

// latencyOut is the latency script's answer: a startup and 40 prompts of
// ms each, recall on the first recalls of them.
func latencyOut(t *testing.T, dir string, ms, recalls int, failures string) {
	t.Helper()
	hooks := []string{`{"name":"start","event":"SessionStart","ms":50,"timeout_s":10,"rc":0}`}
	var prompts []string
	for i := 1; i <= 40; i++ {
		hooks = append(hooks, fmt.Sprintf(`{"name":"p%d","event":"UserPromptSubmit","ms":%d,"timeout_s":25,"rc":0}`, i, ms+i))
		d := "skip"
		if i <= recalls {
			d = "recall"
		}
		prompts = append(prompts, fmt.Sprintf(`{"n":%d,"decision":%q,"reason":"topic_shift","ms":%d}`, i, d, 500+i))
	}
	writeLines(t, filepath.Join(dir, "latency.json"), fmt.Sprintf(`{"waited_s":0,"hooks":[%s],"prompts":[%s],"failures":%s}`,
		strings.Join(hooks, ","), strings.Join(prompts, ","), failures))
}

// latency-<d>ms: netem is set as asked and gone however the check ends;
// the RTT control fails a delay that did not apply, too few recalls fail
// as nothing measured, and a hook past its deadline or timing out fails.
func TestVMBenchLatencyLeavesNoNetem(t *testing.T) {
	for _, c := range []struct {
		name, check, ping, status, detail string
		ms, recalls                       int
		failures                          string
		ignoreReplace                     bool
	}{
		{"20 ms", "latency-20ms", "time=20.4 ms\ntime=20.6 ms\ntime=20.5 ms", "PASS", "40 recall prompts of 40: wall p50 620 p95 638 ms, prompt.ms p50 520 p95 538 ms", 600, 40, "[]", false},
		{"no delay", "latency-0ms", "time=0.4 ms", "PASS", "netem 0 ms (median RTT 0.4 ms)", 600, 40, "[]", false},
		{"netem not applied", "latency-80ms", "time=0.5 ms", "FAIL", "netem delay 80ms is not on A's enp1s0", 600, 40, "[]", true},
		{"packets not delayed", "latency-80ms", "time=0.5 ms\ntime=0.6 ms\ntime=79.0 ms", "FAIL", "median RTT to the server 0.6 ms, below the 80 ms netem delay", 600, 40, "[]", false},
		{"too few recalls", "latency-20ms", "time=20.4 ms", "FAIL", "19 of 40 prompts recalled", 600, 19, "[]", false},
		{"a hook past its deadline", "latency-80ms", "time=80.4 ms", "FAIL", "past its deadline: p40 (UserPromptSubmit) 25000 ms", 24960, 40, "[]", false},
		{"a hook timeout", "latency-20ms", "time=20.4 ms", "FAIL", "hook timeouts in A's telemetry", 600, 40, `[{"event":"hook_failure","kind":"timeout"}]`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			fakes := newBenchFakes(t)
			writeExec(t, filepath.Join(fakes, "ssh"), netSSH)
			if c.ignoreReplace {
				writeLines(t, filepath.Join(fakes, "ignore-replace"), "x")
			}
			writeLines(t, filepath.Join(dir, "ping"), strings.Split(c.ping, "\n")...)
			latencyOut(t, dir, c.ms, c.recalls, c.failures)
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
		})
	}
}

// The run's end removes a netem a check could not (a run stopped by hand).
func TestVMBenchRunEndRemovesNetem(t *testing.T) {
	fakes := newBenchFakes(t)
	writeExec(t, filepath.Join(fakes, "ssh"), netSSH)
	writeLines(t, filepath.Join(fakes, "netem"), "qdisc netem 8001: dev enp1s0 root refcnt 2 limit 1000 delay 80ms")
	bench := filepath.Join(t.TempDir(), "bs-m6")
	if err := os.MkdirAll(filepath.Join(bench, "client-b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := vmBench(t, fakes, t.TempDir(), bench, `vb_run_end`); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(fakes, "netem")); err == nil {
		t.Errorf("the run's end left netem on A: ssh calls %q", benchCalls(t, fakes, "ssh"))
	}
}
