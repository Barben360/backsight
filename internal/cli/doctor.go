package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/selfcheck"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// probes sets the machine probes; tests replace it.
var probes = realProbes

func realProbes(e *doctor.Env) {
	e.ManagedDir = claudecode.ManagedDir(runtime.GOOS)
	e.Docker = func(ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		// The runtime's engine needs no config.
		eng, err := runtimeFor(config.Config{}).Engine(ctx)
		return eng.Version, err
	}
	e.GPUs = doctor.LocalGPUs
	e.FreeBytes = doctor.FreeBytes
	if runtime.GOOS == "linux" {
		e.Linger = func() (bool, error) {
			out, err := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "-p", "Linger", "--value").Output()
			return strings.TrimSpace(string(out)) == "yes", err
		}
	}
	e.ClaudePlugins = func(profile string) ([]byte, error) {
		var env []string
		if profile != "" {
			env = []string{"CLAUDE_CONFIG_DIR=" + profile}
		}
		return claudeRunner.Run(context.Background(), env, "plugin", "list", "--json")
	}
	e.SelfCheck = func() state.SelfCheck {
		exe, err := selfExe()
		if err != nil {
			return state.SelfCheck{Status: selfcheck.Failed, Detail: "cannot locate the running binary: " + err.Error()}
		}
		return selfcheck.Current(e.Dirs, exe)
	}
	e.ReferenceServer = referenceServer
	e.ExposureDetail = func() (string, string) { return exposureDetail(e.Dirs, e.Home) }
	e.ConfigEdits = func(profile string) ([]claudecfg.FileResult, error) {
		c, err := newClientEnv(e.Dirs, e.Home, profile, e.ManagedDir)
		if err != nil {
			return nil, err
		}
		edits, _, err := c.ownedEdits()
		if err != nil {
			return nil, err
		}
		return claudecfg.Plan(edits)
	}
	e.Scheduler = func() (doctor.SchedulerEntries, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return schedulerEntries(ctx, e.Dirs)
	}
}

// hindsightTimeout bounds each Hindsight call of doctor and report.
const hindsightTimeout = 10 * time.Second

// doctorEnv is doctor's view of this machine. The runtime probes go
// through the server runtime's own runners (tests fake those), the machine
// probes after them (tests replace those whole).
func doctorEnv(d paths.Dirs, home string) doctor.Env {
	denv := doctor.Env{Dirs: d, Home: home, Getenv: os.Getenv, Timeout: hindsightTimeout}
	denv.Cwd, _ = os.Getwd()
	runtimeProbes(&denv)
	denv.MetricsIndex = func() (doctor.IndexFacts, error) { return indexFacts(context.Background(), d, home) }
	probes(&denv)
	return denv
}

// indexFactsTimeout bounds doctor's read of the metrics index (a lock or
// a disk that hangs reports the class timeout); inspectIndex is
// index.Inspect (tests slow it down).
var (
	indexFactsTimeout = 5 * time.Second
	inspectIndex      = index.Inspect
)

// indexFacts reads the metrics index for doctor, read-only (Inspect creates
// and migrates nothing). A failure comes back as its class alone: doctor
// output goes into report bundles, which must not carry paths. An index
// that exists but cannot be read is "corrupt", unless a stopped rebuild
// already says so.
func indexFacts(ctx context.Context, d paths.Dirs, home string) (doctor.IndexFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, indexFactsTimeout)
	defer cancel()
	st, err := inspectIndex(ctx, indexOptions(d, home, nil))
	if ctx.Err() != nil {
		return doctor.IndexFacts{}, errors.New("timeout")
	}
	if err != nil {
		return doctor.IndexFacts{}, errors.New(index.Class(err))
	}
	f := doctor.IndexFacts{
		Built: st.Exists, Newer: st.Newer, File: st.File, Size: st.Size, Schema: st.Schema, Head: st.Head,
		StoppedAt: st.StoppedAt, MigrateFailedAt: st.MigrateFailedAt,
		Errors24h: st.Errors24h, ErrorsByReason: st.Errors24hByReason, LagSeconds: st.LagSeconds,
	}
	if st.Filesystem.Kind != index.FSLocal {
		f.Folder = st.Filesystem.Name
		if f.Folder == "" {
			f.Folder = string(st.Filesystem.Kind)
		}
	}
	if st.Exists && !st.Readable && st.StoppedAt.IsZero() {
		return f, errors.New(index.Class(index.ErrUnreadable))
	}
	return f, nil
}

// nativeProbeWait bounds the native runtime's probes (systemctl, psql,
// pg_controldata, loginctl); switchProbeWait the other runtime's size.
var (
	nativeProbeWait = 20 * time.Second
	switchProbeWait = 15 * time.Second
)

