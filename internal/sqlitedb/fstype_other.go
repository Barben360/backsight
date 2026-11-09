//go:build !linux && !darwin && !windows

package sqlitedb

import "runtime"

var goos = runtime.GOOS

func osFSType(string) FSInfo { return FSInfo{FSLocal, ""} }
