package index

import (
	"path/filepath"
	"strings"
)

type FSKind string

const (
	FSLocal   FSKind = "local"
	FSNetwork FSKind = "network"
	FSSynced  FSKind = "synced"
)

// FSInfo says where the index lives. Name is nfs, cifs, smb, smb2, 9p,
// sshfs, unc, onedrive, icloud, dropbox, or empty.
type FSInfo struct {
	Kind FSKind `json:"kind"`
	Name string `json:"name"`
}

// detectFS is the seam: the real one asks the OS for the filesystem type,
// then classifies the path.
var detectFS = func(dir, home string, getenv func(string) string) FSInfo {
	if fs := osFSType(dir); fs.Kind != FSLocal {
		return fs
	}
	return classifyPath(goos, home, dir, getenv)
}

func classifyLinux(magic int64, mountType string) FSInfo {
	switch magic {
	case 0x6969:
		return FSInfo{FSNetwork, "nfs"}
	case 0x517B:
		return FSInfo{FSNetwork, "smb"}
	case 0xFE534D42:
		return FSInfo{FSNetwork, "smb2"}
	case 0xFF534D42:
		return FSInfo{FSNetwork, "cifs"}
	case 0x01021997:
		return FSInfo{FSNetwork, "9p"}
	case 0x65735546: // FUSE: only sshfs is known to be remote
		if mountType == "fuse.sshfs" {
			return FSInfo{FSNetwork, "sshfs"}
		}
	}
	return FSInfo{FSLocal, ""}
}

func classifyDarwin(fstype string) FSInfo {
	switch fstype {
	case "nfs", "smbfs", "afpfs", "webdav":
		return FSInfo{FSNetwork, fstype}
	}
	return FSInfo{FSLocal, ""}
}

// under reports whether dir is base or inside it, with the separator and
// case rules of goos.
func under(goos, dir, base string) bool {
	if base == "" {
		return false
	}
	sep := "/"
	if goos == "windows" {
		sep = `\`
		dir = strings.ToLower(strings.ReplaceAll(dir, "/", `\`))
		base = strings.ToLower(strings.ReplaceAll(base, "/", `\`))
	}
	base = strings.TrimRight(base, sep)
	return dir == base || strings.HasPrefix(dir, base+sep)
}

func classifyPath(goos, home, dir string, getenv func(string) string) FSInfo {
	join := func(parts ...string) string {
		if goos == "windows" {
			return strings.Join(parts, `\`)
		}
		return strings.Join(parts, "/")
	}
	if goos == "windows" {
		d := strings.ReplaceAll(dir, "/", `\`)
		if strings.HasPrefix(strings.ToLower(d), `\\?\unc\`) {
			return FSInfo{FSNetwork, "unc"}
		}
		if strings.HasPrefix(d, `\\`) && !strings.HasPrefix(d, `\\?\`) && !strings.HasPrefix(d, `\\.\`) {
			return FSInfo{FSNetwork, "unc"}
		}
	}
	if home != "" && under(goos, dir, join(home, "Dropbox")) {
		return FSInfo{FSSynced, "dropbox"}
	}
	switch goos {
	case "windows":
		for _, v := range []string{"OneDrive", "OneDriveConsumer", "OneDriveCommercial"} {
			if p := getenv(v); p != "" && under(goos, dir, p) {
				return FSInfo{FSSynced, "onedrive"}
			}
		}
		if home != "" && under(goos, dir, join(home, "iCloudDrive")) {
			return FSInfo{FSSynced, "icloud"}
		}
	case "darwin":
		if home == "" {
			break
		}
		if under(goos, dir, join(home, "Library", "Mobile Documents")) {
			return FSInfo{FSSynced, "icloud"}
		}
		cs := join(home, "Library", "CloudStorage")
		if under(goos, dir, cs) && dir != cs {
			rest := strings.TrimPrefix(filepath.ToSlash(dir), cs+"/")
			provider, _, _ := strings.Cut(strings.SplitN(rest, "/", 2)[0], "-")
			if provider != "" {
				return FSInfo{FSSynced, strings.ToLower(provider)}
			}
		}
	}
	return FSInfo{FSLocal, ""}
}

// driveRemote is Windows' DRIVE_REMOTE, the GetDriveType answer for a
// network drive.
const driveRemote = 4

// winVolumeRoot is the drive root ("C:\\") of a Windows path in drive form
// (C:\x or \\?\C:\x), or "" for any other form (a UNC share, which
// classifyPath recognises, or a relative path).
func winVolumeRoot(dir string) string {
	d := strings.ReplaceAll(dir, "/", `\`)
	d = strings.TrimPrefix(d, `\\?\`)
	if len(d) >= 2 && d[1] == ':' && (d[0]|0x20 >= 'a' && d[0]|0x20 <= 'z') {
		return d[:2] + `\`
	}
	return ""
}

// winFSType is osFSType on Windows: a drive that GetDriveType (driveType,
// the seam) reports as remote is a network share, whether it is mapped
// (Z:\) or not. Other drives are left to classifyPath.
func winFSType(dir string, driveType func(root string) uint32) FSInfo {
	if root := winVolumeRoot(dir); root != "" && driveType(root) == driveRemote {
		return FSInfo{FSNetwork, "smb"}
	}
	return FSInfo{FSLocal, ""}
}
