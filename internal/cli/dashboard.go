package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
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
	s, err := dashboard.Start(ctx, cfg)
	if err != nil {
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
	return err
}

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
	return indexError(err)
}

func runDashboardLink(e *env) error {
	d, _, err := machine()
	if err != nil {
		return err
	}
	// The runtime file is judged on the real clock (process start times).
	r, alive, err := dashboard.Running(d, time.Now())
	if err != nil {
		return indexError(err)
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
		return indexError(err)
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
