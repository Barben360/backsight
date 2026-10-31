package cli

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
)

// Free text of every source stays out of every view: the index fixture of
// TestIndexHoldsNoCanary, plus a server that holds a memory's text, an
// operation's error message and a project tag, and a doctor check whose
// summary is a canary. Every view, every id form, --json and text, --dev:
// no canary in stdout or stderr, except the one memory text that `--text`
// asks for.
func TestStatsViewsHoldNoCanary(t *testing.T) {
	home := canaryState(t)
	port := fakeHindsight(t)
	cfgPath := pathsOf(home).ConfigFile()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(raw, []byte("\n[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	const memText = "CANARY-MEMORY-TEXT"
	memories["mem-allowed-2"] = `{"id":"mem-allowed-2","text":"` + memText + `","type":"world","tags":["project:` + ctlProject + `"]}`
	projectEntities[ctlProject] = "[]"
	now := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	fakeOperations = []string{
		`{"id":"op1","operation_type":"retain","status":"failed","document_id":"doc1","created_at":"` + now + `","updated_at":"` + now + `","retry_count":2,"error_message":"CANARY-OPERROR"}`,
		`{"id":"op2","operation_type":"retain","status":"completed","document_id":"doc2","created_at":"` + now + `","updated_at":"` + now + `","retry_count":0,"error_message":"CANARY-OPERROR"}`,
	}
	oldDoctor := doctorRun
	doctorRun = func(context.Context, doctor.Env) doctor.Report {
		return doctor.Report{Checks: []doctor.Check{{ID: "server", Status: doctor.Warn, Summary: "CANARY-DOCTOR", Detail: "CANARY-DOCTOR", Fix: "backsight doctor"}}}
	}
	t.Cleanup(func() { doctorRun = oldDoctor })

	scan := func(what string, args ...string) string {
		t.Helper()
		code, out, errOut := run(t, args...)
		if code != output.OK {
			t.Errorf("%s: exit %d: %s %s", what, code, out, errOut)
		}
		clean := strings.ReplaceAll(out, memText, "")
		for _, c := range append(canaries, "CANARY-DOCTOR", "CANARY-OPERROR", "CANARY") {
			if strings.Contains(clean+errOut, c) {
				t.Errorf("%s: %q in the output of %v", what, c, args)
			}
		}
		return out
	}
	ids := map[string][]string{
		"sessions": {"s1"},
		"memories": {ctlMemory, "mem-allowed-2", "mem-allowed-3"},
		"projects": {ctlProject},
		"days":     {time.Now().UTC().Format("2006-01-02")},
		"defs":     {"recall.hit_rate"},
	}
	for _, view := range metrics.Views {
		for _, dev := range [][]string{nil, {"--dev"}} {
			for _, js := range [][]string{nil, {"--json"}} {
				args := append(append([]string{"stats", view}, dev...), js...)
				scan(view, args...)
				for _, id := range ids[view] {
					scan(view+" "+id, append(append([]string{"stats", view, id}, dev...), js...)...)
				}
			}
		}
	}
	scan("memories sorted", "stats", "memories", "--sort", "rate", "--state", "hidden", "--json")

	// the positive control: the one text the command is asked for
	out := scan("memory text", "stats", "memories", "mem-allowed-2", "--text", "--json")
	if !strings.Contains(out, memText) {
		t.Errorf("the memory text did not come back: %s", out)
	}
	if out := scan("no text asked", "stats", "memories", "mem-allowed-2", "--json"); strings.Contains(out, memText) {
		t.Error("the memory text came back without --text")
	}
	// the failed operation reached the view as counts, not as its message
	if v := runView(t, "stats", "pipeline"); v.code != output.OK || !strings.Contains(v.raw, `"retries": 2`) {
		t.Errorf("the failed operation did not reach the view: %s", v.raw)
	}
	// the doctor's check came as id, status and fix only
	v := runView(t, "stats", "system")
	if v.code != output.OK || !strings.Contains(v.raw, `"backsight doctor"`) {
		t.Errorf("the doctor check did not reach the view: %s", v.raw)
	}
}
