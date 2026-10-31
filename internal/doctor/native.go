package doctor

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/supervise"
)

// NativeFacts are the native runtime's guards as doctor reads them
// (decision 0078, "Doctor on native"); the cli reads them through the
// runtime.
type NativeFacts struct {
	Units    map[string]string // unit → ActiveState (server.NotInstalled without its file)
	Fsync    string            // SHOW fsync, "" when the database does not answer
	Identity error             // CheckInstance's
	Strays   []int
	Home     string // the units' HOME
	HomeMode fs.FileMode
	// HomeACL: the OS keeps access in ACLs, not mode bits (Windows); then
	// HomeReaders, the accounts other than the user, SYSTEM and
	// Administrators that can read Home, replaces HomeMode.
	HomeACL     bool
	HomeReaders []string
	// UserSID is the user's SID (S-1-5-…) on Windows, for the icacls fixes.
	UserSID  string
	Venv     string   // the deployed venv
	Pin      string   // the venv this backsight pins (its lock's)
	VenvDiff []string // name==version of the lock missing or other in the deployed venv's dist-info
	Linger   string   // loginctl's Linger: yes | no
	Refused  []string // *.refused-* instance dirs kept by restore
	// Unread: check id → why its fact could not be read.
	Unread map[string]error

	// Windows (decision 0080): the scheduled task and its supervisor
	// replace the units and linger, and the VC++ runtime DLLs' copies,
	// the DACLs of what holds the secrets and the path bound are checked.
	Windows        bool
	Project        string // server.project: the logs' prefix
	TaskName       string // \<project>\server
	Task           string // server.TaskEnabled, server.TaskDisabled or server.NotInstalled
	StartOnSession bool   // server.start_on_session: Task Scheduler refused the task at setup
	Stopped        bool   // backsight server stop's marker: down on purpose
	// Supervisor is its answer to status, nil when none answers.
	Supervisor  *supervise.Status
	LogDir      string // the supervisor's logs
	DLLManifest string // dlls.json
	DLLProblems []string
	DLLCopies   int
	// Private are the data root, the config directory, secrets.toml and
	// server.env, with the accounts other than the user, SYSTEM and
	// Administrators that can read each.
	Private []PrivatePath
}

// PrivatePath is one path native-private reads, and who else can read it
// (Err: its DACL could not be read).
type PrivatePath struct {
	Path    string
	Dir     bool // a directory: its entries are inherited by what it holds
	Readers []string
	Err     error
}

// nativeIDs are the native checks, in their order.
var nativeIDs = []string{"native-unit", "native-fsync", "native-identity", "native-stray", "native-linger", "native-home", "native-venv"}

// nativeChecks are a native install's checks (decision 0078): its units,
// fsync, the database's identity, strays, linger, home's mode and the
// venv against the lock, then the refused databases restore kept and the
// web UI the runtime lacks.
func nativeChecks(ctx context.Context, probe func(context.Context) (NativeFacts, error)) []Check {
	f, err := probe(ctx)
	if err != nil {
		list := nativeIDs
		if f.Windows {
			list = nativeWinIDs
		}
		out := []Check{{ID: list[0], Status: Error, Summary: "could not read the native runtime", Detail: err.Error(), Fix: "backsight server status"}}
		for _, id := range list[1:] {
			out = append(out, Check{ID: id, Status: Info, Summary: "skipped: the native runtime could not be read"})
		}
		return append(out, webUICheck())
	}
	out := []Check{
		checkNativeUnits(f), checkNativeFsync(f), checkNativeIdentity(f), checkNativeStrays(f),
		checkNativeLinger(f), checkNativeHome(f), checkNativeVenv(f),
	}
	if f.Windows {
		out = []Check{
			checkNativeTask(f), checkSupervisor(f), checkNativeFsync(f), checkNativeIdentity(f), checkNativeStrays(f),
			checkNativeHome(f), checkNativeVenv(f), checkNativeDLLs(f), checkNativePrivate(f), checkPathLength(f),
		}
	}
	for _, dir := range f.Refused {
		out = append(out, Check{ID: "native-refused", Status: Info, Summary: "a database restore refused is kept in " + dir,
			Detail: "restore moved it aside, never deleting it; delete it yourself once you no longer need it: " + removeDirCmd(f.Windows, dir)})
	}
	return append(out, webUICheck())
}

func webUICheck() Check {
	return Check{ID: "web-ui", Status: Info, Summary: "none on the native runtime"}
}

