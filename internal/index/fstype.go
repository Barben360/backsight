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
