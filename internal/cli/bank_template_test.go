package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// olderTemplate puts on the fake server's bank what an earlier backsight
// sent (its template without the 0071 C5 `subagent` strategy) and records
// it as applied, as that binary's bank apply left it.
func olderTemplate(t *testing.T, home string) {
	t.Helper()
	old := server.Current()
	strategies := old.Config["retain_strategies"].(map[string]any)
	delete(strategies, "subagent")
	set := map[string]json.RawMessage{}
	for k, v := range old.Config {
		set[k], _ = json.Marshal(v)
	}
	bankOverrides("claude-memory", set)
	d := pathsOf(home)
	raw, _ := json.Marshal(old)
	if err := os.MkdirAll(d.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.BankTemplateFile(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.Update(d, "test", func(s *state.State) error { s.BankConfigHash = old.Hash(); return nil }); err != nil {
		t.Fatal(err)
	}
	if old.Hash() == server.TemplateHash() {
		t.Fatal("the older template hashes as this one")
	}
}

// hasSubagentStrategy: the bank holds the 0071 C5 strategy, with its rules.
func hasSubagentStrategy(t *testing.T) bool {
	t.Helper()
	var strategies map[string]map[string]any
	_ = json.Unmarshal(bankOverrides("claude-memory", nil)["retain_strategies"], &strategies)
	rules, _ := strategies["subagent"]["retain_custom_instructions"].(string)
	return strings.Contains(rules, "The source is the final report of a sub-agent.")
}

func recordedHash(t *testing.T, home string) string {
	t.Helper()
	st, err := state.Load(pathsOf(home).StateFile())
	if err != nil {
		t.Fatal(err)
	}
	return st.BankConfigHash
}

func installSteps(t *testing.T, out string) []installStep {
	t.Helper()
	var env struct {
		Data clientInstallResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return env.Data.Steps
}

func installStepNamed(steps []installStep, name string) *installStep {
	for i := range steps {
		if steps[i].Name == name {
			return &steps[i]
		}
	}
	return nil
}

// client install sends this binary's template when the recorded one is
// older, records it, and then leaves the bank alone.
func TestClientInstallBringsTheBankToItsTemplate(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClaude(t)
	olderTemplate(t, home)

	code, out, _ := run(t, "client", "install", "--plan", "--json")
	if s := installStepNamed(installSteps(t, out), "bank"); code != output.OK || s == nil || s.Status != "planned" || hasSubagentStrategy(t) {
		t.Fatalf("plan: exit %d %+v\n%s", code, s, out)
	}
	code, out, _ = run(t, "client", "install", "--json")
	if s := installStepNamed(installSteps(t, out), "bank"); code != output.OK || s == nil || s.Status != "done" {
		t.Fatalf("exit %d %+v\n%s", code, s, out)
	}
	if !hasSubagentStrategy(t) || recordedHash(t, home) != server.TemplateHash() {
		t.Errorf("subagent strategy %v, hash %s", hasSubagentStrategy(t), recordedHash(t, home))
	}
	if got, err := server.LoadApplied(pathsOf(home).BankTemplateFile()); err != nil || got.Hash() != server.TemplateHash() {
		t.Errorf("record %v %v", got.Hash(), err)
	}
	// Then nothing is sent.
	bankCalls = nil
	code, out, _ = run(t, "client", "install", "--json")
	if s := installStepNamed(installSteps(t, out), "bank"); code != output.OK || s == nil || s.Status != "unchanged" {
		t.Errorf("again: exit %d %+v", code, s)
	}
	if slices.ContainsFunc(bankCalls, func(c string) bool {
		return strings.HasPrefix(c, "PATCH claude-memory ") || strings.HasPrefix(c, "PUT claude-memory ")
	}) {
		t.Errorf("an unchanged template was sent again: %v", bankCalls)
	}
}

// A server that cannot take the template is a failed step, never a failed
// client install: the plugin is wired, and the hash stays as it was so the
// next run tries again.
func TestClientInstallGoesOnWhenTheBankRefuses(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClaude(t)
	olderTemplate(t, home)
	before := recordedHash(t, home)
	bankConfigStatus.Store(500)
	code, out, errOut := run(t, "client", "install", "--json")
	s := installStepNamed(installSteps(t, out), "bank")
	if code != output.OK || s == nil || s.Status != "failed" || !strings.Contains(s.Detail, "backsight bank apply") {
		t.Fatalf("exit %d %+v\n%s%s", code, s, out, errOut)
	}
	if recordedHash(t, home) != before {
		t.Error("a template the server refused was recorded")
	}
	if p := installStepNamed(installSteps(t, out), "permission rule"); p == nil || p.Status != "done" {
		t.Errorf("the wiring did not finish: %+v", installSteps(t, out))
	}
}

// update apply brings the 0071 C5 `subagent` strategy to a bank an older
// backsight configured: its client install leaves the bank to the step
// after the commit point, which runs the new binary's bank apply.
func TestUpdateApplySendsTheSubagentStrategy(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	pointAt(t, home, fakeHindsight(t))
	configure(t, home, "[backup]\nenabled = false")
	olderTemplate(t, home)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})

	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	if env := decodeApply(t, out); code != output.OK || !hasApplyStep(env.Data.Steps, "bank", "planned", "bank apply") || hasSubagentStrategy(t) {
		t.Fatalf("plan: exit %d\n%s", code, out)
	}
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || bins.ran("0.2.0 bank apply") != 1 || !hasApplyStep(env.Data.Steps, "bank", "done", "configured") {
		t.Fatalf("exit %d calls %q\n%s%s", code, bins.calls, out, errOut)
	}
	if !hasSubagentStrategy(t) || recordedHash(t, home) != server.TemplateHash() {
		t.Errorf("after the update: subagent strategy %v, hash %s", hasSubagentStrategy(t), recordedHash(t, home))
	}
	if i, j := applyStepIndex(env.Data.Steps, "server"), applyStepIndex(env.Data.Steps, "bank"); i < 0 || j < i {
		t.Errorf("bank step before the server step: %+v", env.Data.Steps)
	}
	// The new binary's client install, under the update's lock, left it.
	if bins.ran("0.2.0 client install") != 1 {
		t.Errorf("calls %q", bins.calls)
	}
}

// The bank step's failure after the commit point is a warning: the update
// stays.
func TestUpdateApplyGoesOnWhenTheBankRefuses(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	pointAt(t, home, fakeHindsight(t))
	configure(t, home, "[backup]\nenabled = false")
	olderTemplate(t, home)
	bankConfigStatus.Store(500)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	d := pathsOf(home)
	if code != output.OK || env.Data.RolledBack || !strings.HasPrefix(shimOf(t, d), filepath.Join(d.Data, "bin", "0.2.0-")) {
		t.Fatalf("a bank failure must not undo the update: exit %d\n%s", code, out)
	}
	if bins.ran("0.2.0 bank apply") != 1 || !hasApplyStep(env.Data.Steps, "bank", "failed", "bank apply") ||
		!slices.ContainsFunc(env.Data.Warnings, func(w string) bool { return strings.Contains(w, "run backsight bank apply") }) {
		t.Errorf("calls %q\n%s", bins.calls, out)
	}
}

// A restore brings back the archive's bank settings; when they are not
// this binary's template, restore applies it and records it.
func TestRestoreAppliesTheTemplateOverTheArchivesSettings(t *testing.T) {
	home, archive, _, _ := restoreSetup(t)
	olderTemplate(t, home) // the archive's settings: an older template
	code, out, errOut := run(t, "restore", archive, "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var env struct {
		Data restoreResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if !hasApplyStep(env.Data.Steps, "bank", "done", "retain_strategies") || !hasSubagentStrategy(t) || recordedHash(t, home) != server.TemplateHash() {
		t.Errorf("steps %+v, subagent %v, hash %s", env.Data.Steps, hasSubagentStrategy(t), recordedHash(t, home))
	}
	if i, j := applyStepIndex(env.Data.Steps, "epoch"), applyStepIndex(env.Data.Steps, "bank"); i < 0 || j < i {
		t.Errorf("bank step before the epoch: %+v", env.Data.Steps)
	}
}

// When restore cannot apply the template, it clears the recorded hash so
// the next client install or update sends it, and says so; the restore
// itself passed.
func TestRestoreClearsTheHashWhenTheTemplateCannotBeApplied(t *testing.T) {
	home, archive, _, _ := restoreSetup(t)
	olderTemplate(t, home)
	bankConfigStatus.Store(500)
	code, out, errOut := run(t, "restore", archive, "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var env struct {
		Data restoreResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if !hasApplyStep(env.Data.Steps, "bank", "failed", "") || recordedHash(t, home) != "" ||
		!slices.ContainsFunc(env.Data.Warnings, func(w string) bool { return strings.Contains(w, "run backsight bank apply") }) {
		t.Errorf("steps %+v, warnings %v, hash %q", env.Data.Steps, env.Data.Warnings, recordedHash(t, home))
	}
	if env.Data.Epoch == nil || env.Data.RolledBack {
		t.Errorf("the restore did not pass: %+v", env.Data)
	}
}

// asClient makes the install a client of the fake server at port
// (server.mode remote): the server and its banks are another machine's.
func asClient(t *testing.T, home string, port int) {
	t.Helper()
	if err := os.WriteFile(pathsOf(home).ConfigFile(), []byte("[server]\nmode = \"remote\"\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The bank's settings are its host's: bank apply on a client refuses,
// names the host, and sends nothing (two clients on two versions would
// overwrite each other's template).
func TestBankApplyRefusedOnAClient(t *testing.T) {
	home := devInstall(t, true)
	asClient(t, home, fakeHindsight(t))
	olderTemplate(t, home)
	for _, args := range [][]string{{"bank", "apply"}, {"bank", "apply", "--force"}, {"bank", "apply", "--plan"}} {
		code, _, errOut := run(t, args...)
		if code != output.Usage || !strings.Contains(errOut, "run backsight bank apply on that server's host") || !strings.Contains(errOut, "127.0.0.1") {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	if len(bankCalls) != 0 || hasSubagentStrategy(t) {
		t.Errorf("a client touched the bank: %v", bankCalls)
	}
}

// client install on a client leaves the bank to the server's host: its
// bank step is skipped, plan and run, and never calls the bank.
func TestClientInstallSkipsTheBankOnAClient(t *testing.T) {
	home := devInstall(t, true)
	asClient(t, home, fakeHindsight(t))
	withClaude(t)
	olderTemplate(t, home)
	before := recordedHash(t, home)
	for _, args := range [][]string{{"client", "install", "--plan", "--json"}, {"client", "install", "--json"}} {
		code, out, errOut := run(t, args...)
		s := installStepNamed(installSteps(t, out), "bank")
		if code != output.OK || s == nil || s.Status != "skipped" || s.Detail != "the server's host applies its bank template (backsight bank apply there)" {
			t.Errorf("%v: exit %d %+v\n%s%s", args, code, s, out, errOut)
		}
	}
	if len(bankCalls) != 0 || hasSubagentStrategy(t) || recordedHash(t, home) != before {
		t.Errorf("a client touched the bank: %v, hash %s", bankCalls, recordedHash(t, home))
	}
}
