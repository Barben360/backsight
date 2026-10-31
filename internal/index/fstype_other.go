//go:build !linux && !darwin && !windows

package index

import "runtime"

var goos = runtime.GOOS

func osFSType(string) FSInfo { return FSInfo{FSLocal, ""} }
