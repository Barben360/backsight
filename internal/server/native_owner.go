package server

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The native runtime names what it installs machine-wide after
// server.project alone: the systemd user units <project>-db.service and
// <project>-server.service (Linux), the scheduled task \<project>\server
// and the supervisor's mutex and pipe (Windows, per user). A second home
// of the same user and project (BACKSIGHT_HOME, a test install) would
// rewrite, start, stop or delete the first install's (issue #158, the
// native twin of #144): every operation on them checks first that they
// are this install's.

// ForeignNativeError: the native server's units, task or supervisor of
// Project belong to another backsight install, in Other. Driving them
// from Dir would rewrite, start, stop or delete that install's server.
type ForeignNativeError struct {
	Project string // server.project
	What    string // what belongs to the other install: its units, its task, its supervisor
	Dir     string // this install's native directory
	Other   string // the other install's directory
	Free    string // how to free them once Other is an old copy nothing runs
}

func (e *ForeignNativeError) Error() string {
	return fmt.Sprintf("the native server of project %q (%s) belongs to another backsight install, in %s (this one is in %s). "+
		"Set server.project in this install's config.toml to a name of its own, or run the command from the install in %s. "+
		"If %s is an old copy of this install that nothing runs any more, %s and run the command again: this install then takes it over",
		e.Project, e.What, e.Other, e.Dir, e.Other, e.Other, e.Free)
}

// ownerChecker is a host that can tell whether the units, task or
// supervisor it would act on are this install's.
type ownerChecker interface {
	checkOwner(ctx context.Context) error
}

// CheckOwner returns a *ForeignNativeError when what the native server of
// this project runs under (units, task, supervisor) belongs to another
// install whose directory is still there; another error when it cannot
// be read (refused: only what is known to be this install's, or gone, is
// driven). Nothing installed yet, this install's own, or another's whose
// directory (Linux, the supervisor: its server.env) or program (the task)
// is gone, a moved or renamed home, is this install's to drive: its next
// render or start takes it over. Read-only.
func (n Native) CheckOwner(ctx context.Context) error {
	if n.Err != nil {
		return nil // every operation returns n.Err itself
	}
	if c, ok := n.host().(ownerChecker); ok {
		return c.checkOwner(ctx)
	}
	return nil
}

// ownerCheckWait bounds checkOwnerNow: the task's query and the
// supervisor's status answer.
const ownerCheckWait = 30 * time.Second

// checkOwnerNow is CheckOwner for an operation that takes no context
// (Write), bounded.
func (n Native) checkOwnerNow() error {
	ctx, cancel := context.WithTimeout(context.Background(), ownerCheckWait)
	defer cancel()
	return n.CheckOwner(ctx)
}

// goneFile: p is known not to exist; one that cannot be looked at
// (another user's home) is not.
func goneFile(p string) bool {
	_, err := os.Stat(p)
	return errors.Is(err, fs.ErrNotExist)
}

// foreignRoot is the refusal of units or a supervisor that run from the
// native directory root, nil when root is this install's or no longer
// holds a server.env (moved, or an old copy nothing runs: the units and
// the supervisor read their environment from it).
func (n Native) foreignRoot(root, what string) error {
	if root == "" || sameDir(root, n.Root()) || goneFile(filepath.Join(root, "server.env")) {
		return nil
	}
	root = filepath.Clean(root)
	return &ForeignNativeError{Project: n.project(), What: what, Dir: n.Root(), Other: root,
		Free: "delete " + filepath.Join(root, "server.env")}
}

var _ ownerChecker = systemdHost{}

// checkOwner reads the native directory each unit of the project runs
// from (the server unit's WorkingDirectory=, the database unit's HOME):
// another install's, still there, is refused.
func (h systemdHost) checkOwner(context.Context) error {
	n := h.n
	if n.UnitDir == "" {
		return nil
	}
	for _, u := range []string{n.DBUnit(), n.ServerUnit()} {
		path := filepath.Join(n.UnitDir, u)
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot check which install owns the native server's unit %s: %w", path, err)
		}
		if err := h.foreignUnit(raw); err != nil {
			return err
		}
	}
	return nil
}

// foreignUnit is the refusal of a unit another install's directory runs.
func (h systemdHost) foreignUnit(unit []byte) error {
	return h.n.foreignRoot(unitRoot(unit), "the systemd user units "+h.n.DBUnit()+" and "+h.n.ServerUnit())
}

