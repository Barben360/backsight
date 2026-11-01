//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/supervise"
)

// nativeOnWindows names the native runtime in the install's config.toml
// and fakes the supervisor's pipe: it answers status with proto (0: not
// running). restarts counts update apply's supervisor restarts, which
// fail with failRestart.
func nativeOnWindows(t *testing.T, home string) (proto *int, restarts *int, failRestart *error) {
	t.Helper()
	d := pathsOf(home)
	raw, _ := os.ReadFile(d.ConfigFile())
	raw, _ = config.SetRuntime(raw, "native")
	if err := os.WriteFile(d.ConfigFile(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	proto, restarts, failRestart = new(int), new(int), new(error)
	oldA, oldR := nativeAsk, restartSupervisor
	nativeAsk = func(_ context.Context, project, op string) (supervise.Status, error) {
		if op != "status" {
			t.Errorf("update apply asked the supervisor %q", op)
		}
		if *proto == 0 {
			return supervise.Status{}, supervise.ErrNotRunning
		}
		return supervise.Status{Proto: *proto, Version: "0.1.0", PID: 4242, DB: "running", API: "running"}, nil
	}
	restartSupervisor = func(context.Context, server.Native) error {
		// Under server.lock, as server start and stop restart it.
		if h := state.Inspect(filepath.Join(d.LocksDir(), "server.lock")); h == nil || h.Stale || h.Owner.Command != "update apply" {
			t.Errorf("the supervisor restarted without update apply holding server.lock: %+v", h)
		}
		*restarts++
		return *failRestart
	}
	t.Cleanup(func() { nativeAsk, restartSupervisor = oldA, oldR })
	return proto, restarts, failRestart
}

// Review Focus 3: the supervisor keeps running its current.exe.old-<n>
// through a backsight-only update, and the result says so; it is
// restarted only when it speaks another protocol than the new binary
// (which reports its own, 1). A supervisor that is not running is left
// to its next start; a restart that fails is a warning, the update kept.
func TestUpdateApplyRestartsTheSupervisorOnlyOnAProtocolChange(t *testing.T) {
	home, _, _ := installedAt(t, "0.1.0")
	proto, restarts, failRestart := nativeOnWindows(t, home)
	key := trustTestKey(t)
	src := t.TempDir()
	apply := func(v string) applyEnvelope {
		t.Helper()
		signedRelease(t, src, v, key, relOpts{})
		code, out, _ := run(t, "update", "apply", "--source", src, "--json")
		env := decodeApply(t, out)
		if code != output.OK || env.Data.RolledBack || env.Data.Target != v {
			t.Fatalf("update to %s: exit %d\n%s", v, code, out)
		}
		return env
	}

	*proto = supervise.Proto // the same protocol: kept running the old backsight
	env := apply("0.2.0")
	if _, st := applyStep(env.Data.Steps, "supervisor"); st.Status != "kept" || st.Detail != "the server keeps running backsight 0.1.0 until its next restart" || *restarts != 0 {
		t.Errorf("same protocol: step %+v, %d restarts", st, *restarts)
	}

	*proto = supervise.Proto + 1 // another protocol: restarted
	setBuild(t, "0.2.0", "abc1234")
	env = apply("0.3.0")
	if _, st := applyStep(env.Data.Steps, "supervisor"); st.Status != "done" || !strings.Contains(st.Detail, "protocol 2") || *restarts != 1 {
		t.Errorf("another protocol: step %+v, %d restarts", st, *restarts)
	}

	*failRestart = errors.New("the supervisor did not answer")
	setBuild(t, "0.3.0", "abc1234")
	env = apply("0.4.0")
	if _, st := applyStep(env.Data.Steps, "supervisor"); st.Status != "failed" || *restarts != 2 ||
		!slices.ContainsFunc(env.Data.Warnings, func(w string) bool {
			return strings.Contains(w, "was not restarted") && strings.Contains(w, restartCmd)
		}) {
		t.Errorf("failed restart: step %+v, %d restarts, warnings %q", st, *restarts, env.Data.Warnings)
	}

	*proto = 0 // not running: nothing to restart
	setBuild(t, "0.4.0", "abc1234")
	env = apply("0.5.0")
	if _, st := applyStep(env.Data.Steps, "supervisor"); st.Status != "skipped" || *restarts != 2 {
		t.Errorf("not running: step %+v, %d restarts", st, *restarts)
	}

	// The new binary cannot say its protocol: the supervisor keeps
	// running, and a warning names why.
	*proto, *failRestart = supervise.Proto, nil
	setBuild(t, "0.5.0", "abc1234")
	bin := filepath.Join(pathsOf(home).Data, "bin")
	inner := binaryExec
	binaryExec = versionFailsIn{inner: inner, dir: bin}
	t.Cleanup(func() { binaryExec = inner })
	env = apply("0.6.0")
	if _, st := applyStep(env.Data.Steps, "supervisor"); st.Status != "kept" || *restarts != 2 ||
		!slices.ContainsFunc(env.Data.Warnings, func(w string) bool {
			return strings.Contains(w, "did not report its supervisor protocol") && strings.Contains(w, "no version here") &&
				strings.Contains(w, "keeps running backsight 0.1.0")
		}) {
		t.Errorf("unknown protocol: step %+v, %d restarts, warnings %q", st, *restarts, env.Data.Warnings)
	}
	binaryExec = inner

	// Another command holds server.lock: no restart, a warning.
	*proto = supervise.Proto + 1
	setBuild(t, "0.6.0", "abc1234")
	l, _, err := state.Acquire(filepath.Join(pathsOf(home).LocksDir(), "server.lock"), "server start")
	if err != nil {
		t.Fatal(err)
	}
	env = apply("0.7.0")
	_ = l.Release()
	if _, st := applyStep(env.Data.Steps, "supervisor"); st.Status != "failed" || *restarts != 2 ||
		!slices.ContainsFunc(env.Data.Warnings, func(w string) bool {
			return strings.Contains(w, "was not restarted") && strings.Contains(w, "server start")
		}) {
		t.Errorf("server.lock held: step %+v, %d restarts, warnings %q", st, *restarts, env.Data.Warnings)
	}
}

// versionFailsIn fails `version` for a binary under dir (the installed
// ones), not for the staged binary update apply checks first.
type versionFailsIn struct {
	inner binaryRunner
	dir   string
}

func (v versionFailsIn) Run(ctx context.Context, exe string, args ...string) ([]byte, []byte, int, error) {
	if len(args) > 0 && args[0] == "version" && strings.HasPrefix(strings.ToLower(exe), strings.ToLower(v.dir)+`\`) {
		return nil, nil, 0, errors.New("no version here")
	}
	return v.inner.Run(ctx, exe, args...)
}
