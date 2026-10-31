//go:build windows

package index

const goos = "windows"

// Windows needs no statfs: network shares show as UNC paths, which
// classifyPath recognises.
func osFSType(string) FSInfo { return FSInfo{FSLocal, ""} }
