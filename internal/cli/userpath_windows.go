//go:build windows

package cli

import (
	"errors"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// userPathKey is the registry key, under HKCU, whose Path value is the
// user's PATH. Tests point it at a throwaway key.
var userPathKey = `Environment`

// samePathEntry compares two PATH entries the way Windows does: case
// does not matter, nor a trailing separator.
func samePathEntry(a, b string) bool {
	trim := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), `\/`) }
	return strings.EqualFold(trim(a), trim(b))
}

// pathHas: some entry of list is dir, as written or once its %VARS% are
// expanded.
func pathHas(list, dir string) bool {
	for _, e := range strings.Split(list, ";") {
		if e == "" {
			continue
		}
		if samePathEntry(e, dir) {
			return true
		}
		if x, err := registry.ExpandString(e); err == nil && samePathEntry(x, dir) {
			return true
		}
	}
	return false
}

// readUserPath returns the Path value and its registry type (REG_EXPAND_SZ
// when absent: what Windows itself creates).
func readUserPath(k registry.Key) (string, uint32, error) {
	v, typ, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "", registry.EXPAND_SZ, nil
	}
	if err != nil {
		return "", 0, err
	}
	return v, typ, nil
}

func writeUserPath(k registry.Key, v string, typ uint32) error {
	if typ == registry.SZ {
		return k.SetStringValue("Path", v)
	}
	return k.SetExpandStringValue("Path", v)
}

// ensureUserPath appends dir to the user's PATH (HKCU, no admin) unless an
// entry already is dir, keeps the value's REG_EXPAND_SZ type, and tells
// running programs (Explorer, so the next terminal) with WM_SETTINGCHANGE.
func ensureUserPath(dir string) (added bool, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, userPathKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		k, _, err = registry.CreateKey(registry.CURRENT_USER, userPathKey, registry.QUERY_VALUE|registry.SET_VALUE)
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = k.Close() }()
	cur, typ, err := readUserPath(k)
	if err != nil {
		return false, err
	}
	if pathHas(cur, dir) {
		return false, nil
	}
	next := dir
	if t := strings.TrimRight(cur, ";"); t != "" {
		next = t + ";" + dir
	}
	if err := writeUserPath(k, next, typ); err != nil {
		return false, err
	}
	broadcastEnvironment()
	return true, nil
}

// removeUserPath removes exactly the entries that are dir from the user's
// PATH (uninstall), and nothing else.
func removeUserPath(dir string) (removed bool, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, userPathKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = k.Close() }()
	cur, typ, err := readUserPath(k)
	if err != nil {
		return false, err
	}
	var keep []string
	for _, e := range strings.Split(cur, ";") {
		if e != "" && samePathEntry(e, dir) {
			removed = true
			continue
		}
		keep = append(keep, e)
	}
	if !removed {
		return false, nil
	}
	if err := writeUserPath(k, strings.Join(keep, ";"), typ); err != nil {
		return false, err
	}
	broadcastEnvironment()
	return true, nil
}

// broadcastEnvironment sends WM_SETTINGCHANGE "Environment" to every top
// window, so Explorer rereads the environment for the programs it starts.
// It is best effort and bounded (5 s per window, hung windows skipped).
func broadcastEnvironment() {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	p, err := syscall.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	var res uintptr
	_, _, _ = proc.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(p)),
		smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&res)))
}
