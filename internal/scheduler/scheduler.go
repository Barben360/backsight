// Package scheduler describes the two OS scheduler entries (decision 0025):
// `jobs tick` every 15 minutes and `jobs maintain` daily, as systemd user
// timers (Linux), launchd agents (macOS) or Task Scheduler tasks (Windows).
// It builds files and commands (and reads whether a timer is enabled);
// the caller writes the files through
// the config editor (pre-images, undo) and runs the commands.
package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Job is one scheduled entry.
type Job struct {
	Name    string // tick | maintain
	Minutes int    // cadence
}

// Jobs are the two entries, in install order.
var Jobs = []Job{{"tick", 15}, {"maintain", 24 * 60}}

// File is a file an entry needs.
type File struct {
	Path    string
	Content string
}

// Plan is what installing (or removing) the entries takes on one system.
type Plan struct {
	Kind     string     `json:"kind"` // systemd | launchd | schtasks
	Files    []File     `json:"-"`
	Activate [][]string `json:"activate"` // commands, after the files are written
	// Start runs a job once now, through the scheduler (its environment,
	// its log), without waiting for it: setup's first tick.
	Start  map[string][]string `json:"-"`
	Remove [][]string          `json:"remove"` // commands, before the files are removed
	// Entries and Reloaded are what a refresh (update apply) takes:
	// rewrite the entries whose files changed and load them again without
	// stopping a running job.
	Entries []Entry `json:"-"`
	// unit is systemd's timer of each entry, for Reloaded.
	unit map[string]string
}

// Entry is one job's part of a plan.
type Entry struct {
	Job   string
	Files []string // its files in Plan.Files (none for a task)
	// Running prints RunningMark while the job runs, when loading the
	// entry again would stop it (launchd's bootout, a replaced task);
	// nil when it never does (systemd re-reads units on daemon-reload and
	// a running service keeps running).
	Running     []string
	RunningMark string
	// Enabled exits 0 while the entry is enabled (a systemd timer) or
	// loaded (a launchd agent); nil when there is no such state to keep
	// (a task is created again). A refresh loads again only the entries
	// that were: one the user disabled stays disabled.
	Enabled []string
	Reload  [][]string // loads the rewritten entry, when it was enabled
}

// Reloaded is what a refresh runs after the entries' own reloads, once
// something was rewritten: systemd's daemon-reload, then enable --now of
// the timers of enabled (the entries enabled before the refresh). Neither
// stops a running service: daemon-reload re-reads the units (a running
// job keeps its process), enable --now starts timers only.
func (p Plan) Reloaded(enabled []Entry) [][]string {
	if p.Kind != "systemd" {
		return nil
	}
	out := [][]string{{"systemctl", "--user", "daemon-reload"}}
	enable := []string{"systemctl", "--user", "enable", "--now"}
	for _, e := range enabled {
		if u := p.unit[e.Job]; u != "" {
			enable = append(enable, u)
		}
	}
	if len(enable) > 4 {
		out = append(out, enable)
	}
	return out
}