// otherUnit: the unit runs from another directory than this install's
// (another install's, or a moved home's).
func (h systemdHost) otherUnit(unit []byte) bool {
	r := unitRoot(unit)
	return r != "" && !sameDir(r, h.n.Root())
}

// unitRoot is the native directory a unit backsight wrote runs from: the
// server unit's WorkingDirectory=, the database unit's HOME (<root>/home);
// "" when it names neither.
func unitRoot(unit []byte) string {
	for _, l := range strings.Split(string(unit), "\n") {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, "WorkingDirectory="); ok && v != "" {
			return strings.ReplaceAll(v, "%%", "%")
		}
		if v, ok := strings.CutPrefix(l, "Environment="); ok {
			if home, ok := strings.CutPrefix(systemdUnquote(strings.TrimSpace(v)), "HOME="); ok && filepath.Base(home) == "home" {
				return filepath.Dir(home)
			}
		}
	}
	return ""
}

var _ ownerChecker = taskHost{}

// checkOwner reads the program the registered task \<project>\server runs
// (another install's shim is that install's task: the shim lives in its
// data directory) and the native directory the supervisor answering the
// project's pipe runs from. A task that cannot be queried is taken as
// not registered, as everywhere else (schtasks' text is localized); a
// supervisor that does not answer, or too old to name its directory,
// cannot be told and is left to the operation, which asks it the same.
func (h taskHost) checkOwner(ctx context.Context) error {
	n := h.n
	var out []byte
	err := errors.New("no schtasks") // a Native built without Sys (the SessionStart hook's) asks the pipe only
	if n.Sys != nil {
		out, err = n.Sys.Run(ctx, "schtasks", "/Query", "/TN", n.TaskName(), "/XML")
	}
	if err == nil {
		if prog := taskProgram(out); prog != "" && !sameDir(prog, n.shim()) && !goneFile(prog) {
			data := filepath.Dir(filepath.Dir(prog)) // <data>\bin\<shim>
			return &ForeignNativeError{Project: n.project(), What: "the scheduled task " + n.TaskName() + ", which runs " + prog,
				Dir: n.Root(), Other: data, Free: "delete its task (schtasks /Delete /TN " + n.TaskName() + " /F)"}
		}
	} else if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("cannot check which install owns the scheduled task %s: %w", n.TaskName(), cerr)
	}
	st, err := h.ask(ctx, "status")
	if err != nil {
		return nil
	}
	return n.foreignRoot(st.Root, fmt.Sprintf("its supervisor, pid %d", st.PID))
}

// taskProgram is the program a server task's XML runs: conhost's
// --headless <program> (scheduler.HeadlessArgs); "" when it names none.
func taskProgram(raw []byte) string {
	var task struct {
		Arguments string `xml:"Actions>Exec>Arguments"`
	}
	dec := xml.NewDecoder(bytes.NewReader(utf8XML(raw)))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil } // UTF-16 already decoded
	if dec.Decode(&task) != nil {
		return ""
	}
	argv := splitWindowsArgs(task.Arguments)
	if len(argv) < 2 || argv[0] != "--headless" {
		return ""
	}
	return argv[1]
}

// splitWindowsArgs splits a command line as Windows programs do
// (CommandLineToArgvW's rules for the arguments): blanks separate words
// outside quotes; 2n backslashes before a quote are n and the quote
// toggles quoting, 2n+1 are n and a literal quote; "" inside quotes is a
// literal quote; other backslashes are themselves.
func splitWindowsArgs(s string) []string {
	var out []string
	var w strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			n := j - i
			if j < len(s) && s[j] == '"' {
				w.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					w.WriteByte('"')
					i = j
				} else {
					i = j - 1
				}
			} else {
				w.WriteString(strings.Repeat(`\`, n))
				i = j - 1
			}
			inWord = true
		case c == '"':
			if quoted && i+1 < len(s) && s[i+1] == '"' {
				w.WriteByte('"')
				i++
			} else {
				quoted = !quoted
			}
			inWord = true
		case (c == ' ' || c == '\t') && !quoted:
			if inWord {
				out = append(out, w.String())
				w.Reset()
				inWord = false
			}
		default:
			w.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, w.String())
	}
	return out
}
