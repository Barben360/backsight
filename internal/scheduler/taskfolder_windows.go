//go:build windows && amd64

package scheduler

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DeleteTaskFolder deletes the Task Scheduler folder \<name> that
// schtasks /Delete leaves behind once its last task is gone. Task
// Scheduler's own COM API does it (schtasks has no folder command):
// CoCreateInstance(CLSID_TaskScheduler) → ITaskService.Connect →
// GetFolder("\") → DeleteFolder(name). Task Scheduler deletes only an
// empty folder: one that still holds a task or a folder is refused, and
// that refusal is the error. A folder that is not there is no error
// (deleted false). No administrator right is needed for the user's own
// folder. x64 only: Connect's VARIANTs are passed by reference as the x64
// ABI passes every aggregate over 8 bytes (taskfolder_arch.go elsewhere).
func DeleteTaskFolder(name string) (deleted bool, err error) {
	if name == "" || strings.ContainsAny(name, `\/`) {
		return false, fmt.Errorf("task folder %q: want one folder name under the root", name)
	}
	// COM's apartment belongs to the thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	switch err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); {
	case err == nil, errors.Is(err, syscall.Errno(sFalse)): // initialized here, or already in this mode
		defer windows.CoUninitialize()
	case errors.Is(err, syscall.Errno(rpcEChangedMode)): // the thread's own apartment serves too
	default:
		return false, fmt.Errorf("deleting the task folder \\%s: CoInitializeEx: %w", name, err)
	}
	// What the calls write through is on the heap, which never moves: a
	// goroutine stack may, while a uintptr holds its address.
	pSvc := new(*comObject)
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidTaskScheduler)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidITaskService)), uintptr(unsafe.Pointer(pSvc))); failed(hr) {
		return false, hresultErr(name, "CoCreateInstance(TaskScheduler)", hr)
	}
	svc := *pSvc
	defer svc.release()
	// Connect(serverName, user, domain, password): four empty VARIANTs, the
	// local machine as the current user. A VARIANT is 24 bytes on amd64,
	// passed by reference as every aggregate larger than 8 bytes is.
	v := new([4]variant)
	if hr := svc.call(vtConnect, uintptr(unsafe.Pointer(&v[0])), uintptr(unsafe.Pointer(&v[1])),
		uintptr(unsafe.Pointer(&v[2])), uintptr(unsafe.Pointer(&v[3]))); failed(hr) {
		return false, hresultErr(name, "ITaskService.Connect", hr)
	}
	runtime.KeepAlive(v)
	root, err := sysAllocString(`\`)
	if err != nil {
		return false, err
	}
	defer sysFreeString(root)
	pFolder := new(*comObject)
	if hr := svc.call(vtGetFolder, root, uintptr(unsafe.Pointer(pFolder))); failed(hr) {
		return false, hresultErr(name, `ITaskService.GetFolder("\")`, hr)
	}
	folder := *pFolder
	defer folder.release()
	sub, err := sysAllocString(name)
	if err != nil {
		return false, err
	}
	defer sysFreeString(sub)
	switch hr := folder.call(vtDeleteFolder, sub, 0); {
	case !failed(hr):
		return true, nil
	case hr == hrFileNotFound, hr == hrPathNotFound:
		return false, nil
	default:
		return false, hresultErr(name, "ITaskFolder.DeleteFolder", hr)
	}
}

// The COM identities and vtable slots (taskschd.h): ITaskService and
// ITaskFolder derive from IDispatch (slots 0-6).
var (
	clsidTaskScheduler = windows.GUID{Data1: 0x0F87369F, Data2: 0xA4E5, Data3: 0x4CFC, Data4: [8]byte{0xBD, 0x3E, 0x73, 0xE6, 0x15, 0x45, 0x72, 0xDD}}
	iidITaskService    = windows.GUID{Data1: 0x2FABA4C7, Data2: 0x4DA9, Data3: 0x4013, Data4: [8]byte{0x96, 0x97, 0x20, 0xCC, 0x3F, 0xD4, 0x0F, 0x85}}

	modole32             = windows.NewLazySystemDLL("ole32.dll")
	modoleaut32          = windows.NewLazySystemDLL("oleaut32.dll")
	procCoCreateInstance = modole32.NewProc("CoCreateInstance")
	procSysAllocString   = modoleaut32.NewProc("SysAllocString")
	procSysFreeString    = modoleaut32.NewProc("SysFreeString")
)

const (
	vtRelease      = 2  // IUnknown::Release
	vtGetFolder    = 7  // ITaskService::GetFolder
	vtConnect      = 10 // ITaskService::Connect
	vtDeleteFolder = 12 // ITaskFolder::DeleteFolder

	clsctxInprocServer = 0x1
	sFalse             = 0x1
	rpcEChangedMode    = 0x80010106
	hrFileNotFound     = 0x80070002 // HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND): no such folder
	hrPathNotFound     = 0x80070003
)

// variant is an empty VARIANT (VT_EMPTY): 24 bytes on amd64.
type variant struct {
	vt, r1, r2, r3 uint16
	val, pad       uintptr
}

// comObject is a COM interface pointer: its first word is its vtable.
type comObject struct{ vtbl *[32]uintptr }

func (o *comObject) call(slot int, args ...uintptr) uintptr {
	hr, _, _ := syscall.SyscallN(o.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return hr
}

func (o *comObject) release() { o.call(vtRelease) }

func failed(hr uintptr) bool { return int32(uint32(hr)) < 0 }

// hresultErr names the failed call; a Win32 error (FACILITY_WIN32) is
// wrapped as its errno (ERROR_DIR_NOT_EMPTY: a task is still there).
func hresultErr(name, what string, hr uintptr) error {
	if uint32(hr)&0xFFFF0000 == 0x80070000 {
		return fmt.Errorf("deleting the task folder \\%s: %s: %w", name, what, syscall.Errno(uint32(hr)&0xFFFF))
	}
	return fmt.Errorf("deleting the task folder \\%s: %s: HRESULT 0x%08X", name, what, uint32(hr))
}

func sysAllocString(s string) (uintptr, error) {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return 0, err
	}
	b, _, _ := procSysAllocString.Call(uintptr(unsafe.Pointer(p)))
	if b == 0 {
		return 0, errors.New("SysAllocString: out of memory")
	}
	return b, nil
}

func sysFreeString(b uintptr) { _, _, _ = procSysFreeString.Call(b) }
