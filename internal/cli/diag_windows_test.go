//go:build windows

package cli

import (
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// diagAccess: temporary evidence for the Windows slice failure.
func diagAccess(t *testing.T, path string) {
	pri, _ := exec.Command("whoami", "/priv").CombinedOutput()
	var en []string
	for _, l := range strings.Split(string(pri), "\n") {
		if strings.Contains(l, "Enabled") {
			en = append(en, strings.Fields(l)[0])
		}
	}
	acl, _ := exec.Command("icacls", path).CombinedOutput()
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err == nil {
		_ = windows.CloseHandle(h)
	}
	h2, err2 := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err2 == nil {
		_ = windows.CloseHandle(h2)
	}
	t.Logf("DIAG2 enabledPrivs=%v plainOpen=%v backupOpen=%v acl=%q", en, err, err2, strings.TrimSpace(string(acl)))
}
