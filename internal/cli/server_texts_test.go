package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/supervise"
)

// nativeOn makes the runtime of a native config.toml the native one laid
// out for goos (the task host on "windows"), Task Scheduler and the pipe
// faked: every schtasks call succeeds, no supervisor answers.
func nativeOn(t *testing.T, home, goos string) *[]string {
	t.Helper()
	d := pathsOf(home)
	raw, _ := os.ReadFile(d.ConfigFile())
	raw, _ = config.SetRuntime(raw, "native")
	if err := os.WriteFile(d.ConfigFile(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := &[]string{}
	old := runtimeWith
	runtimeWith = func(cfg config.Config, _ bool) server.Runtime {
		return server.Native{Cfg: cfg, Dirs: d, GOOS: goos, UnitDir: "/u",
			Sys: sysFunc(func(name string, args ...string) ([]byte, error) {
				*calls = append(*calls, name+" "+strings.Join(args, " "))
				return nil, nil
			}),
			Ask: func(context.Context, string, string) (supervise.Status, error) {
				return supervise.Status{}, supervise.ErrNotRunning
			}}
	}
	t.Cleanup(func() { runtimeWith = old })
	return calls
}

type sysFunc func(name string, args ...string) ([]byte, error)

func (f sysFunc) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return f(name, args...)
}

// server stop on Windows says it disabled the scheduled task, planned or
// done; on Linux its words are the ones it always printed.
func TestServerStopNamesTheTaskOnWindows(t *testing.T) {
	home := devInstall(t, true)
	calls := nativeOn(t, home, "windows")
	_, out, _ := run(t, "server", "stop", "--plan")
	if !strings.Contains(out, `would disable the scheduled task \backsight\server and stop the native server (data kept)`) || strings.Contains(out, "containers") {
		t.Errorf("plan:\n%s", out)
	}
	code, out, errOut := run(t, "server", "stop")
	if code != output.OK || !strings.Contains(out, `the scheduled task \backsight\server stays disabled`) {
		t.Errorf("stop: exit %d\n%s%s", code, out, errOut)
	}
	if len(*calls) == 0 || !strings.Contains((*calls)[0], `schtasks /Change /TN \backsight\server /DISABLE`) {
		t.Errorf("schtasks calls %q", *calls)
	}
	nativeOn(t, home, "linux")
	if _, out, _ := run(t, "server", "stop", "--plan"); out != "would stop the containers of project backsight (data kept)\n" {
		t.Errorf("linux plan: %q", out)
	}
}

// A server that never reports health names where to look: the
// supervisor's logs on Windows, journalctl on Linux.
func TestWaitHealthyNamesTheLogsPerOS(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatal(err)
	}
	old := healthTimeout
	healthTimeout = 50 * time.Millisecond
	t.Cleanup(func() { healthTimeout = old })
	for goos, want := range map[string]string{"windows": "backsight-supervisor.log", "linux": "journalctl --user -u backsight-server.service"} {
		n := server.Native{Cfg: cfg, Dirs: d, GOOS: goos, UnitDir: "/u",
			Sys: sysFunc(func(string, ...string) ([]byte, error) { return nil, context.Canceled }),
			Ask: func(context.Context, string, string) (supervise.Status, error) {
				return supervise.Status{}, supervise.ErrNotRunning
			}}
		_, err := waitHealthy(context.Background(), n, cfg, config.Secrets{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", goos, err, want)
		}
		if goos == "windows" && err != nil && (strings.Contains(err.Error(), "journalctl") || strings.Contains(err.Error(), ".service")) {
			t.Errorf("windows names systemd: %v", err)
		}
	}
}