// runtimeProbes read the server runtime's side of doctor (decision 0078):
// the native runtime's guards and the last runtime switch. Read-only:
// systemctl show, loginctl show-user, psql's SHOW fsync, pg_controldata,
// /proc, and the other runtime's files and sizes.
func runtimeProbes(e *doctor.Env) {
	e.Native = func(ctx context.Context) (doctor.NativeFacts, error) { return nativeFacts(ctx, e.Dirs, e.Home) }
	e.Switch = func() (*state.RuntimeSwitch, string) { return lastSwitch(e.Dirs, e.Home, false) }
}

// nativeFacts reads the native install's guards; a fact it cannot read is
// in Unread under its check's id.
func nativeFacts(ctx context.Context, d paths.Dirs, home string) (doctor.NativeFacts, error) {
	var f doctor.NativeFacts
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		return f, err
	}
	n, ok := runtimeFor(cfg).(server.Native)
	if !ok {
		return f, errors.New("config.toml does not name the native runtime")
	}
	if n.Err != nil {
		return f, n.Err
	}
	ctx, cancel := context.WithTimeout(ctx, nativeProbeWait)
	defer cancel()
	f.Unread = map[string]error{}
	unread := func(id string, err error) {
		if err != nil {
			f.Unread[id] = err
		}
	}
	if nativeGOOS == "windows" {
		windowsFacts(ctx, &f, n, d, unread)
	} else {
		f.Units, err = n.UnitStates(ctx)
		unread("native-unit", err)
	}
	f.Fsync, err = n.Fsync(ctx)
	unread("native-fsync", err)
	f.Identity = n.CheckInstance(ctx)
	strays, err := n.Strays()
	unread("native-stray", err)
	for _, s := range strays {
		f.Strays = append(f.Strays, s.PID)
	}
	if !f.Windows {
		f.Linger, err = linger()
		unread("native-linger", err)
	}
	unread("native-home", f.ReadHome(n.Home()))
	v, err := n.Deployed()
	if v == "" {
		v = n.Version()
	}
	f.Venv, f.Pin = n.VenvDir(v), n.VenvDir(n.Version())
	if err == nil {
		f.VenvDiff, err = doctor.NativeVenvDiff(f.Venv)
	}
	unread("native-venv", err)
	if f.Windows {
		f.DLLManifest = n.DLLManifest()
		f.DLLProblems, f.DLLCopies, err = n.DLLProblems(v)
		unread("native-dlls", err)
	}
	refused, _ := filepath.Glob(n.InstanceDir() + ".refused-*")
	for _, p := range refused {
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
			f.Refused = append(f.Refused, p)
		}
	}
	return f, nil
}

// nativeGOOS is the OS the native runtime runs on, for doctor and the
// SessionStart hook; tests replace it.
var nativeGOOS = runtime.GOOS

// windowsFacts reads what replaces the units and linger on Windows
// (decision 0080): the scheduled task, the supervisor's answer, and the
// DACLs of the data root, the config directory and both secret files.
func windowsFacts(ctx context.Context, f *doctor.NativeFacts, n server.Native, d paths.Dirs, unread func(string, error)) {
	f.Windows, f.Project, f.TaskName, f.LogDir, f.StartOnSession = true, n.Cfg.Server.Project, n.TaskName(), n.LogDir(), n.Cfg.Server.StartOnSession
	f.Stopped = n.Stopped()
	var err error
	f.Task, err = n.TaskState(ctx)
	unread("native-task", err)
	f.Supervisor, err = n.SupervisorStatus(ctx)
	unread("supervisor", err)
	for _, p := range []string{d.Data, d.Config, d.SecretsFile(), n.EnvFile()} {
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue // nothing there to read
		}
		pp := doctor.PrivatePath{Path: p, Dir: err == nil && fi.IsDir()}
		pp.Readers, pp.Err = otherReaders(p)
		f.Private = append(f.Private, pp)
	}
}

// otherReaders lists who but the user, SYSTEM and Administrators can read
// p (fsutil.OtherReaders); tests replace it.
var otherReaders = fsutil.OtherReaders

// keptSizeFresh is how long the size a switch measured of what it kept
// stands for it in doctor.
const keptSizeFresh = 24 * time.Hour