func checkNativeUnits(f NativeFacts) Check {
	c := Check{ID: "native-unit"}
	if err := f.Unread[c.ID]; err != nil {
		c.Status, c.Summary, c.Detail, c.Fix = Error, "could not read the native units' state", err.Error(), "backsight server status"
		return c
	}
	units := slices.Sorted(maps.Keys(f.Units))
	var bad, missing []string
	for _, u := range units {
		switch s := f.Units[u]; s {
		case "active":
		case server.NotInstalled:
			missing = append(missing, u+" is not installed")
		default:
			bad = append(bad, u+" is "+s)
		}
	}
	switch {
	case len(units) == 0:
		c.Status, c.Summary, c.Fix = Error, "no native unit installed", "backsight server install"
	case len(missing) > 0:
		c.Status, c.Summary, c.Fix = Error, strings.Join(append(missing, bad...), ", "), "backsight server install"
	case len(bad) > 0:
		c.Status, c.Summary = Error, strings.Join(bad, ", ")
		c.Fix = "backsight server start; if it fails, journalctl --user -u " + strings.Join(units, " -u ") + " says why"
	default:
		c.Status, c.Summary = OK, strings.Join(units, " and ")+" active"
	}
	return c
}

func checkNativeFsync(f NativeFacts) Check {
	c := Check{ID: "native-fsync"}
	switch f.Fsync {
	case "on":
		c.Status, c.Summary = OK, "fsync is on"
	case "":
		if f.Windows && f.stoppedOnPurpose() {
			c.Status, c.Summary = Info, "not read: the native server is stopped (backsight server stop)"
			return c
		}
		c.Status, c.Summary, c.Fix = Error, "could not read fsync: the native database does not answer", "backsight server start"
		if err := f.Unread[c.ID]; err != nil {
			c.Detail = err.Error()
		}
	default:
		db := "<server.project>-db.service"
		for u := range f.Units {
			if strings.HasSuffix(u, "-db.service") {
				db = u
			}
		}
		c.Status, c.Summary = Error, "fsync is "+f.Fsync+" on the native database, not on: an OS crash could corrupt it"
		c.Fix = "backsight server stop, remove the drop-in that sets it (systemctl --user cat " + db + "), then backsight server render and backsight server start"
		if f.Windows {
			c.Fix = "backsight server stop, then backsight server render (supervise.json gets -c fsync=on back) and backsight server start"
		}
	}
	return c
}

func checkNativeIdentity(f NativeFacts) Check {
	c := Check{ID: "native-identity"}
	if f.Identity == nil {
		c.Status, c.Summary = OK, "the native database is the one backsight recorded"
		return c
	}
	c.Status, c.Summary = Error, f.Identity.Error()
	c.Fix = "backsight restore <archive> (it moves this database aside, never deleting it)"
	return c
}

func checkNativeStrays(f NativeFacts) Check {
	c := Check{ID: "native-stray"}
	holder := "its unit"
	if f.Windows {
		holder = "its supervisor"
	}
	if err := f.Unread[c.ID]; err != nil {
		c.Status, c.Summary, c.Detail = Warn, "could not look for a postgres outside the db unit", err.Error()
		if f.Windows {
			c.Summary = "could not look for a postgres outside the supervisor"
		}
		return c
	}
	if len(f.Strays) == 0 {
		c.Status, c.Summary = OK, "no postgres on the native database outside "+holder
		return c
	}
	pids := make([]string, len(f.Strays))
	for i, p := range f.Strays {
		pids[i] = strconv.Itoa(p)
	}
	c.Status = Error
	c.Summary = "a postgres runs on the native database outside " + holder + " (pid " + strings.Join(pids, ", ") + ")"
	c.Fix = "backsight server start (its native-check stops it, never adopting it)"
	return c
}

func checkNativeLinger(f NativeFacts) Check {
	c := Check{ID: "native-linger", Fix: "loginctl enable-linger"}
	switch f.Linger {
	case "yes":
		c.Status, c.Summary, c.Fix = OK, "linger is on", ""
	case "":
		c.Status, c.Summary = Warn, "could not read linger: without it the native server stops at logout and does not start at boot"
		if err := f.Unread[c.ID]; err != nil {
			c.Detail = err.Error()
		}
	default:
		c.Status, c.Summary = Error, "linger is off: the native server stops at logout and does not start at boot"
	}
	return c
}

func checkNativeHome(f NativeFacts) Check {
	c := Check{ID: "native-home"}
	if err := f.Unread[c.ID]; err != nil {
		c.Status, c.Summary, c.Detail = Error, "could not read "+f.Home, err.Error()
		return c
	}
	if f.HomeACL {
		if len(f.HomeReaders) > 0 {
			c.Status = Error
			c.Summary = fmt.Sprintf("%s can be read by %s: other users can read the database", f.Home, strings.Join(f.HomeReaders, ", "))
			c.Fix = icaclsFix(f.Home, true, f.UserSID, f.HomeReaders)
			return c
		}
		c.Status, c.Summary = OK, f.Home+" is private to you, SYSTEM and Administrators"
		return c
	}
	if m := f.HomeMode.Perm(); m != 0o700 {
		c.Status, c.Summary = Error, fmt.Sprintf("%s has mode %04o, not 0700: other users can read the database", f.Home, m)
		c.Fix = "chmod 700 " + shellQuote(f.Home)
		return c
	}
	c.Status, c.Summary = OK, f.Home+" is 0700"
	return c
}

