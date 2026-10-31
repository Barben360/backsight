package server

import (
	"context"
	"errors"
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
	Transient(ctx context.Context, name string, env []string, stdin io.Reader, argv [][]string) error // steps run in order, every process dies with the call (one exception: transientWatcher)
	Owns(pid int) bool                                                                                // pid belongs to the server (cgroup / Job Object)
	Engine(ctx context.Context) (Engine, error)
	StopCmd() string

	// fileNames are Files' names: Delete removes the ones that exist
	// (none: nothing installed); the first is the one DeployedVenv reads.
	fileNames() []string
	// markers are restart-pending's lines: the database's first (Start
	// restarts it with StartDB), the API's second (StartAPI), then any
	// other the host reads itself.
	markers() []string
	// restartsFor are the markers a change to Files' name, or to
	// server.env (""), records when that file already existed.
	restartsFor(name string) []string
	// stopsWithoutFiles: Delete stops and removes even when none of
	// fileNames exists (systemd: no unit file, nothing to stop).
	stopsWithoutFiles() bool
}

// host is the service manager of n's OS.
func (n Native) host() nativeHost { return hostFor(n) }

// hostFor picks n's host; tests replace it with a fake.
var hostFor = func(n Native) nativeHost {
	if n.goos() == "windows" {
		return taskHost{n: n}
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
// has them implements these; any other host gets what each names.

// dbAloneStarter starts the database without arming anything at
// logon/boot (systemd: start, not enable). Without it StartDatabase
// refuses: StartDB arms the database.
type dbAloneStarter interface {
	startDBAlone(ctx context.Context) error
}

// errNoDBAlone is StartDatabase's refusal on a host that cannot start the
// database alone.
var errNoDBAlone = errors.New("this host cannot start the database alone, without starting it at every logon")

// transientWatcher is the one exception to Transient's "every process
// dies with the call": its host returns from a job whose last step is a
// server (VerifyDump's throwaway database) once that step started
// (systemd: a unit started without --wait outlives systemd-run, until
// stopped or its RuntimeMaxSec), and the caller asks whether it still runs
// and stops it. Any other host's Transient runs the server in the
// foreground until its context is cancelled: serverTransient runs it in a
// goroutine.
type transientWatcher interface {
	transientGone(ctx context.Context, name string) error // nil while it runs, else why it is not
	stopTransient(ctx context.Context, name string)
}

// stopTransient stops what the job named left running, on a context of
// its own (a cancelled one would run nothing). A host without a watcher
// left nothing: the job's processes died with its call.
func (n Native) stopTransient(ctx context.Context, name string) {
	if w, ok := n.host().(transientWatcher); ok {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		w.stopTransient(cctx, name)
	}
}

// serverJob is a Transient job whose last step is a server the caller
// uses while it runs.
type serverJob struct {
	// gone is nil while the server runs, else why it does not.
	gone func(ctx context.Context) error
	// stop ends the job and returns once nothing of it runs; nil when
	// nothing was started.
	stop func()
}

// serverTransient starts the job name, whose last step serves the caller
// until stop. On a transientWatcher host the job runs on its own and the
// watcher answers; elsewhere Transient runs in a goroutine on a context
// stop cancels, then waits for. stop is set even with an error: a failed
// start may have started something.
func (n Native) serverTransient(ctx context.Context, name string, env []string, steps [][]string) (serverJob, error) {
	h := n.host()
	if w, ok := h.(transientWatcher); ok {
		job := serverJob{
			gone: func(ctx context.Context) error { return w.transientGone(ctx, name) },
			stop: func() { n.stopTransient(ctx, name) },
		}
		return job, h.Transient(ctx, name, env, nil, steps)
	}
	tctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.Transient(tctx, name, env, nil, steps) }()
	var ended bool
	var why error
	end := func(err error) {
		ended, why = true, err
		if why == nil {
			why = errors.New("it exited")
		}
	}
	return serverJob{
		gone: func(context.Context) error {
			if !ended {
				select {
				case err := <-done:
					end(err)
				default:
				}
			}
			if ended {
				return why
			}
			return nil
		},
		stop: func() {
			cancel()
			if !ended {
				end(<-done)
			}
		},
	}, nil
}
