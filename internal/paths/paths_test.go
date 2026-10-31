package paths

import (
	"path/filepath"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolve(t *testing.T) {
	cases := []struct {
		name string
		goos string
		home string
		env  map[string]string
		want Dirs
	}{
		{"linux defaults", "linux", "/home/u", nil, Dirs{
			Data: "/home/u/.local/share/backsight", Config: "/home/u/.config/backsight",
			State: "/home/u/.local/state/backsight", Bin: "/home/u/.local/bin"}},
		{"linux XDG", "linux", "/home/u", map[string]string{"XDG_DATA_HOME": "/x/data", "XDG_CONFIG_HOME": "/x/cfg", "XDG_STATE_HOME": "/x/state"}, Dirs{
			Data: "/x/data/backsight", Config: "/x/cfg/backsight", State: "/x/state/backsight", Bin: "/home/u/.local/bin"}},
		// The XDG spec says relative values are invalid and must be ignored.
		{"linux relative XDG ignored", "linux", "/home/u", map[string]string{"XDG_CONFIG_HOME": "rel/cfg"}, Dirs{
			Data: "/home/u/.local/share/backsight", Config: "/home/u/.config/backsight",
			State: "/home/u/.local/state/backsight", Bin: "/home/u/.local/bin"}},
		{"darwin", "darwin", "/Users/u", map[string]string{"XDG_CONFIG_HOME": "/x/cfg"}, Dirs{
			Data: "/Users/u/Library/Application Support/backsight", Config: "/Users/u/Library/Application Support/backsight/config",
			State: "/Users/u/Library/Application Support/backsight/state", Bin: "/Users/u/.local/bin"}},
		{"windows", "windows", `C:\Users\u`, map[string]string{"LOCALAPPDATA": `C:\Users\u\AppData\Local`}, Dirs{
			Data: `C:\Users\u\AppData\Local\backsight`, Config: `C:\Users\u\AppData\Local\backsight\config`,
			State: `C:\Users\u\AppData\Local\backsight\state`, Bin: `C:\Users\u\AppData\Local\Programs\backsight`}},
		{"windows BACKSIGHT_HOME", "windows", `C:\Users\u`, map[string]string{"BACKSIGHT_HOME": `D:/bs`, "BACKSIGHT_STATE_DIR": `\\nas\s\st`}, Dirs{
			Data: `D:\bs\data`, Config: `D:\bs\config`, State: `\\nas\s\st`, Bin: `D:\bs\bin`}},
		{"BACKSIGHT_HOME wins everywhere", "darwin", "/Users/u", map[string]string{"BACKSIGHT_HOME": "/t/bs"}, Dirs{
			Data: "/t/bs/data", Config: "/t/bs/config", State: "/t/bs/state", Bin: "/t/bs/bin"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(c.goos, c.home, env(c.env))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestResolveRefusesMissingBase(t *testing.T) {
	if _, err := Resolve("linux", "", env(nil)); err == nil {
		t.Error("an empty home must be refused, not resolved to relative paths")
	}
	if _, err := Resolve("windows", `C:\Users\u`, env(nil)); err == nil {
		t.Error("windows without LOCALAPPDATA must be refused")
	}
	if _, err := Resolve("linux", "/home/u", env(map[string]string{"BACKSIGHT_HOME": "relative"})); err == nil {
		t.Error("a relative BACKSIGHT_HOME must be refused")
	}
	// Absolute is judged by the target OS's rules, whatever the host.
	if _, err := Resolve("linux", `C:\Users\u`, env(nil)); err == nil {
		t.Error(`a home of C:\Users\u is not absolute on linux`)
	}
	if _, err := Resolve("windows", `C:\Users\u`, env(map[string]string{"BACKSIGHT_HOME": "/t/bs"})); err == nil {
		t.Error("a BACKSIGHT_HOME of /t/bs is not absolute on windows")
	}
	if d, err := Resolve("windows", `C:\Users\u`, env(map[string]string{"BACKSIGHT_HOME": `C:\bs`, "BACKSIGHT_STATE_DIR": "/s"})); err != nil || d.State != `C:\bs\state` {
		t.Errorf("a BACKSIGHT_STATE_DIR of /s is not absolute on windows: %+v %v", d, err)
	}
}

func TestFiles(t *testing.T) {
	d := Dirs{Config: "/c", State: "/s"}
	if d.ConfigFile() != filepath.Join("/c", "config.toml") || d.SecretsFile() != filepath.Join("/c", "secrets.toml") {
		t.Errorf("%s %s", d.ConfigFile(), d.SecretsFile())
	}
}

// Appendix A.3: every state location is named here, once.
func TestStateLayout(t *testing.T) {
	d := Dirs{State: "/s"}
	for got, rel := range map[string]string{
		d.StateFile():       "state.json",
		d.LocksDir():        "locks",
		d.MaintenanceLock(): "maintenance.lock",
		d.CursorsDir():      "cursors",
		d.SessionsDir():     "sessions",
		d.SentIndexDir():    "sent-index",
		d.OutboxDir():       "outbox",
		d.QuarantineDir():   "quarantine",
		d.JournalFile():     "journal.jsonl",
		d.PreImagesDir():    "pre-images",
		d.TelemetryDir():    "telemetry",
		d.FeedbackDir():     "feedback",
	} {
		if want := filepath.Join("/s", rel); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestStateDirOverrideMovesOnlyTheState(t *testing.T) {
	env := map[string]string{"BACKSIGHT_HOME": "/b", "BACKSIGHT_STATE_DIR": "/scratch/state"}
	d, err := Resolve("linux", "/home/u", func(k string) string { return env[k] })
	if err != nil || d.State != "/scratch/state" || d.Config != "/b/config" || d.Data != "/b/data" {
		t.Errorf("%+v %v", d, err)
	}
	env["BACKSIGHT_STATE_DIR"] = "relative"
	if d, _ := Resolve("linux", "/home/u", func(k string) string { return env[k] }); d.State != "/b/state" {
		t.Errorf("a relative override must be ignored: %+v", d)
	}
}
