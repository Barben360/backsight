package scheduler

import (
	"strings"
	"testing"
)

func TestSystemdUnits(t *testing.T) {
	p, err := For("linux", "/home/a/.local/share/backsight/bin/current", "/home/a", "/home/a/.config", 1000)
	if err != nil || p.Kind != "systemd" || len(p.Files) != 4 {
		t.Fatalf("%+v %v", p, err)
	}
	files := map[string]string{}
	for _, f := range p.Files {
		files[f.Path] = f.Content
	}
	tick := files["/home/a/.config/systemd/user/backsight-tick.timer"]
	if !strings.Contains(tick, "OnCalendar=*:0/15\n") || !strings.Contains(tick, "Persistent=true") || !strings.Contains(tick, "WantedBy=timers.target") {
		t.Errorf("tick timer:\n%s", tick)
	}
	if m := files["/home/a/.config/systemd/user/backsight-maintain.timer"]; !strings.Contains(m, "OnCalendar=daily\n") || !strings.Contains(m, "RandomizedDelaySec=1h") {
		t.Errorf("maintain timer:\n%s", m)
	}
	if s := files["/home/a/.config/systemd/user/backsight-tick.service"]; !strings.Contains(s, "ExecStart=/home/a/.local/share/backsight/bin/current jobs tick\n") || !strings.Contains(s, "Type=oneshot") {
		t.Errorf("service:\n%s", s)
	}
	if strings.Join(p.Activate[1], " ") != "systemctl --user enable --now backsight-tick.timer backsight-maintain.timer" ||
		strings.Join(p.Remove[0], " ") != "systemctl --user disable --now backsight-tick.timer backsight-maintain.timer" {
		t.Errorf("%q %q", p.Activate, p.Remove)
	}
	p, _ = For("linux", "/home/a b/current", "/home/a b", "/home/a b/.config", 1000)
	for _, f := range p.Files {
		if strings.HasSuffix(f.Path, "tick.service") && !strings.Contains(f.Content, `ExecStart="/home/a b/current" jobs tick`) {
			t.Errorf("a shim with a space is quoted:\n%s", f.Content)
		}
	}
}

func TestLaunchdAgents(t *testing.T) {
	p, err := For("darwin", "/Users/a/Library/Application Support/backsight/bin/current", "/Users/a", "", 501)
	if err != nil || p.Kind != "launchd" || len(p.Files) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	if p.Files[0].Path != "/Users/a/Library/LaunchAgents/tech.backsight.tick.plist" ||
		!strings.Contains(p.Files[0].Content, "<string>/Users/a/Library/Application Support/backsight/bin/current</string><string>jobs</string><string>tick</string>") ||
		!strings.Contains(p.Files[0].Content, "<integer>900</integer>") || !strings.Contains(p.Files[1].Content, "StartCalendarInterval") {
		t.Errorf("%s\n%s", p.Files[0].Content, p.Files[1].Content)
	}
	if strings.Join(p.Activate[1], " ") != "launchctl bootstrap gui/501 /Users/a/Library/LaunchAgents/tech.backsight.tick.plist" {
		t.Errorf("%q", p.Activate)
	}
}

func TestScheduledTasks(t *testing.T) {
	p, err := For("windows", `C:\Users\a\AppData\Local\backsight\bin\current.exe`, `C:\Users\a`, "", 0)
	if err != nil || p.Kind != "schtasks" || len(p.Files) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	if got := strings.Join(p.Activate[0], "|"); got != `schtasks|/Create|/F|/TN|backsight\tick|/TR|"C:\Users\a\AppData\Local\backsight\bin\current.exe" jobs tick|/SC|MINUTE|/MO|15` {
		t.Errorf("%s", got)
	}
	if _, err := For("plan9", "/x", "/", "/", 0); err == nil {
		t.Error("an unknown system is refused")
	}
}
