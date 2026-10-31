//go:build windows

package supervise

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
)

// noWindow is CREATE_NO_WINDOW when this process has no console (started
// detached): a console child would otherwise open a window of its own.
// Under conhost --headless the children share the hidden console, which
// CTRL_BREAK needs.
func noWindow() uint32 {
	if cp, err := windows.GetConsoleCP(); err != nil || cp == 0 {
		return windows.CREATE_NO_WINDOW
	}
	return 0
}

// childAttr: a process group of its own (CTRL_BREAK reaches the child
// alone), started suspended, and for postgres a restricted token when this process is an
// administrator's: postgres refuses to run with administrative rights
// (E34 W4).
func childAttr(postgres bool) (*syscall.SysProcAttr, func(), error) {
	// Suspended until ownTree has it in its own job (job_windows.go).
	attr := &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED | noWindow()}
	if !postgres || !isAdmin() {
		return attr, func() {}, nil
	}
	tok, err := restrictedToken()
	if err != nil {
		return nil, nil, err
	}
	attr.Token = syscall.Token(tok)
	return attr, func() { _ = tok.Close() }, nil
}

// toolAttr is a short tool's: no window of its own.
func toolAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: noWindow()}
}

// isAdmin: this process's token has Administrators or Power Users enabled,
// which postgres refuses (a UAC-filtered token has them deny-only).
func isAdmin() bool {
	for _, t := range []windows.WELL_KNOWN_SID_TYPE{windows.WinBuiltinAdministratorsSid, windows.WinBuiltinPowerUsersSid} {
		sid, err := windows.CreateWellKnownSid(t)
		if err != nil {
			continue
		}
		if in, err := windows.Token(0).IsMember(sid); err == nil && in {
			return true
		}
	}
	return false
}

// restrictedToken is pg_ctl's (E34 ruling 4): this process's token with
// Administrators and Power Users deny-only, flags 0, no privilege removed,
// and the user's SID added to its default DACL: without it postgres
// starts but cannot run its own postgres -V.
func restrictedToken() (windows.Token, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &tok); err != nil {
		return 0, err
	}
	defer func() { _ = tok.Close() }()
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return 0, err
	}
	power, err := windows.CreateWellKnownSid(windows.WinBuiltinPowerUsersSid)
	if err != nil {
		return 0, err
	}
	deny := []windows.SIDAndAttributes{{Sid: admins}, {Sid: power}}
	var rt windows.Token
	if r, _, e := procCreateRestrictedToken.Call(uintptr(tok), 0, uintptr(len(deny)), uintptr(unsafe.Pointer(&deny[0])), 0, 0, 0, 0, uintptr(unsafe.Pointer(&rt))); r == 0 {
		return 0, e
	}
	if err := addUserToDefaultDACL(tok, rt); err != nil {
		_ = rt.Close()
		return 0, err
	}
	return rt, nil
}

type tokenDefaultDacl struct{ DefaultDacl *windows.ACL }

func addUserToDefaultDACL(tok, rt windows.Token) error {
	u, err := tok.GetTokenUser()
	if err != nil {
		return err
	}
	ea := []windows.EXPLICIT_ACCESS{{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(u.User.Sid)}}}
	n := uint32(0)
	_ = windows.GetTokenInformation(rt, windows.TokenDefaultDacl, nil, 0, &n)
	if n == 0 {
		return windows.ERROR_INSUFFICIENT_BUFFER
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(rt, windows.TokenDefaultDacl, &buf[0], n, &n); err != nil {
		return err
	}
	old := (*tokenDefaultDacl)(unsafe.Pointer(&buf[0])).DefaultDacl
	acl, err := windows.ACLFromEntries(ea, old)
	if err != nil {
		return err
	}
	td := tokenDefaultDacl{DefaultDacl: acl}
	return windows.SetTokenInformation(rt, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&td)), uint32(unsafe.Sizeof(td)))
}
