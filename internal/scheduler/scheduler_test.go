package scheduler

import (
	"fmt"
	"strings"
	"testing"
)

func TestSystemdUnits(t *testing.T) {
	p, err := For("linux", Options{Shim: "/home/a/.local/share/backsight/bin/current", Home: "/home/a", UnitDir: "/home/a/.config/systemd/user",
		Env: map[string]string{"BACKSIGHT_HOME": "/srv/bs 1", "PATH": "/usr/bin:/home/a/.local/bin"}})
	if err != nil || p.Kind != "systemd" || len(p.Files) != 4 {
		t.Fatalf("%+v %v", p, err)
	}
	files := map[string]string{}
	for _, f := range p.Files {
		files[f.Path] = f.Content
	}
	if svc := files["/home/a/.config/systemd/user/backsight-tick.service"]; !strings.Contains(svc, "Environment=\"BACKSIGHT_HOME=/srv/bs 1\"\n") ||
		!strings.Contains(svc, "Environment=PATH=/usr/bin:/home/a/.local/bin\n") || !strings.Contains(svc, "TimeoutStartSec=30min") {
		t.Errorf("the install's environment is not carried:\n%s", svc)
	}
	// A stop (a timeout, a shutdown) signals the job only, then waits long
	// enough for an update's rollback before killing what is left.
	for _, name := range []string{"tick", "maintain"} {
		if svc := files["/home/a/.config/systemd/user/backsight-"+name+".service"]; !strings.Contains(svc, "KillMode=mixed\n") || !strings.Contains(svc, "TimeoutStopSec=45min\n") {
			t.Errorf("%s stop policy:\n%s", name, svc)
		}
	}
	tick := files["/home/a/.config/systemd/user/backsight-tick.timer"]
	if !strings.Contains(tick, "OnCalendar=*:0/15\n") || !strings.Contains(tick, "Persistent=true") || !strings.Contains(tick, "WantedBy=timers.target") {
		t.Errorf("tick timer:\n%s", tick)
	}
	if m := files["/home/a/.config/systemd/user/backsight-maintain.timer"]; !strings.Contains(m, "OnCalendar=*-*-* 04:00:00\n") || !strings.Contains(m, "RandomizedDelaySec=30min") {
		t.Errorf("maintain timer:\n%s", m)
	}
	if s := files["/home/a/.config/systemd/user/backsight-tick.service"]; !strings.Contains(s, "ExecStart=/home/a/.local/share/backsight/bin/current jobs tick\n") || !strings.Contains(s, "Type=oneshot") {
		t.Errorf("service:\n%s", s)
	}
	if strings.Join(p.Activate[1], " ") != "systemctl --user enable --now backsight-tick.timer backsight-maintain.timer" ||
		strings.Join(p.Remove[0], " ") != "systemctl --user disable --now backsight-tick.timer backsight-maintain.timer" {
		t.Errorf("%q %q", p.Activate, p.Remove)
	}
	p, _ = For("linux", Options{Shim: "/home/a b/50%/current", Home: "/home/a b", UnitDir: "/u"})
	for _, f := range p.Files {
		if strings.HasSuffix(f.Path, "tick.service") && !strings.Contains(f.Content, `ExecStart="/home/a b/50%%/current" jobs tick`) {
			t.Errorf("a shim with a space and a %% is quoted and escaped:\n%s", f.Content)
		}
	}
	p, _ = For("linux", Options{Shim: "/s", Home: "/h", UnitDir: "/u", Env: map[string]string{"BACKSIGHT_HOME": `/srv/say "hi"\x`}})
	if s := p.Files[0].Content; !strings.Contains(s, `Environment="BACKSIGHT_HOME=/srv/say \"hi\"\\x"`+"\n") {
		t.Errorf("a quote and a backslash in an env value are escaped inside the quotes:\n%s", s)
	}
}

