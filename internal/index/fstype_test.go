package index

import "testing"

func TestClassifyFilesystems(t *testing.T) {
	home := "/home/u"
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	type c struct {
		name string
		got  FSInfo
		want FSInfo
	}
	net := func(n string) FSInfo { return FSInfo{FSNetwork, n} }
	loc := FSInfo{FSLocal, ""}
	cases := []c{
		{"nfs", classifyLinux(0x6969, ""), net("nfs")},
		{"smb", classifyLinux(0x517B, ""), net("smb")},
		{"smb2", classifyLinux(0xFE534D42, ""), net("smb2")},
		{"cifs", classifyLinux(0xFF534D42, ""), net("cifs")},
		{"9p", classifyLinux(0x01021997, ""), net("9p")},
		{"sshfs", classifyLinux(0x65735546, "fuse.sshfs"), net("sshfs")},
		{"fuse gocryptfs", classifyLinux(0x65735546, "fuse.gocryptfs"), loc},
		{"fuse unknown mount", classifyLinux(0x65735546, ""), loc},
		{"ext4", classifyLinux(0xEF53, "ext4"), loc},
		{"unknown magic", classifyLinux(0x1234abcd, ""), loc},
		{"darwin nfs", classifyDarwin("nfs"), net("nfs")},
		{"darwin smbfs", classifyDarwin("smbfs"), net("smbfs")},
		{"darwin afpfs", classifyDarwin("afpfs"), net("afpfs")},
		{"darwin webdav", classifyDarwin("webdav"), net("webdav")},
		{"apfs", classifyDarwin("apfs"), loc},
		{"unc", classifyPath("windows", `C:\Users\u`, `\\srv\share\x\state`, env(nil)), net("unc")},
		{"dropbox", classifyPath("linux", home, home+"/Dropbox/x", env(nil)), FSInfo{FSSynced, "dropbox"}},
		{"dropbox win", classifyPath("windows", `C:\Users\u`, `C:\Users\u\Dropbox\x`, env(nil)), FSInfo{FSSynced, "dropbox"}},
		{"onedrive win", classifyPath("windows", `C:\Users\u`, `D:\OD\x`, env(map[string]string{"OneDrive": `D:\OD`})), FSInfo{FSSynced, "onedrive"}},
		{"onedrive consumer", classifyPath("windows", `C:\Users\u`, `D:\OC\x`, env(map[string]string{"OneDriveConsumer": `D:\OC`})), FSInfo{FSSynced, "onedrive"}},
		{"onedrive commercial", classifyPath("windows", `C:\Users\u`, `D:\OB\x`, env(map[string]string{"OneDriveCommercial": `D:\OB`})), FSInfo{FSSynced, "onedrive"}},
		{"icloud win", classifyPath("windows", `C:\Users\u`, `C:\Users\u\iCloudDrive\x`, env(nil)), FSInfo{FSSynced, "icloud"}},
		{"icloud mac", classifyPath("darwin", "/Users/u", "/Users/u/Library/Mobile Documents/x", env(nil)), FSInfo{FSSynced, "icloud"}},
		{"cloudstorage", classifyPath("darwin", "/Users/u", "/Users/u/Library/CloudStorage/OneDrive-Acme/x", env(nil)), FSInfo{FSSynced, "onedrive"}},
		{"cloudstorage dropbox", classifyPath("darwin", "/Users/u", "/Users/u/Library/CloudStorage/Dropbox/x", env(nil)), FSInfo{FSSynced, "dropbox"}},
		{"plain state", classifyPath("linux", home, home+"/.local/state/backsight", env(nil)), loc},
		{"onedrive env unset", classifyPath("windows", `C:\Users\u`, `D:\OD\x`, env(nil)), loc},
		{"sibling prefix", classifyPath("linux", home, home+"/Dropbox2/x", env(nil)), loc},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, c.got, c.want)
		}
	}
}