// Digest identifies the activation commands. Tasks have no files to
// compare, so the install records it and a refresh compares it.
func (p Plan) Digest() string {
	h := sha256.New()
	for _, c := range p.Activate {
		h.Write([]byte(strings.Join(c, "\x00") + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Paths lists the plan's files.
func (p Plan) Paths() []string {
	out := make([]string, 0, len(p.Files))
	for _, f := range p.Files {
		out = append(out, f.Path)
	}
	return out
}

// Options are what the entries need from the install.
type Options struct {
	Shim string // absolute path of the backsight shim
	Home string
	// UnitDir is where the systemd user manager reads units (its own
	// XDG_CONFIG_HOME, else ~/.config, + systemd/user).
	UnitDir string
	UID     int    // launchd's gui/<uid> domain
	LogDir  string // launchd's stdout/stderr files
	// Name prefixes every entry (unit, label, task): the Compose project,
	// so that a second install does not replace the first's entries.
	// "" means "backsight".
	Name string
	// Env is carried into the job: the directories the install resolved
	// (BACKSIGHT_HOME, XDG_*_HOME) and PATH, so that the job finds the same
	// config, state and docker as the shell that installed it (not on
	// Windows yet: see schtasks).
	Env map[string]string
}

// For builds the plan for goos.
func For(goos string, o Options) (Plan, error) {
	switch goos {
	case "linux":
		return systemd(o), nil
	case "darwin":
		return launchd(o), nil
	case "windows":
		return schtasks(o), nil
	}
	return Plan{}, fmt.Errorf("no scheduler support on %s", goos)
}

func envKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (o Options) name() string {
	if o.Name == "" {
		return "backsight"
	}
	return o.Name
}

// UnitName is the systemd unit of job j (without suffix).
func (o Options) UnitName(j Job) string { return o.name() + "-" + j.Name }

// stopPolicy: a stop (the start timeout, a shutdown) sends SIGTERM to the
// job only, which cancels its context; an update apply then signals its
// child, which rolls back (up to 30 minutes, then its own client install
// rollback). Whatever is left is killed after TimeoutStopSec (decision
// 0067 block 2 review). The default control-group mode would signal every
// process of the unit, the rollback's docker calls included.
const stopPolicy = "KillMode=mixed\nTimeoutStopSec=45min\n"

func systemd(o Options) Plan {
	dir := o.UnitDir
	p := Plan{Kind: "systemd", unit: map[string]string{}, Start: map[string][]string{}}
	var env strings.Builder
	for _, k := range envKeys(o.Env) {
		env.WriteString("Environment=" + systemdQuote(k+"="+o.Env[k]) + "\n")
	}
	var timers []string
	for _, j := range Jobs {
		name := o.UnitName(j)
		calendar := "*:0/15"
		extra := ""
		if j.Name == "maintain" {
			calendar, extra = "*-*-* 04:00:00", "RandomizedDelaySec=30min\n"
		}
		p.Files = append(p.Files,
			File{filepath.Join(dir, name+".service"), "# Generated by backsight jobs install: do not edit.\n[Unit]\nDescription=backsight jobs " + j.Name +
				"\n\n[Service]\nType=oneshot\nTimeoutStartSec=30min\n" + stopPolicy + env.String() + "ExecStart=" + systemdQuote(o.Shim) + " jobs " + j.Name + "\n"},
			File{filepath.Join(dir, name+".timer"), "# Generated by backsight jobs install: do not edit.\n[Unit]\nDescription=backsight jobs " + j.Name +
				"\n\n[Timer]\nOnCalendar=" + calendar + "\nPersistent=true\n" + extra + "\n[Install]\nWantedBy=timers.target\n"})
		timers = append(timers, name+".timer")
		p.Start[j.Name] = []string{"systemctl", "--user", "start", "--no-block", name + ".service"}
		p.Entries = append(p.Entries, Entry{Job: j.Name, Files: []string{filepath.Join(dir, name+".service"), filepath.Join(dir, name+".timer")},
			Enabled: []string{"systemctl", "--user", "is-enabled", name + ".timer"}})
		p.unit[j.Name] = name + ".timer"
	}
	p.Activate = [][]string{{"systemctl", "--user", "daemon-reload"}, append([]string{"systemctl", "--user", "enable", "--now"}, timers...)}
	p.Remove = InstalledFrom(p.Paths()).Remove(p.Kind, o.UID)
	return p
}

// systemdQuote makes s one word for a unit file: % is systemd's specifier
// prefix (doubled), and a space, quote or backslash needs quoting.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if strings.ContainsAny(s, " \t\"\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

// Label is the launchd label of job j.
func (o Options) Label(j Job) string { return "tech." + o.name() + "." + j.Name }

func launchd(o Options) Plan {
	dir := filepath.Join(o.Home, "Library", "LaunchAgents")
	p := Plan{Kind: "launchd", Start: map[string][]string{}}
	domain := fmt.Sprintf("gui/%d", o.UID)
	var env strings.Builder
	if len(o.Env) > 0 {
		env.WriteString("<key>EnvironmentVariables</key><dict>")
		for _, k := range envKeys(o.Env) {
			env.WriteString("<key>" + xmlEscape(k) + "</key><string>" + xmlEscape(o.Env[k]) + "</string>")
		}
		env.WriteString("</dict>\n")
	}
	for _, j := range Jobs {
		path := filepath.Join(dir, o.Label(j)+".plist")
		schedule := "<key>StartInterval</key><integer>900</integer>"
		if j.Name == "maintain" {
			schedule = "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>4</integer><key>Minute</key><integer>0</integer></dict>"
		}
		p.Files = append(p.Files, File{path, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Generated by backsight jobs install: do not edit. -->
<plist version="1.0"><dict>
<key>Label</key><string>` + o.Label(j) + `</string>
<key>ProgramArguments</key><array><string>` + xmlEscape(o.Shim) + `</string><string>jobs</string><string>` + j.Name + `</string></array>
` + schedule + `
` + env.String() + `<key>StandardOutPath</key><string>` + xmlEscape(filepath.Join(o.LogDir, "jobs-"+j.Name+".log")) + `</string>
<key>StandardErrorPath</key><string>` + xmlEscape(filepath.Join(o.LogDir, "jobs-"+j.Name+".log")) + `</string>
<key>RunAtLoad</key><false/>
<key>ExitTimeOut</key><integer>2700</integer>
</dict></plist>
`})
		// bootout first: bootstrap fails on an agent already loaded
		load := [][]string{{"launchctl", "bootout", domain + "/" + o.Label(j)}, {"launchctl", "bootstrap", domain, path}}
		p.Activate = append(p.Activate, load...)
		p.Start[j.Name] = []string{"launchctl", "kickstart", domain + "/" + o.Label(j)}
		// bootout stops a running job: a refresh leaves that one for later
		probe := []string{"launchctl", "print", domain + "/" + o.Label(j)}
		p.Entries = append(p.Entries, Entry{Job: j.Name, Files: []string{path}, Running: probe,
			RunningMark: "state = running", Enabled: probe, Reload: load})
	}
	p.Remove = InstalledFrom(p.Paths()).Remove(p.Kind, o.UID)
	return p
}

// Installed names the entries written to files (state.json
// scheduler.files): a project renamed since keeps its entries' names,
// and jobs uninstall removes those.
type Installed struct {
	Timers   []string // systemd timers (name.timer)
	Services []string // systemd services (name.service)
	Labels   []string // launchd labels
}

// InstalledFrom reads the names from the files' own names; a file that is
// no entry's names nothing.
func InstalledFrom(files []string) Installed {
	var in Installed
	for _, f := range files {
		base := filepath.Base(f)
		switch {
		case strings.HasSuffix(base, ".timer"):
			in.Timers = append(in.Timers, base)
		case strings.HasSuffix(base, ".service"):
			in.Services = append(in.Services, base)
		case strings.HasSuffix(base, ".plist"):
			in.Labels = append(in.Labels, strings.TrimSuffix(base, ".plist"))
		}
	}
	return in
}

// TimersEnabled reports whether a systemd timer among files may be
// enabled: enable links it into timers.target.wants beside the units. It
// reads the disk only, so it answers without a user session; a link it
// cannot read counts as enabled.
func TimersEnabled(files []string) bool {
	for _, f := range files {
		if !strings.HasSuffix(f, ".timer") {
			continue
		}
		_, err := os.Lstat(filepath.Join(filepath.Dir(f), "timers.target.wants", filepath.Base(f)))
		if !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// Remove deactivates the entries for kind (none for tasks: they have no
// files to name them; their plan's Remove does).
func (in Installed) Remove(kind string, uid int) [][]string {
	var out [][]string
	switch kind {
	case "systemd":
		if len(in.Timers) > 0 {
			out = append(out, append([]string{"systemctl", "--user", "disable", "--now"}, in.Timers...))
		}
	case "launchd":
		for _, l := range in.Labels {
			out = append(out, []string{"launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, l)})
		}
	}
	return out
}

var (
	launchdEnv  = regexp.MustCompile(`<key>EnvironmentVariables</key><dict>(.*?)</dict>`)
	launchdPair = regexp.MustCompile(`<key>(.*?)</key><string>(.*?)</string>`)
)

// InstalledEnv reads back the environment a generated service (systemd)
// or agent (launchd) file carries; ok is false for any other file.
func InstalledEnv(path, content string) (env map[string]string, ok bool) {
	env = map[string]string{}
	switch {
	case strings.HasSuffix(path, ".service"):
		for _, l := range strings.Split(content, "\n") {
			v, found := strings.CutPrefix(l, "Environment=")
			if !found {
				continue
			}
			if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
				var b strings.Builder
				for i := 1; i < len(v)-1; i++ {
					if v[i] == '\\' && i+1 < len(v)-1 {
						i++
					}
					b.WriteByte(v[i])
				}
				v = b.String()
			}
			if k, val, found := strings.Cut(strings.ReplaceAll(v, "%%", "%"), "="); found {
				env[k] = val
			}
		}
		return env, true
	case strings.HasSuffix(path, ".plist"):
		if m := launchdEnv.FindStringSubmatch(content); m != nil {
			for _, kv := range launchdPair.FindAllStringSubmatch(m[1], -1) {
				env[xmlUnescape(kv[1])] = xmlUnescape(kv[2])
			}
		}
		return env, true
	}
	return nil, false
}

func xmlUnescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// TaskName is the Task Scheduler name of job j.
func (o Options) TaskName(j Job) string { return o.name() + `\` + j.Name }

// schtasks does not carry o.Env yet: the task runs with the user's own
// environment, so a BACKSIGHT_HOME, XDG_*_HOME or PATH set only in the
// installing shell is lost (systemd and launchd get them). /TR is capped
// at 261 characters, too short for PATH inline; the fix is a generated
// .cmd wrapper (set "K=V" lines, % doubled), deferred to M7 (Windows),
// where it can be tried on Task Scheduler.
func schtasks(o Options) Plan {
	shim := o.Shim
	p := Plan{Kind: "schtasks", Start: map[string][]string{}}
	for _, j := range Jobs {
		run := `"` + shim + `" jobs ` + j.Name
		sched := []string{"/SC", "MINUTE", "/MO", "15"}
		if j.Name == "maintain" {
			sched = []string{"/SC", "DAILY", "/ST", "04:00"}
		}
		create := append([]string{"schtasks", "/Create", "/F", "/TN", o.TaskName(j), "/TR", run}, sched...)
		p.Activate = append(p.Activate, create)
		p.Start[j.Name] = []string{"schtasks", "/Run", "/TN", o.TaskName(j)}
		// Not tried on Windows yet (M7): a refresh leaves a running task
		// alone rather than rely on /Create /F keeping its instance.
		p.Entries = append(p.Entries, Entry{Job: j.Name, Running: []string{"schtasks", "/Query", "/TN", o.TaskName(j), "/FO", "LIST"},
			RunningMark: "Running", Reload: [][]string{create}})
		p.Remove = append(p.Remove, []string{"schtasks", "/Delete", "/F", "/TN", o.TaskName(j)})
	}
	return p
}
