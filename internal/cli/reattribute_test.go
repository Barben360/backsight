package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

func TestReattributeRefusesUnknownAnswers(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	for _, a := range []string{"resend=maybe", "backup=yes", "confirm=yes"} {
		if code, out, _ := run(t, "retain", "reattribute", "--answer", a); code != output.Usage {
			t.Errorf("--answer %s: exit %d\n%s", a, code, out)
		}
	}
}

// --plan reads and writes nothing; --json carries the plan.
func TestReattributePlanChangesNothing(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dk := withBackupDocker(t)
	code, out, _ := run(t, "retain", "reattribute", "--plan", "--json")
	var env struct {
		Data reattributeResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); code != output.OK || err != nil || env.Data.Action != "planned" || env.Data.Bank != "claude-memory" {
		t.Fatalf("exit %d %v\n%s", code, err, out)
	}
	if len(dk.calls) != 0 {
		t.Errorf("a plan took a backup: %q", dk.calls)
	}
	if code, out, _ := run(t, "retain", "reattribute"); code != output.OK || !strings.Contains(out, "nothing to move") {
		t.Errorf("nothing to do: exit %d\n%s", code, out)
	}
	if len(dk.calls) != 0 {
		t.Errorf("nothing to move, yet a backup was taken: %q", dk.calls)
	}
}

// The exit-code contract: 40 when a write failed (the same command
// finishes), 10 while re-sends wait for an answer, 0 otherwise.
func TestReattributeExitCodes(t *testing.T) {
	resends := retain.ReattributionPlan{Resends: []retain.Resend{{Session: "s1"}}}
	cases := []struct {
		name    string
		res     reattributeResult
		answers map[string]string
		opt     reattributeOptions
		code    int
		action  string
	}{
		{"nothing", reattributeResult{}, nil, reattributeOptions{}, output.OK, "nothing"},
		{"moved", reattributeResult{Retagged: []string{"d"}}, nil, reattributeOptions{}, output.OK, "done"},
		{"failed", reattributeResult{Retagged: []string{"d"}, Failed: []string{"e: HTTP 500"}}, nil, reattributeOptions{}, output.Partial, "partial"},
		{"re-sends unanswered", reattributeResult{Plan: resends}, nil, reattributeOptions{}, output.NeedsUser, "resends_pending"},
		{"re-sends declined", reattributeResult{Plan: resends}, map[string]string{"resend": "no"}, reattributeOptions{}, output.OK, "resends_pending"},
		{"re-sends left by the daily job", reattributeResult{Plan: resends}, nil, reattributeOptions{freeOnly: true}, output.OK, "resends_pending"},
		{"re-sends queued", reattributeResult{Plan: resends, Queued: []string{"s1"}}, map[string]string{"resend": "yes"}, reattributeOptions{}, output.OK, "done"},
	}
	for _, c := range cases {
		res, err := finishReattribute(c.res, reattributeFlags{}, c.opt, c.answers)
		if output.CodeOf(err) != c.code || res.Action != c.action {
			t.Errorf("%s: exit %d %q, want %d %q (%v)", c.name, output.CodeOf(err), res.Action, c.code, c.action, err)
		}
	}
}

