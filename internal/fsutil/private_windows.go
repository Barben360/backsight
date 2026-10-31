//go:build windows

package fsutil

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// accessAllowedAce is ACCESS_ALLOWED_ACE.Header's type value.
const accessAllowedAce = 0

// readMask: the rights that let a principal read file data.
const readMask = 0x1 /* FILE_READ_DATA */ | windows.GENERIC_READ | windows.GENERIC_ALL | 0x1F01FF /* FILE_ALL_ACCESS */

func userSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
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
	me, err := userSID()
	if err != nil {
		return err
	}
	inherit := ""
	if st.IsDir() {
		inherit = "OICI"
	}
	sddl := "D:P(A;" + inherit + ";FA;;;" + me.String() + ")(A;" + inherit + ";FA;;;SY)(A;" + inherit + ";FA;;;BA)"
	sd, err := windows.SecurityDescriptorFromString(sddl)
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
		if ace.Header.AceType != accessAllowedAce || ace.Mask&readMask == 0 {
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
