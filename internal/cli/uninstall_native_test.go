package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/testtool"
)

// guardWatch records, at each disable of the native units, whether the
// db unit's guard (the shim its ExecStartPre runs) and the native files
// were still on disk.
type guardWatch struct {
	mu      sync.Mutex
	inner   server.SysRunner
	n       server.Native
	disable []string
}

func (g *guardWatch) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	if strings.Contains(line, "disable --now") {
		g.mu.Lock()
		g.disable = append(g.disable, fmt.Sprintf("%s [shim %v, root %v, home %v, data %v]", line,
			onDisk(filepath.Join(g.n.Dirs.Data, "bin", "current")), onDisk(g.n.Root()), onDisk(g.n.Home()), onDisk(g.n.DataDir())))
		g.mu.Unlock()
	}
	return g.inner.Run(ctx, name, args...)
}

// nativeUninstallHost is a running native install with a backup on disk,
// whose user manager calls are watched.
func nativeUninstallHost(t *testing.T) (n server.Native, watch *guardWatch, archive string) {
	t.Helper()
	home, n, _, sys, _ := nativeHost(t)
	watch = &guardWatch{inner: sys, n: n}
	old := nativeSys
	nativeSys = watch
	t.Cleanup(func() { nativeSys = old })
	archive = filepath.Join(pathsOf(home).Data, "backups", "backsight-20261025T040000Z.tar.gz.age")
	_ = os.MkdirAll(filepath.Dir(archive), 0o700)
	if err := os.WriteFile(archive, []byte("age-encrypted archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !onDisk(filepath.Join(n.Dirs.Data, "bin", "current")) {
		t.Fatal("the fixture has no shim")
	}
	return n, watch, archive
}

// Review Focus 5: the db unit's guard is the shim uninstall removes. A
// plain uninstall stops and disables both units first, its data kept, and
// its plan says so; the JSON still says the server was kept.
func TestUninstallOnNativeNeverLeavesAUnitWithoutItsGuard(t *testing.T) {
	testtool.SkipWindows(t, "Task 13: uninstall (the PATH link is a copy on Windows, Task 4)")
	n, watch, archive := nativeUninstallHost(t)

	code, out, errOut := run(t, "uninstall", "--plan")
	if code != output.OK || !strings.Contains(out, "stopped and disabled: its guard is backsight, which this uninstall removes; its data is kept in "+n.Home()) {
		t.Fatalf("plan: exit %d\n%s%s", code, out, errOut)
	}
	if len(watch.disable) > 0 {
		t.Fatalf("the plan disabled units: %q", watch.disable)
	}

	code, env, raw := uninstall(t)
	if code != output.OK || env.Data.Server != "keep" {
		t.Fatalf("exit %d\n%s", code, raw)
	}
	if s := stepNamed(env.Data, "server"); s == nil || s.Status != "done" || !strings.Contains(s.Detail, "stopped and disabled") || !strings.Contains(s.Detail, n.Home()) {
		t.Errorf("server step %+v", s)
	}
	want := fmt.Sprintf("systemctl --user disable --now %s %s [shim true, root true, home true, data true]", n.ServerUnit(), n.DBUnit())
	if len(watch.disable) != 1 || watch.disable[0] != want {
		t.Errorf("disables %q\nwant %q", watch.disable, want)
	}
	if onDisk(filepath.Join(n.Dirs.Data, "bin", "current")) {
		t.Error("the shim is still there")
	}
	if !onDisk(filepath.Join(n.DataDir(), "PG_VERSION")) || !onDisk(n.IdentityFile()) || !onDisk(n.EnvFile()) {
		t.Error("a plain uninstall removed the native data")
	}
	if got, _ := os.ReadFile(archive); string(got) != "age-encrypted archive" {
		t.Errorf("backup changed: %q", got)
	}
	if strings.Contains(raw, filepath.Join(n.Cfg.Server.ComposeDir, ".env")) {
		t.Errorf("uninstall names Docker's .env on native:\n%s", raw)
	}
}

// --delete-data stops and disables both units before anything under
// Root() goes; backups stay.
func TestUninstallDeleteDataOnNativeStopsBeforeRemoving(t *testing.T) {
	testtool.SkipWindows(t, "Task 13: uninstall (the PATH link is a copy on Windows, Task 4)")
	n, watch, archive := nativeUninstallHost(t)
	code, env, raw := uninstall(t, "--delete-data", "--answer", "confirm_bank="+n.Cfg.Server.Bank)
	if code != output.OK || env.Data.Server != "delete-data" {
		t.Fatalf("exit %d\n%s", code, raw)
	}
	want := fmt.Sprintf("systemctl --user disable --now %s %s [shim true, root true, home true, data true]", n.ServerUnit(), n.DBUnit())
	if len(watch.disable) != 1 || watch.disable[0] != want {
		t.Errorf("disables %q\nwant %q", watch.disable, want)
	}
	if onDisk(n.Root()) {
		t.Error("Root() kept")
	}
	if got, _ := os.ReadFile(archive); string(got) != "age-encrypted archive" {
		t.Errorf("backup changed: %q", got)
	}
	for _, s := range env.Data.NextSteps {
		if strings.Contains(s, "images") {
			t.Errorf("a native uninstall talks of images it already removed: %q", s)
		}
	}
}

// --purge-local over a kept native server names the file its password
// lives in: server.env, not Docker's .env.
func TestUninstallPurgeLocalOnNativeNamesServerEnv(t *testing.T) {
	testtool.SkipWindows(t, "Task 13: uninstall (the PATH link is a copy on Windows, Task 4)")
	n, _, _ := nativeUninstallHost(t)
	code, env, raw := uninstall(t, "--purge-local", "--answer", "identity_saved=yes", "--answer", "pre_images_ack=yes", "--non-interactive")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, raw)
	}
	found := false
	for _, w := range env.Data.Warnings {
		found = found || strings.Contains(w, "lives only in "+n.EnvFile())
	}
	if !found || strings.Contains(raw, filepath.Join(n.Cfg.Server.ComposeDir, ".env")) {
		t.Errorf("warnings %q", env.Data.Warnings)
	}
}
