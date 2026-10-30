package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/server"
)

// A native config gets the native runtime, with the units where the user
// manager reads them and the directories the install resolved; a Docker
// config keeps Docker.
func TestRuntimeForPicksNative(t *testing.T) {
	home := isolate(t)
	withSys(t, "linux")
	d := pathsOf(home)
	cfg := config.Default(d, home)
	if k := runtimeFor(cfg).Kind(); k != "docker" {
		t.Errorf("a config without server.runtime runs %s", k)
	}
	cfg.Server.Runtime = "native"
	rt := runtimeFor(cfg)
	if rt.Kind() != "native" {
		t.Fatalf("native config runs %s", rt.Kind())
	}
	n, ok := rt.(server.Native)
	if !ok {
		t.Fatalf("%T", rt)
	}
	if n.Err != nil {
		t.Fatal(n.Err)
	}
	if want := filepath.Join(home, "xdg", "systemd", "user"); n.UnitDir != want {
		t.Errorf("UnitDir %s, want %s (the user manager's XDG_CONFIG_HOME)", n.UnitDir, want)
	}
	if want := map[string]string{"BACKSIGHT_HOME": filepath.Join(home, "backsight")}; !reflect.DeepEqual(n.Env, want) {
		t.Errorf("Env %v, want %v (directories only, no PATH)", n.Env, want)
	}
	if n.Dirs != d || n.Proc != "/proc" || n.Cfg.Server.Runtime != "native" {
		t.Errorf("Dirs %+v Proc %q", n.Dirs, n.Proc)
	}
	// TestMain's refusing runners: no test reaches systemd or a tool.
	if err := n.Stop(context.Background()); err != nil {
		t.Errorf("Stop without units: %v", err)
	}
	if err := (server.Native{Cfg: cfg, Sys: n.Sys, UnitDir: t.TempDir()}).StopAPI(context.Background()); err != nil {
		t.Errorf("StopAPI without units: %v", err)
	}
	if _, err := n.Sys.Run(context.Background(), "systemctl", "--user", "daemon-reload"); err == nil || !strings.Contains(err.Error(), "without a fake") {
		t.Errorf("nativeSys in tests: %v", err)
	}
	if _, err := n.Tools.Run(context.Background(), nil, nil, "/bin/true"); err == nil || !strings.Contains(err.Error(), "without a fake") {
		t.Errorf("nativeTools in tests: %v", err)
	}
	if g := runtimeWith(cfg, true).(server.Native); g.Tools == nil {
		t.Error("own-group runtime has no tools")
	}
}

// execTools runs a tool with exactly the environment it is given.
func TestExecToolsGivesExactlyItsEnvironment(t *testing.T) {
	t.Setenv("PYTHONPATH", "/evil")
	out, err := execTools{}.Run(context.Background(), []string{"A=1"}, strings.NewReader("in"), "/usr/bin/env")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "A=1" {
		t.Errorf("env %q, want only A=1", got)
	}
	out, err = execTools{}.Run(context.Background(), nil, nil, "/usr/bin/env")
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("nil env inherited: %q %v", out, err)
	}
	_, err = execTools{}.Run(context.Background(), nil, nil, "/bin/sh", "-c", "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("stderr's tail missing from %v", err)
	}
}

// A user manager that never answers leaves the default unit directory,
// in bounded time.
func TestRuntimeForNativeDoesNotHangOnTheUserManager(t *testing.T) {
	home := isolate(t)
	f := withSys(t, "linux")
	f.hang = true
	old := unitDirTimeout
	unitDirTimeout = 50 * time.Millisecond
	t.Cleanup(func() { unitDirTimeout = old })
	cfg := config.Default(pathsOf(home), home)
	cfg.Server.Runtime = "native"
	n := runtimeFor(cfg).(server.Native)
	if want := filepath.Join(home, ".config", "systemd", "user"); n.UnitDir != want {
		t.Errorf("UnitDir %s, want %s", n.UnitDir, want)
	}
}

// groupSys is a user manager fake that says whether it runs its children
// in their own process group.
type groupSys struct {
	refuseSys
	own bool
}

func (s groupSys) InOwnGroup() server.SysRunner { s.own = true; return s }

// A Ctrl-C at the terminal must not kill a rollback's systemctl: the
// own-group runtime runs the user manager's tools in their own process
// group, as it does Docker's and the native tools.
func TestOwnGroupRuntimeRunsSystemctlInItsOwnGroup(t *testing.T) {
	home := isolate(t)
	withSys(t, "linux")
	old := nativeSys
	nativeSys = groupSys{}
	t.Cleanup(func() { nativeSys = old })
	cfg := config.Default(pathsOf(home), home)
	cfg.Server.Runtime = "native"
	if s := runtimeWith(cfg, true).(server.Native).Sys; s != (groupSys{own: true}) {
		t.Errorf("own-group runtime's Sys %#v, want its own group", s)
	}
	if s := runtimeWith(cfg, false).(server.Native).Sys; s != (groupSys{}) {
		t.Errorf("runtime's Sys %#v, want the terminal's group", s)
	}
	// execSys's own group, as the kernel sees it.
	if runtime.GOOS != "linux" {
		return
	}
	pgrp := func(r server.SysRunner) (pid, group string) {
		out, err := r.Run(context.Background(), "/bin/sh", "-c", "cat /proc/$$/stat")
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(string(out))
		return f[0], f[4]
	}
	s, ok := any(execSys{}).(interface{ InOwnGroup() server.SysRunner })
	if !ok {
		t.Fatal("execSys cannot run in its own group")
	}
	if pid, group := pgrp(s.InOwnGroup()); pid != group {
		t.Errorf("own group: pid %s in group %s", pid, group)
	}
	self, err := os.ReadFile("/proc/" + strconv.Itoa(os.Getpid()) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	ours := strings.Fields(string(self[bytes.LastIndexByte(self, ')')+1:]))[2] // the fields after comm: state, ppid, pgrp
	if _, group := pgrp(execSys{}); group != ours {
		t.Errorf("default: group %s, want this process's %s", group, ours)
	}
}
