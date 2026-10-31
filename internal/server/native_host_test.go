package server

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubHost is a host whose every method fails: fakeHost overrides what a
// test needs.
type stubHost struct{ n Native }

var errStubHost = errors.New("stub host")

func (h stubHost) Dir() string                                     { return h.n.Root() }
func (stubHost) StopCmd() string                                   { return "stop" }
func (stubHost) Files(int, string) (map[string][]byte, error)      { return nil, errStubHost }
func (stubHost) StartDB(context.Context, bool) error               { return errStubHost }
func (stubHost) StartAPI(context.Context, bool) error              { return errStubHost }
func (stubHost) StopAPI(context.Context) error                     { return errStubHost }
func (stubHost) Stop(context.Context) error                        { return errStubHost }
func (stubHost) Remove(context.Context) error                      { return errStubHost }
func (stubHost) States(context.Context) (map[string]string, error) { return nil, errStubHost }
func (stubHost) Logs(context.Context, string, int) (string, error) { return "", errStubHost }
func (stubHost) DeployedVenv(map[string][]byte) (string, error)    { return "", errStubHost }
func (stubHost) Owns(int) bool                                     { return false }
func (stubHost) Engine(context.Context) (Engine, error)            { return Engine{}, errStubHost }
func (stubHost) fileNames() []string                               { return []string{"stub.conf"} }
func (stubHost) markers() []string                                 { return []string{"db", "api"} }
func (stubHost) restartsFor(string) []string                       { return []string{"api"} }
func (stubHost) Transient(context.Context, string, []string, io.Reader, [][]string) error {
	return errStubHost
}

// fakeHost is a host with neither systemd extra (no dbAloneStarter, no
// transientWatcher): a Transient that keeps every process to its call,
// as the interface says.
type fakeHost struct {
	stubHost
	transient func(ctx context.Context) error
	startDB   int
}

func (h *fakeHost) Transient(ctx context.Context, _ string, _ []string, _ io.Reader, _ [][]string) error {
	return h.transient(ctx)
}

func (h *fakeHost) StartDB(context.Context, bool) error { h.startDB++; return nil }

func withHost(t *testing.T, h nativeHost) {
	t.Helper()
	old := hostFor
	hostFor = func(Native) nativeHost { return h }
	t.Cleanup(func() { hostFor = old })
}

// On a host whose Transient runs the throwaway database in the
// foreground until its context ends, VerifyDump restores and counts while
// it runs, then its cleanup cancels it and waits for it to return.
func TestVerifyDumpOnAForegroundTransient(t *testing.T) {
	n, _, tools := nativeFixture(t, secrets)
	installed(t, n, secrets)
	withFreePort(t, 18890)
	var started, returned atomic.Bool
	withHost(t, &fakeHost{transient: func(ctx context.Context) error {
		started.Store(true)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // a server takes a moment to stop
		returned.Store(true)
		return ctx.Err()
	}})
	tools.Fail = func(c toolCall) error { // the throwaway answers once started
		if filepath.Base(c.Path) == "psql" && !started.Load() {
			return errors.New("connection refused")
		}
		return nil
	}
	var countedWhileRunning []bool
	tools.Out = func(c toolCall) []byte {
		if filepath.Base(c.Path) == "psql" && strings.Contains(strings.Join(c.Args, " "), "select count(*)") {
			countedWhileRunning = append(countedWhileRunning, started.Load() && !returned.Load())
			return []byte("3\n")
		}
		return nil
	}
	counts, err := n.VerifyDump(context.Background(), filepath.Join(t.TempDir(), "db.dump"), []string{"documents"})
	if err != nil {
		t.Fatal(err)
	}
	if counts["documents"] != 3 {
		t.Errorf("counts %v", counts)
	}
	if !slices.Equal(countedWhileRunning, []bool{true}) {
		t.Errorf("counted while the throwaway ran: %v, want [true]", countedWhileRunning)
	}
	if !returned.Load() {
		t.Error("VerifyDump returned before its Transient did: the throwaway may still run")
	}
}

// A Transient that ends (initdb failed) ends the wait at once, with its
// error, never after verifyWait.
func TestVerifyDumpOnATransientThatEnds(t *testing.T) {
	n, _, tools := nativeFixture(t, secrets)
	installed(t, n, secrets)
	withFreePort(t, 18890)
	withHost(t, &fakeHost{transient: func(context.Context) error { return errors.New("initdb: could not create directory") }})
	tools.Fail = func(c toolCall) error {
		if filepath.Base(c.Path) == "psql" {
			return errors.New("connection refused")
		}
		return nil
	}
	start := time.Now()
	_, err := n.VerifyDump(context.Background(), filepath.Join(t.TempDir(), "db.dump"), []string{"documents"})
	if err == nil || !strings.Contains(err.Error(), "the throwaway database stopped") || !strings.Contains(err.Error(), "initdb: could not create directory") {
		t.Errorf("a Transient that ended: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("reported after %s: the wait did not end with the Transient", d)
	}
}

// A host that can only arm the database with StartDB refuses
// StartDatabase, which enables nothing, rather than arming it.
func TestStartDatabaseRefusedWithoutDBAlone(t *testing.T) {
	n, _, _ := nativeFixture(t, secrets)
	installed(t, n, secrets)
	h := &fakeHost{}
	withHost(t, h)
	if err := n.StartDatabase(context.Background()); !errors.Is(err, errNoDBAlone) {
		t.Errorf("StartDatabase = %v, want %v", err, errNoDBAlone)
	}
	if h.startDB != 0 {
		t.Error("StartDatabase armed the database through StartDB")
	}
}
