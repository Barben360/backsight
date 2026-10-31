package server

import (
	"context"
	"errors"
	"io"
)

// pendingWindowsHost stands for the Windows host until Task 9 replaces it
// with taskHost (the supervisor under a per-user scheduled task): the seam
// compiles for Windows, and the layout renders there, but nothing runs.
// TEMPORARY: Task 9 deletes this file.
type pendingWindowsHost struct{ n Native }

var _ nativeHost = pendingWindowsHost{}

var errWindowsHost = errors.New("the native server cannot run on Windows yet: its Windows service host is not implemented")

// Dir is where Task 9's files go: Root() (supervise.json, task.xml).
func (h pendingWindowsHost) Dir() string { return h.n.Root() }

// StopCmd is what Task 9's host answers.
func (pendingWindowsHost) StopCmd() string { return "backsight server stop" }

func (pendingWindowsHost) Files(int, string) (map[string][]byte, error) { return nil, errWindowsHost }
func (pendingWindowsHost) StartDB(context.Context, bool) error          { return errWindowsHost }
func (pendingWindowsHost) StartAPI(context.Context, bool) error         { return errWindowsHost }
func (pendingWindowsHost) StopAPI(context.Context) error                { return errWindowsHost }
func (pendingWindowsHost) Stop(context.Context) error                   { return errWindowsHost }
func (pendingWindowsHost) Remove(context.Context) error                 { return errWindowsHost }
func (pendingWindowsHost) States(context.Context) (map[string]string, error) {
	return nil, errWindowsHost
}
func (pendingWindowsHost) Logs(context.Context, string, int) (string, error) {
	return "", errWindowsHost
}
func (pendingWindowsHost) DeployedVenv(map[string][]byte) (string, error) { return "", errWindowsHost }
func (pendingWindowsHost) Transient(context.Context, string, []string, io.Reader, [][]string) error {
	return errWindowsHost
}
func (pendingWindowsHost) Owns(int) bool                          { return false }
func (pendingWindowsHost) Engine(context.Context) (Engine, error) { return Engine{}, errWindowsHost }
