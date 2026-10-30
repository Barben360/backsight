package doctor

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/state"
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
	Venv     string   // the deployed venv
	Pin      string   // the venv this backsight pins (its lock's)
	VenvDiff []string // name==version of the lock missing or other in the deployed venv's dist-info
	Linger   string   // loginctl's Linger: yes | no
	Refused  []string // *.refused-* instance dirs kept by restore
	// Unread: check id → why its fact could not be read.
	Unread map[string]error
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
		out := []Check{{ID: "native-unit", Status: Error, Summary: "could not read the native runtime", Detail: err.Error(), Fix: "backsight server status"}}
		for _, id := range nativeIDs[1:] {
			out = append(out, Check{ID: id, Status: Info, Summary: "skipped: the native runtime could not be read"})
		}
		return append(out, webUICheck())
	}
	out := []Check{
		checkNativeUnits(f), checkNativeFsync(f), checkNativeIdentity(f), checkNativeStrays(f),
		checkNativeLinger(f), checkNativeHome(f), checkNativeVenv(f),
	}
	for _, dir := range f.Refused {
		out = append(out, Check{ID: "native-refused", Status: Info, Summary: "a database restore refused is kept in " + dir,
			Detail: "restore moved it aside, never deleting it: delete it once you no longer need it", Fix: "rm -rf " + dir})
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
	if err := f.Unread[c.ID]; err != nil {
		c.Status, c.Summary, c.Detail = Warn, "could not look for a postgres outside the db unit", err.Error()
		return c
	}
	if len(f.Strays) == 0 {
		c.Status, c.Summary = OK, "no postgres on the native database outside its unit"
		return c
	}
	pids := make([]string, len(f.Strays))
	for i, p := range f.Strays {
		pids[i] = strconv.Itoa(p)
	}
	c.Status = Error
	c.Summary = "a postgres runs on the native database outside its unit (pid " + strings.Join(pids, ", ") + ")"
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
	if m := f.HomeMode.Perm(); m != 0o700 {
		c.Status, c.Summary = Error, fmt.Sprintf("%s has mode %04o, not 0700: other users can read the database", f.Home, m)
		c.Fix = "chmod 700 " + f.Home
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
		c.Fix = "backsight server stop; rm " + filepath.Join(f.Venv, ".backsight-complete") + "; backsight server install (it builds the venv again from the lock)"
		return c
	}
	c.Fix = "backsight server upgrade --allow-unordered"
	return c
}

// checkRuntimeSwitch reports the last runtime switch: a cut-off one as an
// error, else when it ended and the other runtime's data kept since.
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
	entries, err := os.ReadDir(filepath.Join(venv, "lib", "python3.11", "site-packages"))
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