// reportToMove sets up a subagent report an older build filed under its
// session's directory (billing) while its work was in shop, another of
// the user's repositories: a document retain reattribute moves.
func reportToMove(t *testing.T, home string, port int) {
	t.Helper()
	billing, shop := filepath.Join(home, "billing"), filepath.Join(home, "src", "shop")
	gitRepo(t, billing, "git@github.com:me/billing.git")
	gitRepo(t, shop, "git@github.com:me/shop.git")
	writeTranscript(t, home, "s1", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	agent := filepath.Join(home, ".claude", "projects", "-home-u-billing", "s1", "subagents", "agent-ag1.jsonl")
	_ = os.MkdirAll(filepath.Dir(agent), 0o700)
	edit := `{"type":"assistant","isSidechain":true,"uuid":"e%d","cwd":"` + billing + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"t%d","name":"Edit","input":{"file_path":"` + filepath.Join(shop, "cart.go") + `"}}]}}` + "\n"
	_ = os.WriteFile(agent, []byte(fmt.Sprintf(edit, 1, 1)+fmt.Sprintf(edit, 2, 2)), 0o600)
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "k", 5*time.Second)
	if err := cl.RetainNow(context.Background(), "claude-memory", []hindsight.RetainItem{{Content: "Fixed the cart.", DocumentID: "subagent:s1:ag1",
		Tags: []string{"project:billing", "source:subagent", "harness:claude-code"}, Metadata: map[string]string{"source": "subagent"}}}); err != nil {
		t.Fatal(err)
	}
}

func reattributeEnv(t *testing.T) (string, *backupDocker) {
	t.Helper()
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	dk := withBackupDocker(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	reportToMove(t, home, port)
	return home, dk
}

// The move happens after a verified backup; a failed write exits 40 and
// the same command finishes it; the run after that has nothing to do.
func TestReattributeMovesAfterAVerifiedBackupAndFinishesAfterAFailure(t *testing.T) {
	_, dk := reattributeEnv(t)
	docPatchFails.Store(true)
	code, out, _ := run(t, "retain", "reattribute", "--json")
	if code != output.Partial || !strings.Contains(out, "subagent:s1:ag1") || len(docPatches) != 0 {
		t.Fatalf("a 503 on the move: exit %d\n%s", code, out)
	}
	docPatchFails.Store(false)
	if code, out, _ := run(t, "retain", "reattribute"); code != output.OK || !strings.Contains(out, "moved subagent:s1:ag1: billing → shop") {
		t.Fatalf("the same command again: exit %d\n%s", code, out)
	}
	if !slices.Contains(docPatches, "subagent:s1:ag1") {
		t.Errorf("patches %v", docPatches)
	}
	verified := false
	for _, c := range dk.calls {
		verified = verified || strings.HasPrefix(c, "exec backsight-verify-")
	}
	if !verified {
		t.Errorf("no verify before the move: %q", dk.calls)
	}
	if code, out, _ := run(t, "retain", "reattribute"); code != output.OK || !strings.Contains(out, "nothing to move") {
		t.Errorf("a third run: exit %d\n%s", code, out)
	}
}

// Nothing is written while it is not safe: a backup that does not verify,
// the maintenance lock held, a restore or an update not finished.
func TestReattributeRefusesWhenItIsNotSafe(t *testing.T) {
	for name, c := range map[string]struct {
		prep func(home string, dk *backupDocker)
		code int
	}{
		"backup verify fails": {func(_ string, dk *backupDocker) { dk.restored = "41" }, output.Blocked},
		"maintenance lock held": {func(home string, _ *backupDocker) {
			if _, err := state.TryAcquire(pathsOf(home).MaintenanceLock(), "backup create"); err != nil {
				t.Fatal(err)
			}
		}, output.Blocked},
		"restore not finished": {func(home string, _ *backupDocker) {
			if err := state.WriteRestoreMarker(pathsOf(home), state.RestoreMarker{Command: "restore"}); err != nil {
				t.Fatal(err)
			}
		}, output.Blocked},
	} {
		t.Run(name, func(t *testing.T) {
			home, dk := reattributeEnv(t)
			c.prep(home, dk)
			if code, out, _ := run(t, "retain", "reattribute", "--answer", "resend=yes"); code != c.code || len(docPatches) != 0 {
				t.Errorf("exit %d (want %d), patches %v\n%s", code, c.code, docPatches, out)
			}
		})
	}
}

// The claude-code provider runs the server's work on the subscription:
// the moves wait for --allow-subscription, as a backfill does.
func TestReattributeAsksBeforeUsingTheSubscription(t *testing.T) {
	home, _ := reattributeEnv(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[llm]\nprovider = \"claude-code\"\nmodel = \"sonnet\"\n")...), 0o600)
	if code, out, errOut := run(t, "retain", "reattribute"); code != output.NeedsUser || len(docPatches) != 0 || !strings.Contains(errOut, "--allow-subscription") {
		t.Fatalf("exit %d, patches %v\n%s%s", code, docPatches, out, errOut)
	}
	if code, out, _ := run(t, "retain", "reattribute", "--allow-subscription"); code != output.OK || len(docPatches) != 1 {
		t.Errorf("allowed: exit %d, patches %v\n%s", code, docPatches, out)
	}
}

// mixedSessionDoc adds session sid, which moved from billing to shop and
// an older build sent as one document tagged shop: a re-send.
func mixedSessionDoc(t *testing.T, home string, port int, sid string) {
	t.Helper()
	p := filepath.Join(home, ".claude", "projects", "-home-u-billing", sid+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	rec := func(uuid, cwd string) string {
		return `{"type":"user","uuid":"` + uuid + `","sessionId":"` + sid + `","cwd":"` + cwd + `","timestamp":"2026-10-24T09:00:00Z","message":{"role":"user","content":"work"}}` + "\n"
	}
	_ = os.WriteFile(p, []byte(rec(sid+"-u1", filepath.Join(home, "billing"))+rec(sid+"-u2", filepath.Join(home, "src", "shop"))), 0o600)
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "k", 5*time.Second)
	if err := cl.RetainNow(context.Background(), "claude-memory", []hindsight.RetainItem{{Content: "work", DocumentID: "conversation:" + sid,
		Tags: []string{"project:shop", "source:chat"}, Metadata: map[string]string{"session_id": sid, "last_uuid": sid + "-u2", "segment": "1"}}}); err != nil {
		t.Fatal(err)
	}
}

func deletedDocs() []string {
	var out []string
	for _, c := range curation {
		if strings.HasPrefix(c, "DELETE") && strings.Contains(c, "/documents/") {
			out = append(out, c)
		}
	}
	return out
}

// In one run, re-sends come before the re-tags whose consolidation would
// queue operations; a re-send that finds the queue busy changes nothing,
// the run exits 20, and the next run completes it, once.
func TestReattributeResendsBeforeRetagsAndWaitsForTheQueue(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	withBackupDocker(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	reportToMove(t, home, port)
	mixedSessionDoc(t, home, port, "s2")
	mixedSessionDoc(t, home, port, "s3")
	queueOnWrite.Store(true) // each delete or move leaves a consolidation running
	code, out, errOut := run(t, "retain", "reattribute", "--answer", "resend=yes", "--json")
	if code != output.Blocked || !strings.Contains(out, `"waiting"`) || len(deletedDocs()) != 1 || len(docPatches) != 1 {
		t.Fatalf("exit %d, deleted %v, patches %v\n%s%s", code, deletedDocs(), docPatches, out, errOut)
	}
	var env struct {
		Data reattributeResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	waiting := env.Data.Waiting[0]
	// nothing changed for the waiting session: no mark, no generation
	cursors := pathsOf(home).CursorsDir()
	for _, f := range []string{waiting + ".dropped", waiting + ".generation"} {
		if _, err := os.Stat(filepath.Join(cursors, f)); err == nil {
			t.Errorf("%s written for a re-send that waited", f)
		}
	}
	processing.Store(0) // the consolidation ran
	queueOnWrite.Store(false)
	if code, out, errOut := run(t, "retain", "reattribute", "--answer", "resend=yes"); code != output.OK || len(deletedDocs()) != 2 {
		t.Fatalf("the next run: exit %d, deleted %v\n%s%s", code, deletedDocs(), out, errOut)
	}
	if code, out, _ := run(t, "retain", "reattribute", "--answer", "resend=yes"); code != output.OK || len(deletedDocs()) != 2 {
		t.Errorf("a third run deleted again: exit %d, %v\n%s", code, deletedDocs(), out)
	}
}

// The daily job moves a document after its verified backup, never
// re-sends one (the mixed session stays, recorded for doctor), and a
// failing server only marks its line "failed".
func TestMaintainMovesAfterItsVerifiedBackupAndNeverResends(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	dk := withBackupDocker(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	reportToMove(t, home, port)
	mixedSessionDoc(t, home, port, "s2")
	docPatchFails.Store(true)
	if code, out, _ := run(t, "jobs", "maintain"); code != output.OK || !strings.Contains(out, "re-attribution failed:") || len(docPatches) != 0 {
		t.Fatalf("a failing server: exit %d, patches %v\n%s", code, docPatches, out)
	}
	docPatchFails.Store(false)
	code, out, _ := run(t, "jobs", "maintain")
	if code != output.OK || !strings.Contains(out, "re-attribution 1 moved, 1 session(s) to send again") || !slices.Contains(docPatches, "subagent:s1:ag1") {
		t.Fatalf("exit %d, patches %v\n%s", code, docPatches, out)
	}
	for _, c := range curation {
		if strings.HasPrefix(c, "DELETE") && strings.Contains(c, "/documents/") {
			t.Errorf("the daily job deleted a document: %s", c)
		}
	}
	verified := false
	for _, c := range dk.calls {
		verified = verified || strings.HasPrefix(c, "exec backsight-verify-")
	}
	if !verified {
		t.Error("the moves did not wait for a verified backup")
	}
	if st, ok, _ := retain.LoadReattributionStatus(pathsOf(home).State); !ok || len(st.Pending) != 1 || st.Pending[0].Session != "s2" {
		t.Errorf("status %+v", st)
	}
}

// No move when today's backup is missing or does not verify, a restore
// is not finished, or the provider runs on the subscription; the reason
// is kept for doctor.
func TestMaintainMovesNothingWhenItIsNotSafe(t *testing.T) {
	for name, c := range map[string]struct {
		prep func(home string, dk *backupDocker)
		line string
	}{
		"backup fails":           {func(_ string, dk *backupDocker) { dk.failDump = true }, "today's backup failed"},
		"backup does not verify": {func(_ string, dk *backupDocker) { dk.restored = "41" }, "today's backup did not verify"},
		"restore not finished": {func(home string, _ *backupDocker) {
			_ = state.WriteRestoreMarker(pathsOf(home), state.RestoreMarker{Command: "restore"})
		}, "re-attribution"},
		"subscription": {func(home string, _ *backupDocker) {
			cfg := filepath.Join(home, "backsight", "config", "config.toml")
			raw, _ := os.ReadFile(cfg)
			_ = os.WriteFile(cfg, append(raw, []byte("\n[llm]\nprovider = \"claude-code\"\nmodel = \"sonnet\"\n")...), 0o600)
		}, "on your subscription"},
	} {
		t.Run(name, func(t *testing.T) {
			home := devInstall(t, true)
			port := fakeHindsight(t)
			pointAt(t, home, port)
			dk := withBackupDocker(t)
			withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
			reportToMove(t, home, port)
			c.prep(home, dk)
			_, out, _ := run(t, "jobs", "maintain")
			if len(docPatches) != 0 || !strings.Contains(out, c.line) {
				t.Errorf("patches %v\n%s", docPatches, out)
			}
		})
	}
}

// A "no" to the re-sends stands across the daily runs while the same
// sessions are left; a new one makes the question come back.
func TestADeclinedResendIsRemembered(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	withBackupDocker(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	mixedSessionDoc(t, home, port, "s2")
	gitRepo(t, filepath.Join(home, "billing"), "git@github.com:me/billing.git")
	gitRepo(t, filepath.Join(home, "src", "shop"), "git@github.com:me/shop.git")
	if code, out, _ := run(t, "retain", "reattribute", "--answer", "resend=no"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	run(t, "jobs", "maintain")
	if st, _, _ := retain.LoadReattributionStatus(pathsOf(home).State); !st.Declined || len(st.Pending) != 1 {
		t.Fatalf("after the daily run: %+v", st)
	}
	// another mixed session appears: the answer was about the other one
	mixed3 := filepath.Join(home, ".claude", "projects", "-home-u-billing", "s3.jsonl")
	raw, _ := os.ReadFile(filepath.Join(home, ".claude", "projects", "-home-u-billing", "s2.jsonl"))
	_ = os.WriteFile(mixed3, []byte(strings.ReplaceAll(string(raw), "s2", "s3")), 0o600)
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "k", 5*time.Second)
	_ = cl.RetainNow(context.Background(), "claude-memory", []hindsight.RetainItem{{Content: "work", DocumentID: "conversation:s3",
		Tags: []string{"project:shop"}, Metadata: map[string]string{"session_id": "s3", "last_uuid": "s3-u2", "segment": "1"}}})
	run(t, "jobs", "maintain")
	if st, _, _ := retain.LoadReattributionStatus(pathsOf(home).State); st.Declined || len(st.Pending) != 2 {
		t.Errorf("with a new session to send again: %+v", st)
	}
}
