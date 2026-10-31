//go:build !windows

package cli

func isolateUserPath() (cleanup func()) { return func() {} }
