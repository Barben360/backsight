//go:build windows

package fsutil

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// accessAllowedAce is ACCESS_ALLOWED_ACE.Header's type value.
const accessAllowedAce = 0

// readMask: the rights that let a principal read file data.
const (
	fileReadData  = 0x1
	fileAllAccess = 0x1F01FF
	readMask      = fileReadData | windows.GENERIC_READ | windows.GENERIC_ALL
)

// reads: the mask grants file data to its holder; write, delete and
// synchronize bits alone do not.
func reads(mask uint32) bool { return mask&readMask != 0 || mask&fileAllAccess == fileAllAccess }

func userSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
}

// CurrentUserSID is the current user's SID (S-1-5-…), "" when the token
// does not tell it.
func CurrentUserSID() string {
	sid, err := userSID()
	if err != nil {
		return ""
	}
	return sid.String()
}

func wellKnown(t windows.WELL_KNOWN_SID_TYPE) (*windows.SID, error) {
	return windows.CreateWellKnownSid(t)
}

// MakePrivate gives path a protected DACL (no inherited entries) granting
// full control to the current user, SYSTEM and Administrators alone; on a
// directory the entries are inherited by what is created inside it.
func MakePrivate(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	sd, err := privateSD(st.IsDir())
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// OtherReaders names the accounts, beyond the current user, SYSTEM and
// Administrators, that an allow entry of path's DACL lets read it. A SID
// that does not resolve is named by its string form; a missing DACL (full
// access to everyone) is "Everyone".
func OtherReaders(path string) ([]string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, err
	}
	if dacl == nil {
		return []string{"Everyone"}, nil
	}
	me, err := userSID()
	if err != nil {
		return nil, err
	}
	var own []*windows.SID
	own = append(own, me)
	for _, t := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		s, err := wellKnown(t)
		if err != nil {
			return nil, err
		}
		own = append(own, s)
	}
	var who []string
	seen := map[string]bool{}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, err
		}
		if ace.Header.AceType != accessAllowedAce || !reads(uint32(ace.Mask)) {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		mine := false
		for _, o := range own {
			if sid.Equals(o) {
				mine = true
			}
		}
		if mine {
			continue
		}
		name := sid.String()
		if acct, dom, _, err := sid.LookupAccount(""); err == nil {
			name = acct
			if dom != "" {
				name = dom + `\` + acct
			}
		}
		if !seen[name] {
			seen[name] = true
			who = append(who, name)
		}
	}
	return who, nil
}

// createPrivateTemp creates a new temp file in dir, named as
// os.CreateTemp names it from pattern (its last "*" replaced by a random
// number), already private: CreateFile with CREATE_NEW and the private
// security descriptor (MakePrivate's, for a file), so no other user can
// open it at any moment, not even empty (a handle opened before a DACL
// change keeps its access).
func createPrivateTemp(dir, pattern string) (*os.File, error) {
	sd, err := privateSD(false)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	prefix, suffix := pattern, ""
	if i := strings.LastIndex(pattern, "*"); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	for try := 0; try < 10000; try++ {
		name := filepath.Join(dir, prefix+strconv.FormatUint(uint64(rand.Uint32()), 10)+suffix)
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, sa,
			windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "createtemp", Path: name, Err: err}
		}
		return os.NewFile(uintptr(h), name), nil
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: os.ErrExist}
}

// privateSD is the security descriptor MakePrivate gives: a protected DACL
// granting full control to the current user, SYSTEM and Administrators,
// inherited by what a directory holds.
func privateSD(dir bool) (*windows.SECURITY_DESCRIPTOR, error) {
	me, err := userSID()
	if err != nil {
		return nil, err
	}
	inherit := ""
	if dir {
		inherit = "OICI"
	}
	return windows.SecurityDescriptorFromString("D:P(A;" + inherit + ";FA;;;" + me.String() + ")(A;" + inherit + ";FA;;;SY)(A;" + inherit + ";FA;;;BA)")
}
