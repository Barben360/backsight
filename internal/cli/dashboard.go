package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/dashboard"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/telemetry"
)

// Messages of the dashboard commands.
const (
	portUsage         = "--port: 0 for a free port, or a whole number from 1024 to 65535"
	dashboardStarting = "a dashboard is starting: run backsight dashboard link in a few seconds"
	noDashboard       = "no dashboard is running: start one with backsight dashboard"
)

// newDashboardCmd is `dashboard`, which serves the memory dashboard in the
// foreground, and `dashboard link`, which prints a new link for the one
// that runs.
func newDashboardCmd(e *env) *cobra.Command {
	var port int
	var open bool
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Serve the memory dashboard on this machine and print its one-time link",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDashboard(cmd.Context(), e, port, open)
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "the port to listen on, 1024 to 65535 (default: a free one)")
	cmd.Flags().BoolVar(&open, "open", false, "open the link in the default browser")
	cmd.AddCommand(&cobra.Command{
		Use:   "link",
		Short: "Print a new one-time link for the running dashboard",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(*cobra.Command, []string) error { return runDashboardLink(e) },
	})
	return cmd
}

func runDashboard(ctx context.Context, e *env, port int, open bool) error {
	began := time.Now()
	if port != 0 && (port < 1024 || port > 65535) {
		return &output.Error{Code: output.Usage, Message: portUsage}
	}
	d, home, err := machine()
	if err != nil {
		return err
	}
	cfg, err := dashboardConfig(ctx, d, home, port, e.stderr)
	if err != nil {
		return err
	}
	s, err := startServer(ctx, cfg)
	if err != nil {
		// Stopped before it served: nothing is left running. A failure that
		// is not the stop itself (a taken port, an unwritable directory) is
		// still reported, even when Ctrl+C came at the same time.
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			_, _ = fmt.Fprintln(e.stderr, "the dashboard was stopped before it started")
			return nil
		}
		return dashboardStartError(port, err)
	}
	tw := telemetry.New(d.State, telemetry.Common{BinaryVersion: buildinfo.Version})
	link := s.FirstLink()
	if err := e.emit("dashboard", link, func() string { return "Dashboard: " + link.URL + "\n" }); err != nil {
		_, _ = s.Serve(canceled())
		return err
	}
	_, _ = fmt.Fprintln(e.stderr, "Open the link within 2 minutes (backsight dashboard link prints a new one). Ctrl+C stops the dashboard.")
	tw.Emit(telemetry.Dashboard{Action: "start", MS: time.Since(began).Milliseconds()})
	if open {
		if err := openBrowser(link.URL); err != nil {
			_, _ = fmt.Fprintln(e.stderr, "could not open a browser: open the link above")
		}
	}
	served := time.Now()
	_, err = s.Serve(ctx)
	tw.Emit(telemetry.Dashboard{Action: "stop", MS: time.Since(served).Milliseconds(), Pages: s.Pages()})
	if err != nil {
		// The result is already on stdout: the failure goes to stderr only,
		// as sentences (the error's own text holds paths).
		reportStopFailure(e.stderr, err)
		return reported{output.Blocked}
	}
	return nil
}

// reportStopFailure says why Serve ended with err: a cleanup failure is
// one, anything else has its own sentence.
func reportStopFailure(w io.Writer, err error) {
	var serve *dashboard.ServeError
	var cleanup *dashboard.CleanupError
	isServe, isCleanup := errors.As(err, &serve), errors.As(err, &cleanup)
	if isServe {
		_, _ = fmt.Fprintln(w, "backsight: the dashboard stopped because it could not keep serving")
	}
	if isCleanup {
		_, _ = fmt.Fprintf(w, "backsight: the dashboard stopped but could not clean up its files in the state directory (%s): check that directory\n", fileClass(cleanup))
	}
	if !isServe && !isCleanup {
		_, _ = fmt.Fprintln(w, "backsight: the dashboard stopped with an error")
	}
}

// fileClass names why a file operation failed, never the path.
func fileClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, syscall.ENOSPC):
		return "disk full"
	case errors.Is(err, fs.ErrNotExist):
		return "missing"
	}
	return "io"
}

