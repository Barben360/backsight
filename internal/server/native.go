package server

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server/nativelock"
)

// Native is the native runtime (decision 0078): Hindsight from PyPI, in a
// virtual environment built by a pinned uv with a pinned Python from the
// hash-locked set nativelock carries, on pg0's Postgres, under two systemd
// user units. Linux x86_64 only. Everything it installs lives under Root():
//
//	uv/<uv version>/uv      the pinned uv, checked against its sha256
//	python/                 uv's Python (UV_PYTHON_INSTALL_DIR)
//	venv/<version>/         one virtual environment per server version
//	home/                   the units' HOME (pg0's ~/.pg0), 0700
//	hf/                     the models (HF_HOME)
//	server.env, identity    the server's environment and the database's
//	                        identity, 0600
type Native struct {
	Cfg     config.Config
	Dirs    paths.Dirs        // shim (<data>/bin), config editor journal, state
	UnitDir string            // the scheduler's rule (jobs_install.go)
	Env     map[string]string // BACKSIGHT_HOME, XDG_*_HOME carried into the units (scheduler.Options.Env)
	Sys     SysRunner         // systemctl, loginctl, systemd-run, journalctl
	Tools   ToolRunner        // uv, pg0, Postgres tools, hindsight-admin, hf
	Proc    string            // "/proc"; tests: a fake tree
	// MayCreate lets Start and StartDatabase create the database when
	// neither it nor its identity exists (ruling R35): only an install or
	// a switch to native sets it; elsewhere nothing there is "missing".
	MayCreate bool
	// UserHome is the user's HOME, which the db unit's native-check finds
	// backsight's directories from when Env sets none: the unit's own
	// HOME is home/, pg0's.
	UserHome string
	// StrayWait bounds how long StopStrays waits for a stray to be gone
	// (0: 30 s).
	StrayWait time.Duration
	Venv      string // "" = the pinned version; WithVersion sets another
	Err       error  // a construction error every method returns
	// GOOS is the OS the runtime lays out and runs for ("": this one):
	// tests set "windows" on Linux to render the Windows layout and files.
	GOOS string
	// Note, when set, tells the user what a command's result does not
	// say: on Windows, NoLogonNote.
	Note func(string)
	// NotInUse: config.toml names the docker runtime, so the native data
	// here is what a switch left (the target of a switch, built before
	// config.toml names it, sets it). A refused database then points at
	// server runtime clean: restore acts on Docker's.
	NotInUse bool
}

// notInUse: config.toml does not name the native runtime.
func (n Native) notInUse() bool { return n.NotInUse || n.Cfg.RuntimeKind() != "native" }

// SysRunner runs the system's service tools.
type SysRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ToolRunner runs the native runtime's own programs.
type ToolRunner interface {
	// Run starts path with exactly env (nothing inherited) and stdin; out
	// is stdout, err carries stderr's tail.
	Run(ctx context.Context, env []string, stdin io.Reader, path string, args ...string) (out []byte, err error)
}

// Root is <compose_dir>/native: a second install with another compose_dir
// stays apart.
func (n Native) Root() string { return n.join(n.Cfg.Server.ComposeDir, "native") }

// Python is where uv installs Python (UV_PYTHON_INSTALL_DIR).
func (n Native) Python() string { return n.join(n.Root(), "python") }

// PythonBin is the pinned Python's interpreter, as uv installs it.
func (n Native) PythonBin() string {
	return n.join(n.Python(), "cpython-"+nativelock.Python()+"-linux-x86_64-gnu", "bin", "python3.11")
}

// VenvDir is version v's virtual environment.
func (n Native) VenvDir(v string) string { return n.join(n.Root(), "venv", v) }

// Home is the units' HOME: pg0 keeps its installation and instance in
// ~/.pg0.
func (n Native) Home() string { return n.join(n.Root(), "home") }

// HF is the models' cache (HF_HOME).
func (n Native) HF() string { return n.join(n.Root(), "hf") }

// EnvFile is the server unit's environment, secrets included (0600).
func (n Native) EnvFile() string { return n.join(n.Root(), "server.env") }

// IdentityFile holds the database's system identifier (0600).
func (n Native) IdentityFile() string { return n.join(n.Root(), "identity") }

func (Native) Kind() string { return "native" }

// Version is the server version this runtime deploys: the one WithVersion
// set (ruling R9), else the lock's, <hindsight version>-<lock hash12>.
func (n Native) Version() string {
	if n.Venv != "" {
		return n.Venv
	}
	return nativelock.Hindsight() + "-" + nativelock.Hash12()
}

var _ Runtime = Native{}

// WithVersion is the same runtime deploying version v ("": the pin).
func (n Native) WithVersion(v string) Runtime { n.Venv = v; return n }

var _ Pruner = Native{}

// PruneVersions removes every venv but keep's, incomplete ones and an
// older build's *.partial included, once an upgrade passed (decision 0078:
// the new venv and the previous one stay). The venv the server unit runs
// is never removed, whatever keep says.
func (n Native) PruneVersions(keep ...string) error {
	if n.Err != nil {
		return n.Err
	}
	running, err := n.Deployed()
	if err != nil {
		return err
	}
	keep = append(keep, running)
	dir := filepath.Join(n.Root(), "venv")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		if !slices.Contains(keep, e.Name()) {
			errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

// HindsightVersion is the Hindsight release of Version ("1.1.0").
func (n Native) HindsightVersion() string { return HindsightVersionOf(n.Version()) }

// ComparePins orders two native versions by their Hindsight x.y.z; the
// same release with another lock is not comparable (0, false).
func (Native) ComparePins(a, b string) (int, bool) {
	if a != "" && a == b {
		return 0, true
	}
	va, okA := nativeRelease(a)
	vb, okB := nativeRelease(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range va {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, false
}

// nativeRelease is the x.y.z of a native version <x.y.z>-<hash12>.
func nativeRelease(v string) ([3]int, bool) {
	m := nativeTag.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	return imageVersion(":" + m[1])
}