func TestLaunchdAgents(t *testing.T) {
	p, err := For("darwin", Options{Shim: "/Users/a/Library/Application Support/backsight/bin/current", Home: "/Users/a", UID: 501,
		LogDir: "/Users/a/Library/Application Support/backsight/state/logs", Env: map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin"}})
	if err != nil || p.Kind != "launchd" || len(p.Files) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	if p.Files[0].Path != "/Users/a/Library/LaunchAgents/tech.backsight.tick.plist" ||
		!strings.Contains(p.Files[0].Content, "<string>/Users/a/Library/Application Support/backsight/bin/current</string><string>jobs</string><string>tick</string>") ||
		!strings.Contains(p.Files[0].Content, "<integer>900</integer>") || !strings.Contains(p.Files[1].Content, "StartCalendarInterval") ||
		!strings.Contains(p.Files[0].Content, "<key>PATH</key><string>/opt/homebrew/bin:/usr/bin</string>") ||
		!strings.Contains(p.Files[0].Content, "<key>StandardErrorPath</key><string>/Users/a/Library/Application Support/backsight/state/logs/jobs-tick.log</string>") {
		t.Errorf("%s\n%s", p.Files[0].Content, p.Files[1].Content)
	}
	if strings.Join(p.Activate[1], " ") != "launchctl bootstrap gui/501 /Users/a/Library/LaunchAgents/tech.backsight.tick.plist" {
		t.Errorf("%q", p.Activate)
	}
	p, _ = For("darwin", Options{Shim: "/Users/a&b/bin/current", Home: "/Users/a&b", UID: 501, LogDir: "/Users/a&b/logs",
		Env: map[string]string{"PATH": "/opt/<x>:/usr/bin"}})
	c := p.Files[0].Content
	for _, want := range []string{"<string>/Users/a&amp;b/bin/current</string>", "<string>/opt/&lt;x&gt;:/usr/bin</string>",
		"<key>StandardOutPath</key><string>/Users/a&amp;b/logs/jobs-tick.log</string>"} {
		if !strings.Contains(c, want) {
			t.Errorf("%s is not escaped for XML:\n%s", want, c)
		}
	}
	if strings.Contains(c, "a&b") || strings.Contains(c, "<x>") {
		t.Errorf("a raw & or < is left in the plist:\n%s", c)
	}
}

func TestScheduledTasks(t *testing.T) {
	p, err := For("windows", Options{Shim: `C:\Users\a\AppData\Local\backsight\bin\current.exe`, Home: `C:\Users\a`})
	if err != nil || p.Kind != "schtasks" || len(p.Files) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	if got := strings.Join(p.Activate[0], "|"); got != `schtasks|/Create|/F|/TN|backsight\tick|/TR|"C:\Users\a\AppData\Local\backsight\bin\current.exe" jobs tick|/SC|MINUTE|/MO|15` {
		t.Errorf("%s", got)
	}
	if got := strings.Join(p.Activate[1], "|"); got != `schtasks|/Create|/F|/TN|backsight\maintain|/TR|"C:\Users\a\AppData\Local\backsight\bin\current.exe" jobs maintain|/SC|DAILY|/ST|04:00` {
		t.Errorf("maintain: %s", got)
	}
	if _, err := For("plan9", Options{Shim: "/x"}); err == nil {
		t.Error("an unknown system is refused")
	}
}

// A second install (another Compose project) gets entries of its own.
func TestEntriesAreNamedAfterTheProject(t *testing.T) {
	o := Options{Shim: "/s", Home: "/h", UnitDir: "/u", Name: "bs-fresh"}
	p, _ := For("linux", o)
	got := strings.Join(p.Paths(), " ")
	if !strings.Contains(got, "/u/bs-fresh-tick.timer") || strings.Contains(got, "backsight-") {
		t.Errorf("systemd files %s", got)
	}
	if a := strings.Join(p.Activate[1], " "); !strings.Contains(a, "bs-fresh-maintain.timer") {
		t.Errorf("activate %s", a)
	}
	if l, _ := For("darwin", o); !strings.Contains(strings.Join(l.Remove[0], " "), "tech.bs-fresh.tick") {
		t.Errorf("launchd %v", l.Remove)
	}
	if w, _ := For("windows", o); !strings.Contains(strings.Join(w.Remove[0], " "), `bs-fresh\tick`) {
		t.Errorf("schtasks %v", w.Remove)
	}
	if d, _ := For("linux", Options{Shim: "/s", Home: "/h", UnitDir: "/u"}); !strings.Contains(strings.Join(d.Paths(), " "), "/u/backsight-tick.timer") {
		t.Error("the default names changed")
	}
}

// A launchd stop waits as long as a systemd one before its SIGKILL, so an
// update's rollback is not cut short (launchd's default is 20 s).
func TestLaunchdStopTimeout(t *testing.T) {
	p, _ := For("darwin", Options{Shim: "/s", Home: "/h", UID: 501, LogDir: "/l"})
	for _, f := range p.Files {
		if !strings.Contains(f.Content, "<key>ExitTimeOut</key><integer>2700</integer>") {
			t.Errorf("%s:\n%s", f.Path, f.Content)
		}
	}
}

// The environment an entry was installed with is read back from its
// files, so that a refresh renders the new template with it (and not
// with the refreshing shell's PATH).
func TestInstalledEnvRoundTrips(t *testing.T) {
	env := map[string]string{"BACKSIGHT_HOME": `/srv/say "hi"\x 50%`, "PATH": "/opt/<x>&y:/usr/bin", "XDG_STATE_HOME": "/s t"}
	for _, goos := range []string{"linux", "darwin"} {
		p, _ := For(goos, Options{Shim: "/s", Home: "/h", UnitDir: "/u", UID: 501, LogDir: "/l", Env: env})
		got, ok := InstalledEnv(p.Files[0].Path, p.Files[0].Content)
		if !ok || len(got) != len(env) {
			t.Fatalf("%s: %v %q", goos, ok, got)
		}
		for k, v := range env {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", goos, k, got[k], v)
			}
		}
	}
	// No environment at all is an environment (an empty one).
	p, _ := For("linux", Options{Shim: "/s", Home: "/h", UnitDir: "/u"})
	if got, ok := InstalledEnv(p.Files[0].Path, p.Files[0].Content); !ok || len(got) != 0 {
		t.Errorf("empty: %v %q", ok, got)
	}
	if _, ok := InstalledEnv("/u/backsight-tick.timer", p.Files[1].Content); ok {
		t.Error("a timer carries no environment")
	}
}

// Each entry names its files, how to reload them and, where reloading
// stops a running job, how to tell that it runs.
func TestEntries(t *testing.T) {
	s, _ := For("linux", Options{Shim: "/s", Home: "/h", UnitDir: "/u"})
	if len(s.Entries) != 2 || len(s.Entries[0].Files) != 2 || s.Entries[0].Running != nil || len(s.Entries[0].Reload) != 0 {
		t.Errorf("systemd entries %+v", s.Entries)
	}
	if e := s.Entries[1]; strings.Join(e.Enabled, " ") != "systemctl --user is-enabled backsight-maintain.timer" {
		t.Errorf("systemd enabled probe %q", e.Enabled)
	}
	// Only the timers that were enabled are enabled again: a timer the
	// user disabled stays disabled, and the units are re-read anyway.
	if got := fmt.Sprintf("%q", s.Reloaded(s.Entries[1:])); got != `[["systemctl" "--user" "daemon-reload"] ["systemctl" "--user" "enable" "--now" "backsight-maintain.timer"]]` {
		t.Errorf("systemd reload of maintain: %s", got)
	}
	if got := fmt.Sprintf("%q", s.Reloaded(nil)); got != `[["systemctl" "--user" "daemon-reload"]]` {
		t.Errorf("systemd reload with no timer enabled: %s", got)
	}
	l, _ := For("darwin", Options{Shim: "/s", Home: "/h", UID: 501, LogDir: "/l"})
	e := l.Entries[1]
	if e.Job != "maintain" || strings.Join(e.Files, " ") != "/h/Library/LaunchAgents/tech.backsight.maintain.plist" ||
		strings.Join(e.Running, " ") != "launchctl print gui/501/tech.backsight.maintain" || e.RunningMark != "state = running" ||
		len(e.Reload) != 2 || strings.Join(e.Reload[1], " ") != "launchctl bootstrap gui/501 /h/Library/LaunchAgents/tech.backsight.maintain.plist" {
		t.Errorf("launchd entry %+v", e)
	}
	if strings.Join(e.Enabled, " ") != "launchctl print gui/501/tech.backsight.maintain" || l.Reloaded(l.Entries) != nil {
		t.Errorf("launchd loaded probe %q, plan reload %q", e.Enabled, l.Reloaded(l.Entries))
	}
	w, _ := For("windows", Options{Shim: `C:\s.exe`, Home: `C:\h`})
	if e := w.Entries[0]; len(e.Files) != 0 || strings.Join(e.Running, " ") != `schtasks /Query /TN backsight\tick /FO LIST` ||
		e.RunningMark != "Running" || strings.Join(e.Reload[0], " ") != strings.Join(w.Activate[0], " ") {
		t.Errorf("schtasks entry %+v", e)
	}
	if w.Entries[0].Enabled != nil || w.Reloaded(w.Entries) != nil {
		t.Errorf("tasks: enabled probe %q, plan reload %q", w.Entries[0].Enabled, w.Reloaded(w.Entries))
	}
	if w.Digest() == "" || w.Digest() == s.Digest() {
		t.Error("digest")
	}
	w2, _ := For("windows", Options{Shim: `C:\other.exe`, Home: `C:\h`})
	if w.Digest() == w2.Digest() {
		t.Error("the digest ignores the commands")
	}
}

// The entries a project installed keep the names they were installed
// under: their removal is read from the recorded files, whatever the
// project is called now.
func TestInstalledFromRecordedFiles(t *testing.T) {
	old, _ := For("linux", Options{Shim: "/s", Home: "/h", UnitDir: "/u", Name: "old-name"})
	in := InstalledFrom(old.Paths())
	if strings.Join(in.Timers, " ") != "old-name-tick.timer old-name-maintain.timer" || strings.Join(in.Services, " ") != "old-name-tick.service old-name-maintain.service" {
		t.Errorf("systemd names %+v", in)
	}
	if got := fmt.Sprintf("%q", in.Remove("systemd", 501)); got != fmt.Sprintf("%q", old.Remove) {
		t.Errorf("systemd remove %s, want %q", got, old.Remove)
	}
	l, _ := For("darwin", Options{Shim: "/s", Home: "/h", UID: 501, LogDir: "/l", Name: "old-name"})
	in = InstalledFrom(l.Paths())
	if strings.Join(in.Labels, " ") != "tech.old-name.tick tech.old-name.maintain" {
		t.Errorf("launchd labels %+v", in)
	}
	if got := fmt.Sprintf("%q", in.Remove("launchd", 501)); got != fmt.Sprintf("%q", l.Remove) {
		t.Errorf("launchd remove %s, want %q", got, l.Remove)
	}
	// Files that are not entries name nothing.
	if in := InstalledFrom([]string{"/u/notes.txt", "/u/x.conf"}); len(in.Timers)+len(in.Services)+len(in.Labels) != 0 || in.Remove("systemd", 1) != nil {
		t.Errorf("foreign files %+v", in)
	}
}
