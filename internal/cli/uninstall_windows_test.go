//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/supervise"
	"github.com/Barben360/backsight/internal/testtool"
)

// Uninstall on Windows (decision 0080, Task 13): the server's task and
// the supervisor, the jobs' tasks, their folder, the user PATH entry,
// then the files, with what a running program holds moved to the trash
// for a detached cleaner. Task Scheduler and the pipe are faked; the
// user PATH is TestMain's throwaway key.

// winEvents is what uninstall did across the fakes, in order.
type winEvents struct {
	mu   sync.Mutex
	list []string
}

func (e *winEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, s)
}

func (e *winEvents) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.list)
}

// at is the index of the first event that starts with prefix (-1: none).
func (e *winEvents) at(prefix string) int {
	return slices.IndexFunc(e.all(), func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// winTasks is Task Scheduler (the server's task through nativeSys, the
// jobs' through sysRunner) and the supervisor's pipe.
type winTasks struct {
	ev         *winEvents
	mu         sync.Mutex
	registered map[string]bool // task name, without its leading \
	up         bool            // the supervisor answers
	folderErr  error           // what deleting the folder answers
	folders    []string        // the folders deleted
}

func (w *winTasks) schtasks(line string, args []string) ([]byte, error) {
	w.ev.add(line)
	w.mu.Lock()
	defer w.mu.Unlock()
	name := ""
	if i := slices.Index(args, "/TN"); i >= 0 && i+1 < len(args) {
		name = strings.TrimPrefix(args[i+1], `\`)
	}
	notFound := func() ([]byte, error) {
		return []byte("ERROR: The system cannot find the file specified."), errors.New("exit status 1")
	}
	switch {
	case slices.Contains(args, "/Create"):
		w.registered[name] = true
	case !w.registered[name]:
		return notFound()
	case slices.Contains(args, "/Delete"):
		delete(w.registered, name)
	}
	return nil, nil
}

func (w *winTasks) left() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for n := range w.registered {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

type winServerSys struct{ w *winTasks }

func (s winServerSys) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return s.w.schtasks(name+" "+strings.Join(args, " "), args)
}

type winJobsSys struct {
	*fakeSys
	w *winTasks
}

func (s winJobsSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name != "schtasks" {
		return s.fakeSys.Run(ctx, name, args...)
	}
	return s.w.schtasks(name+" "+strings.Join(args, " "), args)
}

// windowsUninstallHost is a whole Windows install: client install's shim,
// its PATH copy and user PATH entry, the jobs' tasks, and a native server
// whose task is registered and whose supervisor answers.
func windowsUninstallHost(t *testing.T) (home string, d paths.Dirs, w *winTasks) {
	t.Helper()
	home = devInstall(t, true)
	d = pathsOf(home)
	jobs := withSys(t, "windows")
	w = &winTasks{ev: &winEvents{}, registered: map[string]bool{}}
	oldR := sysRunner
	sysRunner = winJobsSys{fakeSys: jobs, w: w}
	t.Cleanup(func() { sysRunner = oldR })
	installedOn(t, home, "0.5.0")
	if code, out, errOut := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("jobs install: exit %d\n%s%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(d.ConfigFile())
	raw, _ = config.SetRuntime(raw, "native")
	if err := os.WriteFile(d.ConfigFile(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(d.Data, "server", "native")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"supervise.json": `{"venv": "1.1.0-abcdefabcdef"}`, "task.xml": "<Task/>"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.registered[`backsight\server`], w.up = true, true
	oldS, oldA, oldF := nativeSys, nativeAsk, deleteTaskFolder
	nativeSys = winServerSys{w}
	nativeAsk = func(_ context.Context, project, op string) (supervise.Status, error) {
		w.ev.add("pipe " + project + " " + op)
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.up {
			return supervise.Status{}, supervise.ErrNotRunning
		}
		if op == "stop" {
			w.up = false
		}
		return supervise.Status{Proto: supervise.Proto, Version: "0.5.0", PID: 4242, DB: "running", API: "running"}, nil
	}
	deleteTaskFolder = func(name string) (bool, error) {
		w.ev.add("folder " + name)
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.folderErr != nil {
			return false, w.folderErr
		}
		w.folders = append(w.folders, name)
		return true, nil
	}
	t.Cleanup(func() { nativeSys, nativeAsk, deleteTaskFolder = oldS, oldA, oldF })
	if left := w.left(); !slices.Equal(left, []string{`backsight\maintain`, `backsight\server`, `backsight\tick`}) {
		t.Fatalf("the fixture's tasks: %q", left)
	}
	if has, err := userPathHas(d.Bin); err != nil || !has {
		t.Fatalf("client install did not put %s on the user PATH (%v)", d.Bin, err)
	}
	w.ev.list = nil
	return home, d, w
}

// The order on Windows: the jobs' tasks, then the server's (disabled,
// its supervisor stopped and gone, then deleted), then their folder once
// both are gone; nothing registered, nothing running, the server's data
// kept. A plan changes none of it.
func TestUninstallLeavesNoTaskAndNoProcess(t *testing.T) {
	_, d, w := windowsUninstallHost(t)
	root := filepath.Join(d.Data, "server", "native")

	code, env, out := uninstall(t, "--plan")
	if code != output.OK || !w.up || len(w.left()) != 3 || len(w.folders) != 0 {
		t.Fatalf("plan: exit %d, supervisor up %v, tasks %q, folders %q\n%s", code, w.up, w.left(), w.folders, out)
	}
	for _, e := range w.ev.all() {
		if strings.Contains(e, "/Delete") || strings.Contains(e, "/Change") || strings.HasSuffix(e, " stop") {
			t.Errorf("the plan ran %q", e)
		}
	}
	if s := stepNamed(env.Data, "task folder"); s == nil || s.Status != "planned" || !strings.Contains(s.Detail, `\backsight`) {
		t.Errorf("plan's task folder step %+v", s)
	}
	if s := stepNamed(env.Data, "server"); s == nil || s.Status != "planned" || !strings.Contains(s.Detail, `\backsight\server deleted`) {
		t.Errorf("plan's server step %+v", s)
	}

	w.ev.list = nil
	code, env, out = uninstall(t)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	ev := w.ev.all()
	tick, maintain := w.ev.at(`schtasks /Delete /F /TN backsight\tick`), w.ev.at(`schtasks /Delete /F /TN backsight\maintain`)
	disable, stop := w.ev.at(`schtasks /Change /TN \backsight\server /DISABLE`), w.ev.at("pipe backsight stop")
	gone, server, folder := w.ev.at("pipe backsight status"), w.ev.at(`schtasks /Delete /F /TN \backsight\server`), w.ev.at("folder backsight")
	switch {
	case tick < 0 || maintain < 0 || disable < 0 || stop < 0 || gone < 0 || server < 0 || folder < 0:
		t.Fatalf("missing steps:\n%s", strings.Join(ev, "\n"))
	case !(disable < stop && stop < gone && gone < server):
		t.Errorf("the task must be disabled, then the supervisor stopped and gone, then the task deleted:\n%s", strings.Join(ev, "\n"))
	case folder < max(tick, maintain, server):
		t.Errorf("the folder went before its tasks:\n%s", strings.Join(ev, "\n"))
	}
	if left := w.left(); len(left) != 0 || w.up || !slices.Equal(w.folders, []string{"backsight"}) {
		t.Errorf("left: tasks %q, supervisor up %v, folders deleted %q", left, w.up, w.folders)
	}
	if s := stepNamed(env.Data, "task folder"); s == nil || s.Status != "done" {
		t.Errorf("task folder step %+v", s)
	}
	if s := stepNamed(env.Data, "server"); s == nil || s.Status != "done" || !strings.Contains(s.Detail, `\backsight\server deleted`) || !strings.Contains(s.Detail, root) {
		t.Errorf("server step %+v", s)
	}
	for _, f := range []string{"supervise.json", "task.xml"} {
		if !onDisk(filepath.Join(root, f)) {
			t.Errorf("a plain uninstall removed the kept server's %s", f)
		}
	}
	if len(env.Data.Trash) != 0 || onDisk(filepath.Join(d.Data, "trash")) {
		t.Errorf("nothing was held, yet a trash: %q", env.Data.Trash)
	}
	if onDisk(filepath.Join(d.Data, "bin")) {
		t.Error("the shim and the binaries stay")
	}
	if onDisk(filepath.Join(d.State, "tasks")) { // the jobs' task files' directories, once empty
		t.Errorf("%s stays", filepath.Join(d.State, "tasks"))
	}
}

// A folder Task Scheduler refuses (a task is still in it) stays, named in
// a warning; the uninstall goes on.
func TestUninstallKeepsATaskFolderThatIsNotEmpty(t *testing.T) {
	_, _, w := windowsUninstallHost(t)
	w.folderErr = syscall.Errno(145) // ERROR_DIR_NOT_EMPTY
	code, env, out := uninstall(t)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if s := stepNamed(env.Data, "task folder"); s == nil || s.Status != "kept" {
		t.Errorf("task folder step %+v", s)
	}
	if !slices.ContainsFunc(env.Data.Warnings, func(s string) bool { return strings.Contains(s, `folder \backsight stays`) }) {
		t.Errorf("no warning names the folder: %q", env.Data.Warnings)
	}
}

// --stop and --delete-data delete the server's task too: it would run a
// shim that is gone.
func TestUninstallStopAndDeleteDataDeleteTheTask(t *testing.T) {
	for _, args := range [][]string{{"--stop"}, {"--delete-data", "--answer", "confirm_bank=claude-memory"}} {
		t.Run(args[0], func(t *testing.T) {
			_, d, w := windowsUninstallHost(t)
			code, _, out := uninstall(t, args...)
			if code != output.OK {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if left := w.left(); len(left) != 0 || w.up || len(w.folders) != 1 {
				t.Errorf("left: tasks %q, supervisor up %v, folders %q", left, w.up, w.folders)
			}
			if kept := onDisk(filepath.Join(d.Data, "server", "native")); kept == (args[0] == "--delete-data") {
				t.Errorf("%s: the native root kept %v", args[0], kept)
			}
		})
	}
}

// userPathValue is the throwaway user PATH key's Path value and type.
func userPathValue(t *testing.T) (string, uint32) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, userPathKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	v, typ, err := k.GetStringValue("Path")
	if err != nil {
		t.Fatal(err)
	}
	return v, typ
}

func setUserPathValue(t *testing.T, v string) {
	t.Helper()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, userPathKey, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	if err := k.SetExpandStringValue("Path", v); err != nil {
		t.Fatal(err)
	}
}

// Uninstall removes <bin> from the user PATH and no other entry, a
// look-alike and an expanded %VAR% entry included, keeps the value's
// REG_EXPAND_SZ type, and removes the PATH command and its directory.
func TestUninstallRemovesOnlyItsPathEntry(t *testing.T) {
	_, d, _ := windowsUninstallHost(t)
	others := []string{`%USERPROFILE%\tools`, d.Bin + "-other", `C:\Program Files\Git\cmd`}
	// The signed checksums the PATH copy carries once it is a release's.
	for _, name := range []string{"checksums.txt", "checksums.txt.minisig"} {
		if err := os.WriteFile(filepath.Join(d.Bin, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// <bin> twice: as written by setup (case and a trailing \ aside) and
	// through a variable, as a user may have written it: both are <bin>.
	t.Setenv("BSTEST_BIN_PARENT", filepath.Dir(d.Bin))
	setUserPathValue(t, others[0]+";"+strings.ToUpper(d.Bin)+`\;`+others[1]+`;%BSTEST_BIN_PARENT%\`+filepath.Base(d.Bin)+";"+others[2])

	if code, _, out := uninstall(t, "--plan"); code != output.OK {
		t.Fatalf("plan: exit %d\n%s", code, out)
	}
	if v, _ := userPathValue(t); !strings.Contains(strings.ToLower(v), strings.ToLower(d.Bin)+`\;`) {
		t.Errorf("the plan changed the user PATH: %q", v)
	}

	code, env, out := uninstall(t)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	v, typ := userPathValue(t)
	if v != strings.Join(others, ";") || typ != registry.EXPAND_SZ {
		t.Errorf("user PATH %q (type %d), want %q (REG_EXPAND_SZ)", v, typ, strings.Join(others, ";"))
	}
	if s := stepNamed(env.Data, "user path"); s == nil || s.Status != "done" || !strings.Contains(s.Detail, d.Bin) {
		t.Errorf("user path step %+v", s)
	}
	// The order: the tasks and their folder, then the PATH entry, then
	// every file.
	at := func(name string) int {
		return slices.IndexFunc(env.Data.Steps, func(a undoAction) bool { return a.Name == name })
	}
	if f, p, l, b := at("task folder"), at("user path"), at("local files"), at("binaries"); f < 0 || !(f < p && p < l && l < b) {
		t.Errorf("steps task folder %d, user path %d, local files %d, binaries %d: want that order", f, p, l, b)
	}
	if onDisk(pathCommand(d.Bin)) || onDisk(d.Bin) {
		t.Errorf("the PATH command or its directory %s stays", d.Bin)
	}
}

// The backsight.exe.old-<n> copies a swap of the PATH command left
// (setup run from that command renames it aside: pointShimWindows) go
// with it, and <bin> with them.
func TestUninstallRemovesThePathCommandsOldCopies(t *testing.T) {
	_, d, _ := windowsUninstallHost(t)
	link := pathCommand(d.Bin)
	olds := []string{link + ".old-1", link + ".old-3"}
	for _, p := range olds {
		if err := os.WriteFile(p, []byte("MZ an older copy"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	code, env, out := uninstall(t, "--plan")
	if s := stepNamed(env.Data, "binaries"); code != output.OK || s == nil || !strings.Contains(s.Detail, olds[1]) {
		t.Errorf("the plan does not name %s: exit %d\n%s", olds[1], code, out)
	}
	if !onDisk(olds[0]) {
		t.Fatal("the plan removed a copy")
	}
	if code, _, out := uninstall(t); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, p := range append(olds, link, d.Bin) {
		if onDisk(p) {
			t.Errorf("%s stays", p)
		}
	}
}

// A file in <bin> that is neither the PATH command, its signed checksums
// nor one of its .old-<n> copies stays, and <bin> with it.
func TestUninstallKeepsAnotherFileBesideThePathCommand(t *testing.T) {
	_, d, _ := windowsUninstallHost(t)
	other := []string{filepath.Join(d.Bin, "notes.txt"), pathCommand(d.Bin) + ".old-x"}
	for _, p := range other {
		if err := os.WriteFile(p, []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, out := uninstall(t); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, p := range other {
		if !onDisk(p) {
			t.Errorf("%s was removed", p)
		}
	}
	if onDisk(pathCommand(d.Bin)) {
		t.Error("the PATH command stays")
	}
}

// A <bin>\backsight.exe that is not a file (not backsight's copy) stays,
// and <bin> stays on the user PATH with it.
func TestUninstallKeepsAPathEntryWhoseCommandIsNotOurs(t *testing.T) {
	_, d, _ := windowsUninstallHost(t)
	link := pathCommand(d.Bin)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(link, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, env, out := uninstall(t)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if has, _ := userPathHas(d.Bin); !has || !onDisk(filepath.Join(link, "inside")) {
		t.Errorf("on the PATH %v, kept %v", has, onDisk(filepath.Join(link, "inside")))
	}
	if s := stepNamed(env.Data, "user path"); s == nil || s.Status != "kept" {
		t.Errorf("user path step %+v", s)
	}
	if !slices.ContainsFunc(env.Data.Warnings, func(s string) bool { return strings.Contains(s, link+" is not backsight's") }) {
		t.Errorf("no warning names %s: %q", link, env.Data.Warnings)
	}
}

// holdShim makes current.exe a real program that runs, as a Claude Code
// session's hooks run it: Windows then refuses to delete it, not to
// rename it. release kills it.
func holdShim(t *testing.T, d paths.Dirs) (release func()) {
	t.Helper()
	shim := filepath.Join(d.Data, "bin", plugin.ShimName("windows"))
	testtool.Write(t, filepath.Dir(shim), "current", testtool.Fake{Script: "sleep 600000", NoLog: true})
	cmd := exec.Command(shim)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		})
	}
	t.Cleanup(release)
	// Running: its image cannot be deleted.
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(shim, os.O_WRONLY, 0)
		if err != nil {
			break // the running image refuses a writer
		}
		_ = f.Close()
		if time.Now().After(deadline) {
			t.Fatal("the held shim never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return release
}

// waitGone waits up to within for p to be gone.
func waitGone(p string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for onDisk(p) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// Review Focus 3, the uninstall side: with a session running current.exe,
// the shim goes to <data>\trash, the cleaner is started on that trash and
// the result names it; once the session ends, the cleaner (its loop
// shortened) empties the trash within 10 s.
func TestUninstallWithTheShimHeldOpen(t *testing.T) {
	_, d, _ := windowsUninstallHost(t)
	release := holdShim(t, d)
	oldT, oldP, oldC := cleanerTries, cleanerPings, startCleaner
	cleanerTries, cleanerPings = 20, 2 // every second, for 20 s
	var started []string
	startCleaner = func(trash string) error { started = append(started, trash); return startTrashCleaner(trash) }
	t.Cleanup(func() { cleanerTries, cleanerPings, startCleaner = oldT, oldP, oldC })
	trash := filepath.Join(d.Data, "trash")

	code, env, out := uninstall(t)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if want := []string{filepath.Join(trash, "1-current.exe")}; !slices.Equal(env.Data.Trash, want) || !onDisk(want[0]) {
		t.Fatalf("trash %q, want %q (on disk %v)\n%s", env.Data.Trash, want, onDisk(want[0]), out)
	}
	if !slices.Equal(started, []string{trash}) {
		t.Errorf("cleaner started on %q, want %s once", started, trash)
	}
	if !slices.ContainsFunc(env.Data.NextSteps, func(s string) bool { return strings.Contains(s, "close Claude Code, then delete "+trash) }) {
		t.Errorf("the result does not name the trash: %q", env.Data.NextSteps)
	}
	if onDisk(filepath.Join(d.Data, "bin")) || onDisk(pathCommand(d.Bin)) {
		t.Error("what was not held stays")
	}

	release()
	if !waitGone(trash, 10*time.Second) {
		left, _ := os.ReadDir(trash)
		t.Errorf("the cleaner left %s after the session ended: %v", trash, left)
	}
}

// A file held so that it can be neither deleted nor renamed (open without
// FILE_SHARE_DELETE) is not lost: the uninstall stops, naming what is
// left to remove by hand, and no cleaner starts.
func TestUninstallNamesAFileItCanNeitherDeleteNorMove(t *testing.T) {
	_, d, _ := windowsUninstallHost(t)
	held := filepath.Join(d.Data, "marketplace", ".claude-plugin", "marketplace.json")
	f, err := os.Open(held) // Go opens without FILE_SHARE_DELETE
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	code, env, out := uninstall(t)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "by hand") ||
		!strings.Contains(env.Error.Message, filepath.Join(d.Data, "marketplace")) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if s := stepNamed(env.Data, "binaries"); s == nil || s.Status != "failed" || !strings.Contains(s.Detail, "cannot be moved") {
		t.Errorf("binaries step %+v", s)
	}
}

// Review Focus 4: the trash path reaches cmd.exe through BACKSIGHT_TRASH
// only, never its command line, so a path with & and % is never parsed
// as cmd.exe syntax; the cleaner runs without a window, out of the
// caller's job, from System32; for real it deletes that trash, then the
// data root once empty, but never a data root that still holds something.
func TestCleanerGetsTheTrashPathThroughItsEnvironment(t *testing.T) {
	oldT, oldP := cleanerTries, cleanerPings
	cleanerTries, cleanerPings = 20, 2
	t.Cleanup(func() { cleanerTries, cleanerPings = oldT, oldP })
	base := t.TempDir()
	for _, kept := range []bool{false, true} {
		data := filepath.Join(base, "Jé & Co! 100%PATH% x%", map[bool]string{false: "data", true: "data-kept"}[kept])
		trash := filepath.Join(data, "trash")
		for _, p := range []string{filepath.Join(trash, "1-current.exe"), filepath.Join(trash, "2-dir & more", "f%PATH%.txt")} {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if kept {
			if err := os.MkdirAll(filepath.Join(data, "backups"), 0o700); err != nil {
				t.Fatal(err)
			}
		}

		cmd := cleanerCommand(trash)
		line := cmd.SysProcAttr.CmdLine
		for _, part := range []string{base, "Jé", "Co! 100", "trash", data} {
			if strings.Contains(line, part) {
				t.Errorf("the command line holds %q: %s", part, line)
			}
		}
		if !strings.HasPrefix(line, "cmd.exe /d /q /v:off /c ") || !strings.Contains(line, `rmdir /s /q "%BACKSIGHT_TRASH%"`) || !strings.Contains(line, "ping -n 2 127.0.0.1 >nul") {
			t.Errorf("command line %s", line)
		}
		if !slices.Contains(cmd.Env, "BACKSIGHT_TRASH="+trash) {
			t.Errorf("environment %q", cmd.Env)
		}
		sys32 := filepath.Join(os.Getenv("SystemRoot"), "System32")
		if !strings.EqualFold(cmd.Path, filepath.Join(sys32, "cmd.exe")) || !strings.EqualFold(cmd.Dir, sys32) {
			t.Errorf("cmd %s in %s", cmd.Path, cmd.Dir)
		}
		if fl := cmd.SysProcAttr.CreationFlags; fl&windows.CREATE_NO_WINDOW == 0 || fl&windows.CREATE_BREAKAWAY_FROM_JOB == 0 {
			t.Errorf("creation flags %#x", fl)
		}

		if err := startTrashCleaner(trash); err != nil {
			t.Fatal(err)
		}
		if !waitGone(trash, 10*time.Second) {
			t.Fatalf("the cleaner left %s", trash)
		}
		if kept {
			time.Sleep(2 * time.Second) // its last step, the data root's rmdir, has run
			if !onDisk(filepath.Join(data, "backups")) {
				t.Errorf("a data root that holds backups lost them")
			}
		} else if !waitGone(data, 5*time.Second) {
			t.Errorf("the empty data root %s stays", data)
		}
	}
}
