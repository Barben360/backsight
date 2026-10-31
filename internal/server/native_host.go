package server

import (
	"context"
	"io"
	"strings"
)

// nativeHost runs the native server's two processes: systemd user units
// on Linux, the supervisor and its task on Windows.
type nativeHost interface {
	Files(pg int, venv string) (map[string][]byte, error) // base name → content, written under Dir()
	Dir() string
	StartDB(ctx context.Context, restart bool) error // enable/arm and start the database
	StartAPI(ctx context.Context, restart bool) error
	StopAPI(ctx context.Context) error
	Stop(ctx context.Context) error                        // both, and nothing starts at logon/boot
	Remove(ctx context.Context) error                      // stop, then unregister (files removed by the caller)
	States(ctx context.Context) (map[string]string, error) // "db", "hindsight" → active|activating|inactive|failed, absent when not installed
	Logs(ctx context.Context, service string, lines int) (string, error)
	DeployedVenv(files map[string][]byte) (string, error)                                             // the venv version the files run
	Transient(ctx context.Context, name string, env []string, stdin io.Reader, argv [][]string) error // steps run in order, every process dies with the call
	Owns(pid int) bool                                                                                // pid belongs to the server (cgroup / Job Object)
	Engine(ctx context.Context) (Engine, error)
	StopCmd() string
}

// host is the service manager of n's OS.
func (n Native) host() nativeHost {
	if n.goos() == "windows" {
		return pendingWindowsHost{n: n}
	}
	return systemdHost{n: n}
}

// What the interface says further (Task 7):
//
// Files' venv is the version whose venv the server runs (Version(),
// WithVersion's), as DeployedVenv reads it back.
//
// A States value may carry the host's own sub-state after a slash
// (systemd: activating/auto-restart); stateParts splits it.
//
// Transient's name is the transient job's name (systemd: its unit, and
// the settings systemdHost keeps for it). Its env is the steps' whole
// environment, stdin goes to them. In a step, leading KEY=value words are
// that step's own environment (env(1)); stepNoStdin and stepNoStdout,
// anywhere in a step, give it no stdin, or no stdout: pg0 forks a
// postmaster that keeps the stdout it was given, which must never be the
// call's pipe.
const (
	stepNoStdin  = "</dev/null"
	stepNoStdout = ">/dev/null"
)

// stateParts splits a States value into its state and its host's
// sub-state ("" without one).
func stateParts(s string) (state, sub string) {
	state, sub, _ = strings.Cut(s, "/")
	return state, sub
}

// Linux behaviours the interface leaves out (Task 7 ruling): a host that
// has them implements these, any other gets the fallback each names.

// dbAloneStarter starts the database without arming anything at
// logon/boot (systemd: start, not enable). Fallback: StartDB(ctx, false).
type dbAloneStarter interface {
	startDBAlone(ctx context.Context) error
}

// transientWatcher is a host one of whose Transient jobs may outlive the
// call (systemd: a unit started without --wait outlives systemd-run, until
// stopped or its RuntimeMaxSec): the caller asks whether it still runs and
// stops it. Fallback: it runs while the call does, and nothing is left to
// stop after it.
type transientWatcher interface {
	transientRunning(ctx context.Context, name string) bool
	stopTransient(ctx context.Context, name string)
}

// transientRunning is the host's answer, true without a watcher.
func (n Native) transientRunning(ctx context.Context, name string) bool {
	if w, ok := n.host().(transientWatcher); ok {
		return w.transientRunning(ctx, name)
	}
	return true
}

// stopTransient stops what the job named left running, on a context of
// its own (a cancelled one would run nothing).
func (n Native) stopTransient(ctx context.Context, name string) {
	if w, ok := n.host().(transientWatcher); ok {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		w.stopTransient(cctx, name)
	}
}
