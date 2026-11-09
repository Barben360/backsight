package index

import "github.com/Barben360/backsight/internal/sqlitedb"

// FSKind is where the index lives (sqlitedb.FSKind).
type FSKind = sqlitedb.FSKind

const (
	FSLocal   = sqlitedb.FSLocal
	FSNetwork = sqlitedb.FSNetwork
	FSSynced  = sqlitedb.FSSynced
)

// FSInfo says where the index lives. Name is nfs, cifs, smb, smb2, 9p,
// sshfs, unc, onedrive, icloud, dropbox, or empty. It has
// sqlitedb.FSInfo's fields and converts from it; it is declared here, not
// an alias, so that go vet takes its unkeyed literals as the package's own.
type FSInfo struct {
	Kind FSKind `json:"kind"`
	Name string `json:"name"`
}

// detectFS is the seam: the real one asks the OS for the filesystem type,
// then classifies the path (sqlitedb.DetectFS).
var detectFS = func(dir, home string, getenv func(string) string) FSInfo {
	return FSInfo(sqlitedb.DetectFS(dir, home, getenv))
}

// classifyLinux, classifyDarwin and classifyPath are sqlitedb's
// classifiers, answering FSInfo.
func classifyLinux(magic int64, mountType string) FSInfo {
	return FSInfo(sqlitedb.ClassifyLinux(magic, mountType))
}

func classifyDarwin(fstype string) FSInfo { return FSInfo(sqlitedb.ClassifyDarwin(fstype)) }

func classifyPath(goos, home, dir string, getenv func(string) string) FSInfo {
	return FSInfo(sqlitedb.ClassifyPath(goos, home, dir, getenv))
}

// winFSType is sqlitedb.WinFSType, driveType the seam over GetDriveType.
func winFSType(dir string, driveType func(root string) uint32) FSInfo {
	return FSInfo(sqlitedb.WinFSType(dir, driveType))
}
