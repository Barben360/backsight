package main

import (
	"os"
	"slices"
	"syscall"
	"testing"
)

// A closed terminal (SIGHUP) cancels a restore like Ctrl-C, so its
// rollback runs instead of the process dying half way.
func TestSignalsCancelTheCommand(t *testing.T) {
	for _, s := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if !slices.Contains(signals, s) {
			t.Errorf("%v does not cancel the command", s)
		}
	}
}
