package cli

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/report"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/stats"
)

// reportDays is the telemetry window the report counts errors over.
const reportDays = 7

// osRelease names the Linux distribution; tests replace it.
var osRelease = func() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

// hostname is the machine's name, scrubbed from the report; tests replace it.
var hostname = os.Hostname

func newReportCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Print a redacted diagnostic and a pre-filled GitHub issue link (nothing is sent)",
		Long: `report gathers versions, the doctor report, local counts and recent
backsight errors, redacted: no memory text, prompt, transcript, secret or
path under your home directory (shown as ~). It prints a link to search
existing issues for the same error class and a link that opens a new
issue with a summary filled in, plus the full diagnostic to paste into it.
It sends nothing: you read it and decide (decision 0002).`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			rep, err := buildReport(cmd, d, home)
			if err != nil {
				return blocked(err)
			}
			return e.emit("report", rep, func() string { return reportText(rep) })
		},
	}
}

func buildReport(cmd *cobra.Command, d paths.Dirs, home string) (report.Report, error) {
	now := time.Now().UTC()
	diag := report.Diagnostic{GeneratedAt: now.Truncate(time.Second), Versions: reportVersions(cmd.Context(), d, home)}

	sc := report.NewScrubber(home)
	if h, err := hostname(); err == nil {
		sc.Name(h, "<host>")
		if short, _, ok := strings.Cut(h, "."); ok {
			sc.Name(short, "<host>")
		}
	}

	doc := runDoctor(cmd, d, home)
	diag.Doctor = report.Doctor{Checks: doc.Checks, Errors: doc.Errors, Warnings: doc.Warnings}
	if t := doc.Server; t != nil {
		srv := &report.Server{Source: t.Source}
		if u, err := url.Parse(t.URL); err == nil {
			srv.Scheme = u.Scheme
			host := u.Hostname()
			srv.Loopback = host == "localhost" || net.ParseIP(host).IsLoopback()
			if !srv.Loopback {
				sc.Name(host, "<server>")
			}
		}
		sc.Name(t.Bank, "<bank>")
		diag.Doctor.Server = srv
	}

	diag.Counts = reportCounts(d)
	if ev, err := stats.Load(d.TelemetryDir(), now.AddDate(0, 0, -reportDays), now.Add(time.Minute)); err == nil {
		diag.Counts.ErrorClasses, diag.RecentErrors = report.Errors(ev)
	} else {
		diag.Counts.ErrorClasses = map[string]int{"telemetry/unreadable": 1}
	}
	if diag.RecentErrors == nil {
		diag.RecentErrors = []report.ErrorEvent{}
	}
	return report.Build(diag, sc)
}

func reportVersions(ctx context.Context, d paths.Dirs, home string) report.Versions {
	v := report.Versions{Binary: buildinfo.Version, Commit: buildinfo.Commit, Date: buildinfo.Date, Go: runtime.Version(),
		HindsightPin: server.HindsightImage, OS: runtime.GOOS, Arch: runtime.GOARCH, OSRelease: osRelease()}
	if st, err := state.Load(d.StateFile()); err == nil {
		v.Plugin = st.PluginVersion
	}
	if cfg, err := config.Load(d.ConfigFile(), d, home); err == nil {
		v.HindsightDeployed, _ = runtimeFor(cfg).Deployed()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := claudeRunner.Run(ctx, nil, "--version")
	switch {
	case errors.Is(err, errClaudeMissing):
		v.ClaudeCode = "not found"
	case err != nil:
		v.ClaudeCode = "unknown"
	default:
		v.ClaudeCode = strings.Fields(string(out) + " unknown")[0]
	}
	return v
}

func reportCounts(d paths.Dirs) report.Counts {
	c := report.Counts{Days: reportDays}
	c.SessionsSent = countFiles(d.CursorsDir(), "*.json")
	c.SessionsTracked = countFiles(d.SessionsDir(), "*.json")
	if q, err := retain.QuarantinedSessions(d.CursorsDir()); err == nil {
		c.SessionsQuarantined = len(q)
	}
	ob := retain.Outbox{Path: filepath.Join(d.OutboxDir(), "notes.jsonl"), QuarantinePath: filepath.Join(d.QuarantineDir(), "notes.jsonl")}
	c.NotesQueued, _ = ob.Pending()
	c.NotesQuarantined, _ = ob.Quarantined()
	return c
}

func countFiles(dir, pattern string) int {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	return len(m)
}

func reportText(r report.Report) string {
	var b strings.Builder
	b.WriteString("backsight report: nothing was sent. Read the diagnostic below, then decide.\n\n")
	if r.Class != "" {
		b.WriteString("Error class: " + r.Class + "\n")
		b.WriteString("Existing issues with this class (look here first):\n  " + r.SearchURL + "\n")
	} else {
		b.WriteString("doctor found no error and the telemetry holds no recent one.\n")
	}
	b.WriteString("New issue with the title and a summary filled in (you submit it, or not):\n  " + r.IssueURL + "\n")
	if r.Omitted > 0 {
		b.WriteString("  (" + strconv.Itoa(r.Omitted) + " summary line(s) left out to keep the link short)\n")
	}
	b.WriteString("\nPaste this block into the issue, under \"Full diagnostic\":\n\n")
	b.WriteString(r.Paste)
	return b.String()
}
