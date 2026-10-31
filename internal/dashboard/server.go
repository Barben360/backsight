package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

// Config is what the dashboard is started with. It binds 127.0.0.1 only:
// nothing here chooses another address.
type Config struct {
	Dirs    paths.Dirs
	Port    int              // 0: a free port; else 1024-65535
	Now     func() time.Time // nil: time.Now
	Version string           // the footer's
	Stderr  io.Writer        // progress and hints; nil: io.Discard
}

// ErrPortTaken: the fixed port asked for is in use.
var ErrPortTaken = errors.New("the port is taken")

// StopReason says why Serve returned.
type StopReason string

// StopInterrupted: Serve's context ended (Ctrl+C, a signal).
const StopInterrupted StopReason = "interrupted"

// shutdownGrace is how long the requests running at the stop may finish.
var shutdownGrace = 5 * time.Second

// beforeShutdown runs once Serve's context ended, before the shutdown:
// tests release a blocked request there.
var beforeShutdown = func() {}

// Server is a started dashboard: bound, claimed, not yet serving.
type Server struct {
	cfg      Config
	now      func() time.Time
	ln       net.Listener
	port     int
	pid      int
	srv      *http.Server
	sessions *sessions
	link     Link

	idleMu    sync.Mutex
	lastTouch time.Time // the latest signed-in request or login; Start's time before
}

// Start refuses while another dashboard runs or starts (a *RunningError,
// read before binding), listens on 127.0.0.1:cfg.Port (ErrPortTaken when
// that port is in use), claims the runtime file with the bound port,
// removes every login code a previous run left and, last, mints the first
// code (FirstLink). A Start that fails after a step undoes it.
func Start(ctx context.Context, cfg Config) (_ *Server, err error) {
	if cfg.Port != 0 && (cfg.Port < 1024 || cfg.Port > 65535) {
		return nil, fmt.Errorf("port %d: give 0 for a free port, or a port from 1024 to 65535", cfg.Port)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	if err := privateDirs(cfg.Dirs); err != nil {
		return nil, err
	}
	// The runtime file's times are the system's (file mtimes, process
	// start times): it is judged on the real clock, not cfg.Now.
	if r, alive, err := Running(cfg.Dirs, time.Now()); err != nil {
		return nil, err
	} else if alive {
		return nil, &RunningError{Port: r.Port}
	}

	lc := listenConfig()
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)))
	if err != nil {
		if addrInUse(err) {
			return nil, fmt.Errorf("port %d: %w", cfg.Port, ErrPortTaken)
		}
		return nil, fmt.Errorf("listen on 127.0.0.1: %w", err)
	}
	var undo []func()
	defer func() {
		if err != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()
	undo = append(undo, func() { _ = ln.Close() })

	// The undo steps read only these locals: the result is nil when they run.
	pid, port := os.Getpid(), ln.Addr().(*net.TCPAddr).Port
	started := time.Now().UTC()
	if err := Claim(cfg.Dirs, Runtime{PID: pid, Port: port, StartedAt: started}, started); err != nil {
		return nil, err
	}
	undo = append(undo, func() { _ = Release(cfg.Dirs, pid) })
	s := &Server{cfg: cfg, now: cfg.Now, ln: ln, port: port, pid: pid, sessions: newSessions(cfg.Now)}
	s.lastTouch = s.now().Round(0)
	if err := SweepCodes(cfg.Dirs, s.now(), true); err != nil {
		return nil, err
	}

	s.srv = &http.Server{
		Handler:                      s.guard(s.routes()),
		ReadHeaderTimeout:            5 * time.Second,
		ReadTimeout:                  10 * time.Second,
		WriteTimeout:                 15 * time.Second,
		IdleTimeout:                  60 * time.Second,
		MaxHeaderBytes:               16384,
		DisableGeneralOptionsHandler: true, // "OPTIONS *" reaches the guard: 405
		ErrorLog:                     log.New(io.Discard, "", 0),
	}

	// Last: the code's 120 s start once everything before is done.
	minted := s.now()
	code, err := MintCode(cfg.Dirs, minted)
	if err != nil {
		return nil, err
	}
	s.link = Link{URL: URL(port, code), Port: port, ExpiresAt: minted.Add(CodeTTL).UTC()}
	return s, nil
}

// Port is the port the dashboard listens on.
func (s *Server) Port() int { return s.port }

// Serve answers requests until ctx ends ("interrupted") or nobody signed
// in used the dashboard for idleLimit ("idle"). It then lets the running
// requests finish for up to shutdownGrace, closes what is left, removes
// the runtime file and every login code.
func (s *Server) Serve(ctx context.Context) (StopReason, error) {
	// The requests' context outlives ctx: a request running at the stop
	// gets the shutdown grace, then the forced close cancels it.
	base := context.WithoutCancel(ctx)
	s.srv.BaseContext = func(net.Listener) context.Context { return base }
	ticks, stopTicks := idleTicks()
	defer stopTicks()
	served := make(chan error, 1)
	go func() { served <- s.srv.Serve(s.ln) }()

	var reason StopReason
	var errs []error
	for reason == "" && errs == nil {
		select {
		case <-ctx.Done():
			reason = StopInterrupted
		case <-ticks:
			if s.idle() {
				reason = StopIdle
			}
		case err := <-served:
			errs = append(errs, fmt.Errorf("serve the dashboard: %w", err))
			_ = s.srv.Close()
		}
	}
	if reason != "" {
		beforeShutdown()
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		if s.srv.Shutdown(sctx) != nil {
			_ = s.srv.Close() // the grace is over: drop what still runs
		}
		cancel()
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("serve the dashboard: %w", err))
		}
	}
	errs = append(errs, Release(s.cfg.Dirs, s.pid), SweepCodes(s.cfg.Dirs, s.now(), true))
	return reason, errors.Join(errs...)
}

// privateDirs creates dashboard/ and dashboard/codes/ 0700, and tightens
// them and claim.lock (0600) when an earlier run left them looser.
// Windows has no such modes.
func privateDirs(d paths.Dirs) error {
	if err := os.MkdirAll(CodesDir(d), 0o700); err != nil {
		return fmt.Errorf("create the dashboard directory: %w", err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	for _, x := range []struct {
		path string
		mode fs.FileMode
	}{
		{dashboardDir(d), 0o700},
		{CodesDir(d), 0o700},
		{filepath.Join(dashboardDir(d), "claim.lock"), 0o600},
	} {
		fi, err := os.Lstat(x.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check the dashboard directory: %w", err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 || fi.Mode().Perm()&^x.mode == 0 {
			continue
		}
		if err := os.Chmod(x.path, x.mode); err != nil {
			return fmt.Errorf("make %s private: %w", x.path, err)
		}
	}
	return nil
}