// venvShown is how many differences the summary names.
const venvShown = 5

func checkNativeVenv(f NativeFacts) Check {
	c := Check{ID: "native-venv"}
	if err := f.Unread[c.ID]; err != nil {
		c.Status, c.Summary, c.Detail = Error, "could not read the venv "+f.Venv, err.Error()
		return c
	}
	if len(f.VenvDiff) == 0 {
		c.Status, c.Summary = OK, "the venv "+f.Venv+" matches the lock"
		return c
	}
	shown := f.VenvDiff[:min(len(f.VenvDiff), venvShown)]
	more := ""
	if n := len(f.VenvDiff) - len(shown); n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	c.Status = Error
	c.Summary = "the venv " + f.Venv + " differs from the lock, which wants " + strings.Join(shown, ", ") + more
	if more != "" {
		c.Detail = "every package of the lock missing or at another version: " + strings.Join(f.VenvDiff, ", ")
	}
	if f.Pin != "" && f.Venv == f.Pin {
		// This backsight's own venv changed since it was built: an upgrade
		// to the version it runs does nothing, a rebuild from the lock does.
		c.Fix = "backsight server stop; rm " + shellQuote(filepath.Join(f.Venv, ".backsight-complete")) + "; backsight server install (it builds the venv again from the lock)"
		if f.Windows {
			c.Fix = "in PowerShell: backsight server stop; Remove-Item -Force " + server.PSQuote(f.Venv+`\.backsight-complete`) +
				"; backsight server install (it builds the venv again from the lock)"
		}
		return c
	}
	c.Fix = "backsight server upgrade --allow-unordered"
	return c
}

// checkRuntimeSwitch reports the last runtime switch: a cut-off one as an
// error, else when it ended, its recall overlap beyond near-ties (issue
// #123; none for a switch recorded before it) and the other runtime's data
// kept since.
func checkRuntimeSwitch(rs *state.RuntimeSwitch, kept string) Check {
	c := Check{ID: "runtime-switch", Status: Info}
	when := rs.At.Local().Format("2006-01-02 15:04")
	switch {
	case rs.Step == "running" && rs.Running():
		c.Summary = "a runtime switch to " + rs.To + " is running (pid " + strconv.Itoa(rs.PID) + ")"
		return c
	case rs.Step == "running":
		c.Status = Error
		c.Summary = "a runtime switch to " + rs.To + " was cut off: run backsight server runtime switch " + rs.To + ", which first puts " + rs.From + " back"
		c.Fix = "backsight server runtime switch " + rs.To
		return c
	case rs.Step == "rolled-back":
		c.Summary = "the switch from " + rs.From + " to " + rs.To + " was rolled back on " + when + ": the memory stays on " + rs.From
	default:
		c.Summary = "switched from " + rs.From + " to " + rs.To + " on " + when
		if rs.OverlapBeyondTies > 0 {
			c.Summary += fmt.Sprintf(", recall overlap@10 %.2f beyond near-ties", rs.OverlapBeyondTies)
		}
	}
	if kept != "" {
		c.Detail = kept + " kept from before the switch: backsight server runtime clean deletes it"
		c.Fix = "backsight server runtime clean"
	}
	return c
}

// NativeVenvDiff compares a venv's installed distributions (the names of
// its site-packages' *.dist-info, no Python run) with the lock: each
// locked package missing or at another version, as the lock's
// name==version, sorted. Packages the lock does not name are no
// difference.
func NativeVenvDiff(venv string) ([]string, error) {
	entries, err := os.ReadDir(sitePackages(venv))
	if err != nil {
		return nil, err
	}
	have := map[string]string{}
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".dist-info")
		if !ok || !e.IsDir() {
			continue
		}
		if i := strings.LastIndex(base, "-"); i > 0 {
			have[nativelock.Normalize(base[:i])] = base[i+1:]
		}
	}
	var diff []string
	for name, v := range nativelock.Versions() {
		if have[name] != v {
			diff = append(diff, name+"=="+v)
		}
	}
	slices.Sort(diff)
	return diff, nil
}

// sitePackages is venv's site-packages: lib/python3.11/site-packages,
// Lib\site-packages on Windows.
func sitePackages(venv string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Lib", "site-packages")
	}
	return filepath.Join(venv, "lib", "python3.11", "site-packages")
}

// removeDirCmd deletes dir: rm -rf, or PowerShell's Remove-Item on
// Windows.
func removeDirCmd(windows bool, dir string) string {
	if windows {
		return "in PowerShell, Remove-Item -Recurse -Force " + server.PSQuote(dir)
	}
	return "rm -rf " + shellQuote(dir)
}

// shellQuote is s as one POSIX shell word, for a path spliced into a
// command doctor prints (compose_dir may hold a space or a quote). The
// same rule as the cli's shellQuote, which doctor cannot import.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