// lastSwitch is state.json's last runtime switch and, once it ended, the
// other runtime's data kept since: what server runtime clean deletes
// (ruling R43), "" when nothing of it is here or it cannot be told. Its
// size is the one the switch measured, for a day; after that, only when
// live (doctor --json): docker system df or a du of the native directory
// can take seconds, too long for every doctor, report and maintain run.
func lastSwitch(d paths.Dirs, home string, live bool) (*state.RuntimeSwitch, string) {
	st, err := state.Load(d.StateFile())
	if err != nil || st.RuntimeSwitch == nil {
		return nil, ""
	}
	rs := st.RuntimeSwitch
	if rs.Step == "running" {
		return rs, ""
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil || cfg.Server.Mode != "local" {
		return rs, ""
	}
	c, ok := runtimeFor(configOfKind(cfg, otherRuntime(cfg.RuntimeKind()))).(server.Cleaner)
	if !ok {
		return rs, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), switchProbeWait)
	defer cancel()
	if left, err := c.Leftover(ctx); err != nil || !left {
		return rs, ""
	}
	switch {
	case !rs.KeptAt.IsZero() && time.Since(rs.KeptAt) < keptSizeFresh:
		return rs, c.CleanWhat() + " (" + sizeText(rs.KeptBytes) + ")"
	case live:
		size := "size unknown"
		if n, err := c.CleanSize(ctx); err == nil {
			size = sizeText(n)
		}
		return rs, c.CleanWhat() + " (" + size + ")"
	}
	return rs, c.CleanWhat()
}

func newDoctorCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the memory stack and Claude Code wiring (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			rep := runDoctorSized(cmd, d, home, e.json)
			var failed error
			if rep.Errors > 0 {
				failed = shownError{&output.Error{Code: output.Blocked, Message: fmt.Sprintf("%d check(s) failed", rep.Errors)}}
			}
			return e.report("doctor", rep, func() string { return doctorText(rep) }, failed)
		},
	}
}

var statusLabel = map[doctor.Status]string{doctor.OK: "ok", doctor.Info: "info", doctor.Warn: "WARN", doctor.Error: "ERROR"}

func doctorText(r doctor.Report) string {
	var b strings.Builder
	pad := strings.Repeat(" ", 19)
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-6s %-11s %s\n", statusLabel[c.Status], c.ID, c.Summary)
		if c.Detail != "" {
			fmt.Fprintf(&b, "%s%s\n", pad, c.Detail)
		}
		if c.Fix != "" {
			fmt.Fprintf(&b, "%sfix: %s\n", pad, c.Fix)
		}
	}
	fmt.Fprintf(&b, "\n%d error(s), %d warning(s)\n", r.Errors, r.Warnings)
	return b.String()
}

// runDoctor runs every check (doctor and the jobs maintain snapshot).
func runDoctor(cmd *cobra.Command, d paths.Dirs, home string) doctor.Report {
	return runDoctorSized(cmd, d, home, false)
}

// runDoctorSized is runDoctor; sized also measures what a runtime switch
// kept when the switch's own measure is older than a day (doctor --json).
func runDoctorSized(cmd *cobra.Command, d paths.Dirs, home string, sized bool) doctor.Report {
	denv := doctorEnv(d, home)
	if sized {
		denv.Switch = func() (*state.RuntimeSwitch, string) { return lastSwitch(d, home, true) }
	}
	return doctor.Run(cmd.Context(), denv)
}

// referenceServer asks the reference server's /health, then, for a
// loopback server, which Docker container publishes its port: its compose
// project gives the stop command (0067 block 4). Read-only.
func referenceServer(ctx context.Context, rawURL string) (doctor.RefServer, error) {
	cl, err := hindsight.New(rawURL, "", 5*time.Second)
	if err != nil {
		return doctor.RefServer{}, err
	}
	if _, err := cl.Health(ctx); errors.Is(err, hindsight.ErrUnreachable) || errors.Is(err, hindsight.ErrTimeout) {
		return doctor.RefServer{}, nil
	}
	srv := doctor.RefServer{Running: true} // it answered, healthy or not
	port := localPort(rawURL)
	if port == "" {
		return srv, nil // another machine: its Docker is not this one's
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := composeRunner.Run(ctx, "ps", "--filter", "publish="+port, "--format", `{{.Label "com.docker.compose.project"}}`+"\t"+`{{.Names}}`)
	if err == nil {
		srv.Stop, srv.Start = stopCommands(string(out))
	}
	return srv, nil
}

// localPort is the port of a server URL on this machine (localhost or a
// loopback address), "" for another machine or a URL without a port.
func localPort(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Port() == "" {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return ""
	}
	return u.Port()
}

// stopCommands turns `docker ps` lines (compose project, tab, container
// name) into the commands that stop and start that server: the whole
// compose project when there is one, else the container. A project or a
// name that is not a plain one is never put in a command the user would
// paste (0067 block 4 review rules).
func stopCommands(ps string) (stop, start string) {
	for _, line := range strings.Split(ps, "\n") {
		project, name, _ := strings.Cut(strings.TrimRight(line, " \r"), "\t")
		switch {
		case project != "":
			if composeProject.MatchString(project) {
				return "docker compose -p " + project + " stop", "docker compose -p " + project + " start"
			}
		case name != "":
			if containerName.MatchString(name) {
				return "docker stop " + name, "docker start " + name
			}
		}
	}
	return "", ""
}

var (
	// composeProject is a name Compose accepts for a project.
	composeProject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// containerName is a name Docker accepts for a container.
	containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)
