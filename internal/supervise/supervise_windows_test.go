//go:build windows

package supervise

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