// startServer is dashboard.Start; tests replace it.
var startServer = dashboard.Start

// canceled is a context already done: Serve then stops at once.
func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// dashboardStartError maps Start's refusals to the command's exit codes.
func dashboardStartError(port int, err error) error {
	var running *dashboard.RunningError
	switch {
	case errors.As(err, &running):
		if running.Port == 0 {
			return &output.Error{Code: output.Blocked, Message: dashboardStarting}
		}
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("a dashboard is already running at http://127.0.0.1:%d/: run backsight dashboard link for a new link", running.Port)}
	case errors.Is(err, dashboard.ErrPortTaken):
		return &output.Error{Code: output.Usage, Message: fmt.Sprintf("port %d is taken: choose another --port or leave it out for a free one", port)}
	}
	var se *dashboard.StartError
	if errors.As(err, &se) {
		switch se.Kind {
		case dashboard.StartIndex:
			return indexError(err)
		case dashboard.StartListen:
			on := "on a free port of 127.0.0.1"
			if port != 0 {
				on = "on 127.0.0.1:" + strconv.Itoa(port)
			}
			return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("could not listen %s (%s)", on, fileClass(err))}
		case dashboard.StartLoginCode:
			return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("could not create a login code (%s)", fileClass(err))}
		case dashboard.StartRuntimeFile:
			return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("could not write the dashboard's runtime file or its directory in the state directory (%s)", fileClass(err))}
		}
	}
	return &output.Error{Code: output.Blocked, Message: "the dashboard could not start: an unexpected internal error"}
}

func runDashboardLink(e *env) error {
	d, _, err := machine()
	if err != nil {
		return err
	}
	// The runtime file is judged on the real clock (process start times).
	r, alive, err := dashboard.Running(d, time.Now())
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("could not read the dashboard's runtime file (%s)", fileClass(err))}
	}
	switch {
	case !alive:
		return &output.Error{Code: output.Blocked, Message: noDashboard}
	case r.Port == 0:
		return &output.Error{Code: output.Blocked, Message: dashboardStarting}
	}
	at := now()
	code, err := dashboard.MintCode(d, at)
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("could not create a login code (%s)", fileClass(err))}
	}
	link := dashboard.Link{URL: dashboard.URL(r.Port, code), Port: r.Port, ExpiresAt: at.Add(dashboard.CodeTTL).UTC()}
	return e.emit("dashboard link", link, func() string { return "Dashboard: " + link.URL + "\n" })
}

// dashboardConfig is what the dashboard serves: the views' source (the
// configuration, a read-only server client, the doctor) and the metrics
// index, opened or built in the background by the dashboard itself.
func dashboardConfig(_ context.Context, d paths.Dirs, home string, port int, stderr io.Writer) (dashboard.Config, error) {
	src, err := viewServer(d, home)
	if err != nil {
		return dashboard.Config{}, err
	}
	return dashboard.Config{
		Dirs: d, Port: port, Now: now, Version: buildinfo.Version, Stderr: stderr, Source: src,
		OpenIndex: func(ctx context.Context) (dashboard.Index, error) {
			ix, err := index.Open(ctx, indexOptionsAt(d, home, stderr))
			if err != nil {
				return nil, err // raw: the dashboard tells the update-in-progress error apart
			}
			return ix, nil
		},
		NeedsBuild: func(ctx context.Context) (bool, error) {
			st, err := index.Inspect(ctx, indexOptionsAt(d, home, nil))
			if err != nil {
				return false, err
			}
			return !st.Exists || st.IngestedAt.IsZero(), nil
		},
	}, nil
}

// openBrowser opens url in the default browser. It starts the program and
// reaps it in the background; the URL, which holds the one-time code, is
// never printed or logged.
var openBrowser = func(url string) error {
	argv := browserCommand(runtime.GOOS, url)
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// browserCommand is the program that opens url on goos.
func browserCommand(goos, url string) []string {
	switch goos {
	case "darwin":
		return []string{"open", url}
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler", url}
	}
	return []string{"xdg-open", url}
}
