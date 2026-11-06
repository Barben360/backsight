//go:build windows

package index

import "golang.org/x/sys/windows"

const goos = "windows"

// getDriveType is the seam over GetDriveType.
var getDriveType = func(root string) uint32 {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return windows.DRIVE_UNKNOWN
	}
	return windows.GetDriveType(p)
}

// A UNC path is recognised by classifyPath; a mapped drive (Z:\) looks
// like a local one, so its type is asked of Windows.
func osFSType(dir string) FSInfo { return winFSType(dir, getDriveType) }
