package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/Barben360/backsight/internal/scheduler"
)

// The native runtime's pre-flight on Windows (decision 0080): the path
// bound, the % rule, conhost --headless and Task Scheduler. Every check
// here is pure over its probe, so it runs on any OS; the real probes are
// in preflight_windows.go.

// The path bound (E34 ruling 9): Windows' MAX_PATH is 260 with long
// paths off (turning them on needs admin), and the deepest file below a
// venv root is 133 characters (measured): the root plus VenvDepth must
// stay within PathBound.
const (
	PathBound = 250
	VenvDepth = 140
)

// VenvPathLength is p's length as Windows counts it: UTF-16 units.
func VenvPathLength(p string) int { return len(utf16.Encode([]rune(p))) }

// VenvPathFits: the venv root leaves room for the files below it.
func VenvPathFits(venvRoot string) bool { return VenvPathLength(venvRoot)+VenvDepth <= PathBound }

// ShortHomeFix is the way out of a path too long or holding a %.
const ShortHomeFix = `set BACKSIGHT_HOME to a short directory of yours without a % (C:\bs, say) and run setup again from that shell: everything backsight keeps goes there`

func pathLengthCheck(venvRoot string) Check {
	c := Check{ID: "path-length"}
	n := VenvPathLength(venvRoot)
	if VenvPathFits(venvRoot) {
		c.Status, c.Summary = OK, fmt.Sprintf("the venv path is %d characters (at most %d)", n, PathBound-VenvDepth)
		return c
	}
	c.Status, c.Class = Fail, "blocked"
	c.Summary = fmt.Sprintf("the native server's venv %s is %d characters: its deepest files, %d below it, would pass Windows' path limit (long paths need admin to turn on)",
		venvRoot, n, VenvDepth)
	c.Fix = ShortHomeFix
	return c
}

// pathCharsCheck: Task Scheduler expands %VAR% in its task's command line
// (E34 ruling 5), so a path holding a % would reach the supervisor as
// another one.
func pathCharsCheck(paths []string) Check {
	c := Check{ID: "path-chars"}
	for _, p := range paths {
		if strings.Contains(p, "%") {
			c.Status, c.Class = Fail, "blocked"
			c.Summary = p + " holds a %, which Task Scheduler expands in the server task's command line"
			c.Fix = ShortHomeFix
			return c
		}
	}
	c.Status, c.Summary = OK, "no % in backsight's paths"
	return c
}

// errNotProbed: a check whose probe was not set.
var errNotProbed = errors.New("not probed")

func conhostCheck(ctx context.Context, p Probes) Check {
	c := Check{ID: "conhost"}
	err := errNotProbed
	if p.Conhost != nil {
		err = p.Conhost(ctx)
	}
	if err != nil {
		c.Status, c.Class = Fail, "blocked"
		c.Summary = "conhost.exe --headless did not run (" + err.Error() + "): the server's task runs its supervisor under it, with no window"
		c.Fix = "conhost --headless comes with Windows 10 version 1809 and later: update Windows"
		return c
	}
	c.Status, c.Summary = OK, "conhost.exe --headless runs"
	return c
}

// TaskFallback is how the native server runs when Task Scheduler refuses
// its task (a policy): setup goes on, and says so.
const TaskFallback = "setup goes on without the task: it starts the server until you log off and sets server.start_on_session = true, " +
	"so every Claude Code session starts it when it is down (backsight server start does too); it no longer starts at logon on its own"

func taskCheck(ctx context.Context, p Probes) Check {
	c := Check{ID: "task"}
	err := errNotProbed
	if p.Task != nil {
		err = p.Task(ctx)
	}
	if err != nil {
		c.Status = Warn
		c.Summary = "Task Scheduler refused the server's task (" + strings.TrimSpace(err.Error()) + ")"
		c.Fix = TaskFallback
		return c
	}
	c.Status, c.Summary = OK, "Task Scheduler accepts your task"
	return c
}

// StartOnSession: the pre-flight found Task Scheduler refusing the task,
// so setup sets server.start_on_session.
func StartOnSession(cs []Check) bool {
	for _, c := range cs {
		if c.ID == "task" && c.Status == Warn {
			return true
		}
	}
	return false
}

// TaskAccepted: the pre-flight's probe task passed, so a
// server.start_on_session an earlier setup set no longer holds.
func TaskAccepted(cs []Check) bool {
	for _, c := range cs {
		if c.ID == "task" && c.Status == OK {
			return true
		}
	}
	return false
}

// Run runs a system program, as the real probes and tests do.
type Run func(ctx context.Context, name string, args ...string) ([]byte, error)

// probeStart is the probe's only trigger: never reached, so the task
// never runs (StartWhenAvailable runs a missed start at once).
const probeStart = "2099-01-01T00:00:00"

// ProbeTask creates \<project>\probe from the XML scheduler.TaskXML
// renders for the user sid (shim version under conhost --headless, never
// due), then deletes it, always: a refused /Create may have left it
// half made. /F replaces a probe an interrupted setup left behind. The XML goes through a file in dir, removed after.
func ProbeTask(ctx context.Context, run Run, sid, project, shim, dir string) (err error) {
	name := `\` + project + `\probe`
	xml, err := scheduler.TaskXML(scheduler.TaskSpec{
		Description: "backsight setup's probe (" + project + "), deleted at once",
		UserSID:     sid,
		Triggers:    []scheduler.Trigger{{Kind: scheduler.TriggerTime, Start: probeStart}},
		Program:     shim,
		Args:        []string{"version"},
	})
	if err != nil {
		return err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	file := filepath.Join(dir, "backsight-probe-"+hex.EncodeToString(rnd[:])+".xml")
	if err := os.WriteFile(file, xml, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(file) }()
	defer func() {
		_, derr := run(ctx, "schtasks", "/Delete", "/F", "/TN", name)
		if err == nil && derr != nil {
			err = fmt.Errorf("deleting the probe task %s: %w", name, derr)
		}
	}()
	if out, cerr := run(ctx, "schtasks", "/Create", "/F", "/XML", file, "/TN", name); cerr != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %w", msg, cerr)
		}
		return cerr
	}
	return nil
}
