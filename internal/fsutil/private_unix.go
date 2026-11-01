//go:build !windows

package fsutil

import (
	"io/fs"
	"os"
)

// MakePrivate makes the existing path its owner's alone: 0700 for a
// directory, 0600 for a file.
func MakePrivate(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return os.Chmod(path, 0o700)
	}
	return os.Chmod(path, 0o600)
}

// OtherReaders names the principals other than the owner that can read
// path: "group" and "others" when their read bit is set.
func OtherReaders(path string) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var who []string
	if st.Mode().Perm()&fs.FileMode(0o040) != 0 {
		who = append(who, "group")
	}
	if st.Mode().Perm()&fs.FileMode(0o004) != 0 {
		who = append(who, "others")
	}
	return who, nil
}

// CurrentUserSID is "" outside Windows: no SIDs.
func CurrentUserSID() string { return "" }

// createPrivateTemp creates a new temp file in dir (os.CreateTemp's
// pattern), mode 0600 from its creation as os.CreateTemp makes it.
func createPrivateTemp(dir, pattern string) (*os.File, error) { return os.CreateTemp(dir, pattern) }
