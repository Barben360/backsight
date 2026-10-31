//go:build windows

package supervise

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Barben360/backsight/internal/testtool"
)

// The product's supervisor runs under conhost --headless, a console its
// children share: CTRL_BREAK reaches them. A test run over SSH or by CI
// may have none; it gets one (hidden from no one: session 0 or the
// runner's), so the interrupt path is the one tested. Go's os.Stdout keeps
// the handle it started with: the test output still goes to go test.
func init() {
	beforeTests = func() {
		if _, err := windows.GetConsoleCP(); err == nil {
			return
		}
		if r, _, err := kernel32.NewProc("AllocConsole").Call(); r == 0 {
			fmt.Fprintln(os.Stderr, "supervise tests: no console, and AllocConsole failed:", err)
		}
	}
}

// pids reads the PIDs a fake's pid step wrote.
func pids(t *testing.T, path string) []int {
	t.Helper()
	raw, _ := os.ReadFile(path)
	var out []int
	for _, f := range strings.Fields(string(raw)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// gone waits up to within for pid to exit; true when it has (or no such
// process remains).
func gone(pid int, within time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ev, _ := windows.WaitForSingleObject(h, uint32(max(within, 0)/time.Millisecond))
	return ev == windows.WAIT_OBJECT_0
}

func TestKillingTheSupervisorKillsItsChildren(t *testing.T) {
	x := newFx(t, "kill", fakes{})
	raw, err := json.Marshal(x.cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := x.file("supervise.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sup := exec.Command(exe)
	sup.Env = append(os.Environ(), superviseEnv+"="+cfgPath)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Process.Kill(); _ = sup.Wait() })
	st := waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	if st.PID != sup.Process.Pid {
		t.Fatalf("the answering supervisor is pid %d, want the subprocess %d", st.PID, sup.Process.Pid)
	}
	waitFor(t, "both children's pids", func() bool {
		return len(pids(t, x.file("pg.pids"))) == 1 && len(pids(t, x.file("api.pids"))) == 1
	})
	children := append(pids(t, x.file("pg.pids")), pids(t, x.file("api.pids"))...)
	if len(children) != 2 {
		t.Fatalf("children %v, want postgres' and the API's", children)
	}
	for _, pid := range children {
		if gone(pid, 0) {
			t.Fatalf("child %d not running before the kill", pid)
		}
	}
	if err := sup.Process.Kill(); err != nil { // TerminateProcess
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for _, pid := range children {
		if !gone(pid, time.Until(deadline)) {
			t.Errorf("child %d still runs 2 s after the supervisor was killed", pid)
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	}
}

func TestPipeRefusesAnotherUser(t *testing.T) {
	fast(t)
	x := newFx(t, "pipe", fakes{})
	x.start(Options{})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	name, err := pipeName(x.cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("the pipe's DACL is not protected (control %#x, %v): it would inherit entries", control, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("the pipe's DACL: %v %v", dacl, err)
	}
	me, err := userSID()
	if err != nil {
		t.Fatal(err)
	}
	if dacl.AceCount != 1 {
		t.Fatalf("the pipe's DACL has %d entries (%s), want the user's alone", dacl.AceCount, sd)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || sid.String() != me {
		t.Errorf("the pipe's one entry is type %d for %s, want an allow for %s", ace.Header.AceType, sid, me)
	}
	// Nobody else serves the name while the supervisor has it.
	if l, err := listen(x.cfg.Project); err == nil {
		_ = l.Close()
		t.Error("a second server opened the supervisor's pipe: FILE_FLAG_FIRST_PIPE_INSTANCE missing")
	}
	if st, err := ask(x.cfg.Project, opStatus); err != nil || st.DB != stateRunning {
		t.Errorf("the supervisor after the checks: %+v %v", st, err)
	}
}

// tokenOf is the open token of pid.
func tokenOf(t *testing.T, pid int) windows.Token {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tok.Close() })
	return tok
}

// adminsAttrs is the Administrators group's attributes in tok.
func adminsAttrs(t *testing.T, tok windows.Token) (uint32, bool) {
	t.Helper()
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := tok.GetTokenGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups.AllGroups() {
		if g.Sid.Equals(admins) {
			return g.Attributes, true
		}
	}
	return 0, false
}

// defaultDACLGrants: tok's default DACL has an allow entry for sid.
func defaultDACLGrants(t *testing.T, tok windows.Token, sid string) bool {
	t.Helper()
	n := uint32(0)
	_ = windows.GetTokenInformation(tok, windows.TokenDefaultDacl, nil, 0, &n)
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenDefaultDacl, &buf[0], n, &n); err != nil {
		t.Fatal(err)
	}
	acl := (*tokenDefaultDacl)(unsafe.Pointer(&buf[0])).DefaultDacl
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil {
			continue
		}
		if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String() == sid {
			return true
		}
	}
	return false
}

func TestPostgresGetsARestrictedTokenWhenAdmin(t *testing.T) {
	if !isAdmin() {
		if os.Getenv("CI") != "" {
			t.Fatal("the CI runner is not an administrator: this test must run there")
		}
		t.Skip("not an administrator: CI's runner runs this test")
	}
	fast(t)
	x := newFx(t, "token", fakes{})
	x.start(Options{})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	waitFor(t, "both children's pids", func() bool {
		return len(pids(t, x.file("pg.pids"))) == 1 && len(pids(t, x.file("api.pids"))) == 1
	})
	pg, api := pids(t, x.file("pg.pids")), pids(t, x.file("api.pids"))
	if len(pg) != 1 || len(api) != 1 {
		t.Fatalf("pids: postgres %v, api %v", pg, api)
	}
	pgTok := tokenOf(t, pg[0])
	attrs, ok := adminsAttrs(t, pgTok)
	if !ok || attrs&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 || attrs&windows.SE_GROUP_ENABLED != 0 {
		t.Errorf("postgres' token: Administrators attributes %#x (present %v), want deny-only", attrs, ok)
	}
	me, err := userSID()
	if err != nil {
		t.Fatal(err)
	}
	if !defaultDACLGrants(t, pgTok, me) {
		t.Error("postgres' token: the default DACL does not grant the user (pg_ctl's recipe adds it)")
	}
	// The API keeps the supervisor's token: only postgres needs the
	// restriction.
	if attrs, ok := adminsAttrs(t, tokenOf(t, api[0])); !ok || attrs&windows.SE_GROUP_ENABLED == 0 {
		t.Errorf("the API's token: Administrators attributes %#x (present %v), want enabled", attrs, ok)
	}
}

// Clients that open the pipe and close it at once, some before the
// supervisor accepts them (ERROR_NO_DATA), cost nothing: the pipe still
// answers.
func TestPipeSurvivesClientsThatComeAndGo(t *testing.T) {
	fast(t)
	x := newFx(t, "churn", fakes{})
	x.start(Options{})
	waitStatus(t, x.cfg.Project, stateRunning, stateRunning)
	name, err := pipeName(x.cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	for i := range 500 {
		var h windows.Handle
		for deadline := time.Now().Add(5 * time.Second); ; {
			h, err = windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
			if !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
				break
			}
			_, _, _ = procWaitNamedPipe.Call(uintptr(unsafe.Pointer(p)), 50) // every instance busy: wait for one
		}
		if err != nil {
			t.Fatalf("open %d: %v (the pipe is gone)", i, err)
		}
		opened++
		if i%5 == 0 {
			var n uint32
			_ = windows.WriteFile(h, []byte("garbage"), &n, nil) // half a request
		}
		_ = windows.CloseHandle(h)
	}
	for range 3 {
		if st, err := ask(x.cfg.Project, opStatus); err != nil || st.DB != stateRunning {
			t.Fatalf("status after %d clients came and went: %+v %v", opened, st, err)
		}
	}
}

// A kill ends the child's whole tree: what it started dies with it (the
// venv launcher's python, postgres' backends).
func TestKillEndsTheChildsTree(t *testing.T) {
	dir := t.TempDir()
	gc := filepath.Join(dir, "grandchild.pids")
	fake := testtool.Write(t, dir, "grandchild", testtool.Fake{Script: "pid " + gc + "\nwaitfile " + filepath.Join(dir, "never")})
	cmdExe := filepath.Join(os.Getenv("SYSTEMROOT"), "System32", "cmd.exe")
	var out strings.Builder
	p, err := startChild([]string{cmdExe, "/d", "/c", fake}, nil, nopCloser{&out}, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.treeErr != nil {
		t.Fatalf("the child has no job of its own (nested job assignment failed here): %v", p.treeErr)
	}
	waitFor(t, "the grandchild's pid", func() bool { return len(pids(t, gc)) == 1 })
	grandchild := pids(t, gc)[0]
	if err := p.kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the child did not exit after kill")
	}
	if !gone(grandchild, 2*time.Second) {
		t.Errorf("grandchild %d still runs after its parent's kill", grandchild)
		if gp, err := os.FindProcess(grandchild); err == nil {
			_ = gp.Kill()
		}
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// A child that cannot be resumed is killed, and its start failed: the
// restart backoff applies, nothing waits for a frozen postgres.
func TestUnresumableChildIsKilledAndRetried(t *testing.T) {
	fast(t)
	fa := &fakeAfter{fire: 0}
	tm.after = fa.after
	old := resumeChild
	resumeChild = func(*os.Process) error { return errors.New("resume refused") }
	t.Cleanup(func() { resumeChild = old })
	x := newFx(t, "frozen", fakes{})
	x.start(Options{})
	waitStatus(t, x.cfg.Project, stateRestarting, stateStarting)
	waitFor(t, "a restart scheduled", func() bool { return len(fa.got()) == 1 })
	l := x.log("supervisor")
	m := regexp.MustCompile(`db: starting postgres: resuming it \(pid (\d+)\) failed, killed: resume refused`).FindStringSubmatch(l)
	if m == nil {
		t.Fatalf("supervisor log: want the failed resume:\n%s", l)
	}
	pid, _ := strconv.Atoi(m[1])
	if !gone(pid, 2*time.Second) {
		t.Errorf("the unresumable postgres %d still exists", pid)
	}
	if got := testtool.Calls(t, x.pg); got != nil {
		t.Errorf("the frozen postgres ran %q", got)
	}
}
