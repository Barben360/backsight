package scheduler

import (
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
}

func TestScheduledTasks(t *testing.T) {
	p, err := For("windows", Options{Shim: `C:\Users\a\AppData\Local\backsight\bin\current.exe`, Home: `C:\Users\a`})
	if err != nil || p.Kind != "schtasks" || len(p.Files) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	if got := strings.Join(p.Activate[0], "|"); got != `schtasks|/Create|/F|/TN|backsight\tick|/TR|"C:\Users\a\AppData\Local\backsight\bin\current.exe" jobs tick|/SC|MINUTE|/MO|15` {
		t.Errorf("%s", got)
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
