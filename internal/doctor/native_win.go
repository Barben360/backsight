package doctor

import (
	"fmt"
	"strings"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/supervise"
)

// The native runtime's checks on Windows (decision 0080): the scheduled
// task and the supervisor in place of the units and linger, then the
// VC++ runtime DLLs, the DACLs of what holds secrets and the path bound.

// nativeWinIDs are the native checks on Windows, in their order.
var nativeWinIDs = []string{"native-task", "supervisor", "native-fsync", "native-identity", "native-stray", "native-home", "native-venv",
	"native-dlls", "native-private", "path-length"}

// restartFix restarts the supervisor: there is no server restart command.
const restartFix = "backsight server stop, then backsight server start"

func checkNativeTask(f NativeFacts) Check {
	c := Check{ID: "native-task"}
	if err := f.Unread[c.ID]; err != nil {
		c.Status, c.Summary, c.Detail = Error, "could not read the scheduled task "+f.TaskName, err.Error()
		return c
	}
	switch f.Task {
	case server.TaskEnabled:
		c.Status, c.Summary = OK, "the scheduled task "+f.TaskName+" is registered and enabled: the server starts at logon"
	case server.TaskDisabled:
		c.Status, c.Summary = Info, "the scheduled task "+f.TaskName+" is disabled (backsight server stop): the server does not start at logon"
		c.Fix = "backsight server start"
	case server.NotInstalled:
		if f.StartOnSession {
			c.Status = Info
			c.Summary = "no scheduled task: Task Scheduler refused it at setup, so each Claude Code session starts the server when it is down (server.start_on_session)"
			return c
		}
		c.Status, c.Summary, c.Fix = Error, "the scheduled task "+f.TaskName+" is not registered: the server does not start at logon", "backsight server start"
	default:
		c.Status, c.Summary = Error, "the scheduled task "+f.TaskName+" is "+f.Task
	}
	return c
}

func checkSupervisor(f NativeFacts) Check {
	c := Check{ID: "supervisor"}
	log := func(what string) string { return f.LogDir + `\` + f.Project + "-" + what + ".log" }
	if err := f.Unread[c.ID]; err != nil {
		c.Status, c.Summary, c.Detail = Error, "the native server's supervisor did not answer", err.Error()
		c.Fix = "see " + log("supervisor")
		return c
	}
	st := f.Supervisor
	if st == nil {
		if f.Task == server.TaskDisabled {
			c.Status, c.Summary = Info, "the native server is stopped (backsight server stop)"
			return c
		}
		c.Status, c.Summary = Error, "no supervisor answers: the native server is not running"
		c.Fix = "backsight server start; if it does not come up, " + log("supervisor") + " says why"
		return c
	}
	switch {
	case st.Refusal != "":
		c.Status, c.Summary, c.Fix = Error, "the supervisor refused to start the database: "+st.Refusal, "backsight server status"
	case st.Proto != supervise.Proto:
		c.Status = Warn
		c.Summary = fmt.Sprintf("the supervisor (pid %d) speaks protocol %d, this backsight %d: restart it", st.PID, st.Proto, supervise.Proto)
		c.Fix = restartFix
	case st.DB != "running" || st.API != "running":
		c.Status = Warn
		c.Summary = fmt.Sprintf("the supervisor (pid %d) answers, but the database is %s and the API is %s", st.PID, st.DB, st.API)
		c.Fix = "see " + log("db") + " and " + log("api")
	case release.NewerThan(buildinfo.Version, st.Version):
		c.Status = Info
		c.Summary = fmt.Sprintf("the supervisor (pid %d) runs backsight %s, older than this one (%s): the newer one runs from its next restart", st.PID, st.Version, buildinfo.Version)
	default:
		c.Status, c.Summary = OK, fmt.Sprintf("the supervisor (pid %d) runs the database and the API", st.PID)
	}
	return c
}

func checkNativeDLLs(f NativeFacts) Check {
	c := Check{ID: "native-dlls"}
	if err := f.Unread[c.ID]; err != nil {
		c.Status, c.Summary, c.Detail, c.Fix = Error, "could not read "+f.DLLManifest, err.Error(), "backsight server install"
		return c
	}
	if len(f.DLLProblems) == 0 {
		c.Status, c.Summary = OK, fmt.Sprintf("the VC++ runtime DLLs' %d copies match %s", f.DLLCopies, f.DLLManifest)
		return c
	}
	shown := f.DLLProblems[:min(len(f.DLLProblems), venvShown)]
	more := ""
	if n := len(f.DLLProblems) - len(shown); n > 0 {
		more = fmt.Sprintf(" and %d more", n)
		c.Detail = strings.Join(f.DLLProblems, "; ")
	}
	c.Status = Error
	c.Summary = "the VC++ runtime DLLs Postgres and the API load differ from " + f.DLLManifest + ": " + strings.Join(shown, ", ") + more
	c.Fix = "backsight server stop, delete " + f.Venv + `\.backsight-complete, then backsight server install (it builds the venv again and copies its DLLs)`
	return c
}

func checkNativePrivate(f NativeFacts) Check {
	c := Check{ID: "native-private"}
	var open, unread []string
	var fix string
	for _, p := range f.Private {
		switch {
		case p.Err != nil:
			unread = append(unread, p.Path+" ("+p.Err.Error()+")")
		case len(p.Readers) > 0:
			open = append(open, p.Path+" can be read by "+strings.Join(p.Readers, ", "))
			if fix == "" {
				fix = icaclsFix(p.Path)
			}
		}
	}
	switch {
	case len(open) > 0:
		c.Status, c.Summary, c.Fix = Error, strings.Join(open, "; ")+": other users can read your memory or its secrets", fix
	case len(unread) > 0:
		c.Status, c.Summary = Warn, "could not read who can read "+strings.Join(unread, ", ")
	default:
		names := make([]string, len(f.Private))
		for i, p := range f.Private {
			names[i] = p.Path
		}
		c.Status, c.Summary = OK, strings.Join(names, ", ")+": private to you, SYSTEM and Administrators"
	}
	return c
}

// icaclsFix makes path private again (MakePrivate's DACL); a directory's
// entries are inherited by what it holds.
func icaclsFix(path string) string {
	return fmt.Sprintf(`icacls "%s" /inheritance:r /grant:r "%%USERNAME%%:(OI)(CI)F" "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F"`, path)
}

func checkPathLength(f NativeFacts) Check {
	c := Check{ID: "path-length"}
	n := setup.VenvPathLength(f.Venv)
	if setup.VenvPathFits(f.Venv) {
		c.Status, c.Summary = OK, fmt.Sprintf("the venv path is %d characters (at most %d)", n, setup.PathBound-setup.VenvDepth)
		return c
	}
	c.Status = Error
	c.Summary = fmt.Sprintf("the venv %s is %d characters: its deepest files, %d below it, pass Windows' path limit", f.Venv, n, setup.VenvDepth)
	c.Fix = setup.ShortHomeFix
	return c
}
