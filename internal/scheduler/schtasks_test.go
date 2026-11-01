package scheduler

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden task files")

// winOptions are a Windows install's: a profile path with a space, an &
// and a non-ASCII letter (review focus 4).
func winOptions() Options {
	return Options{Shim: `C:\Users\Jé & Co\AppData\Local\backsight\bin\current.exe`, Home: `C:\Users\Jé & Co`,
		State: `C:\Users\Jé & Co\AppData\Local\backsight\state`, UserSID: testSID}
}

// The jobs are Task Scheduler tasks registered from XML (decision 0080):
// the server task's principal and settings, a 15-minute repetition for
// tick and a daily 04:00 calendar trigger for maintain, conhost
// --headless running the shim. The files are the goldens byte for byte
// (UTF-16LE with its BOM; the goldens are kept in UTF-8 to be read).
func TestSchtasksPlanIsXML(t *testing.T) {
	o := winOptions()
	p, err := For("windows", o)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "schtasks" || len(p.Files) != 2 || len(p.Entries) != 2 {
		t.Fatalf("plan %+v", p)
	}
	for i, j := range []string{"tick", "maintain"} {
		f := p.Files[i]
		if want := filepath.Join(o.State, "tasks", "backsight", j+".xml"); f.Path != want {
			t.Errorf("%s file %q, want %q", j, f.Path, want)
		}
		golden := filepath.Join("testdata", j+".xml")
		doc := decodeTask(t, []byte(f.Content))
		if *updateGolden {
			if err := os.WriteFile(golden, []byte(doc), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if f.Content != string(utf16LE(string(want))) {
			t.Errorf("%s task differs from %s:\n%s", j, golden, doc)
		}
		create := []string{"schtasks", "/Create", "/F", "/XML", f.Path, "/TN", `backsight\` + j}
		if got := strings.Join(p.Activate[i], "|"); got != strings.Join(create, "|") {
			t.Errorf("%s activate %q, want %q", j, got, strings.Join(create, "|"))
		}
		e := p.Entries[i]
		if e.Job != j || strings.Join(e.Files, "|") != f.Path || len(e.Reload) != 1 || strings.Join(e.Reload[0], "|") != strings.Join(create, "|") {
			t.Errorf("%s entry %+v", j, e)
		}
		if got := strings.Join(p.Remove[i], " "); got != `schtasks /Delete /F /TN backsight\`+j {
			t.Errorf("%s remove %q", j, got)
		}
		if got := strings.Join(p.Start[j], " "); got != `schtasks /Run /TN backsight\`+j {
			t.Errorf("%s start %q", j, got)
		}
	}
	// Each trigger is the job's own: tick never runs daily, maintain never
	// every 15 minutes.
	tick, maintain := parseTask(t, []byte(p.Files[0].Content)), parseTask(t, []byte(p.Files[1].Content))
	if len(tick.Triggers.Time) != 1 || tick.Triggers.Time[0].Interval != "PT15M" || len(tick.Triggers.Calendar) != 0 || len(tick.Triggers.Logon) != 0 {
		t.Errorf("tick triggers %+v", tick.Triggers)
	}
	if len(maintain.Triggers.Calendar) != 1 || !strings.HasSuffix(maintain.Triggers.Calendar[0].Start, "T04:00:00") || len(maintain.Triggers.Time) != 0 {
		t.Errorf("maintain triggers %+v", maintain.Triggers)
	}
	// The jobs keep Task Scheduler's default priority: only the server
	// asks for normal. A failed run waits for the next trigger: no restart.
	if tick.Settings.Priority != "" || maintain.Settings.Priority != "" {
		t.Errorf("priority %q %q, want the default", tick.Settings.Priority, maintain.Settings.Priority)
	}
	if tick.Settings.RestartCount != 0 || maintain.Settings.RestartEvery != "" {
		t.Errorf("a job restarts on failure: %+v", tick.Settings)
	}
}

// Whether a job runs is its own lock file held (the lock jobs tick and
// jobs maintain take), never schtasks' output, which is localized.
func TestSchtasksEntriesProbeTheJobsLock(t *testing.T) {
	o := winOptions()
	p, err := For("windows", o)
	if err != nil {
		t.Fatal(err)
	}
	for i, j := range []string{"tick", "maintain"} {
		e := p.Entries[i]
		if want := filepath.Join(o.State, "locks", "job-"+j+".lock"); e.Lock != want {
			t.Errorf("%s lock %q, want %q", j, e.Lock, want)
		}
		if e.Running != nil || e.RunningMark != "" || e.Enabled != nil {
			t.Errorf("%s still asks schtasks: running %q mark %q enabled %q", j, e.Running, e.RunningMark, e.Enabled)
		}
	}
	// systemd and launchd keep their probes and name no lock.
	for _, goos := range []string{"linux", "darwin"} {
		q, _ := For(goos, Options{Shim: "/s", Home: "/h", UnitDir: "/u", UID: 501, LogDir: "/l", State: "/st"})
		for _, e := range q.Entries {
			if e.Lock != "" {
				t.Errorf("%s entry %s names a lock %q", goos, e.Job, e.Lock)
			}
		}
	}
}

// A task cannot carry environment variables: a BACKSIGHT_HOME and a
// BACKSIGHT_STATE_DIR the install resolved reach the job as
// --backsight-home and --backsight-state-dir, quoted as Windows splits
// them; without them the job gets no flag.
func TestJobsTaskCarriesBacksightHome(t *testing.T) {
	o := winOptions()
	o.Env = map[string]string{"BACKSIGHT_HOME": `D:\bs home\Jé & Co`, "BACKSIGHT_STATE_DIR": `E:\st ate`, "PATH": `C:\Windows\System32`}
	p, err := For("windows", o)
	if err != nil {
		t.Fatal(err)
	}
	for i, j := range []string{"tick", "maintain"} {
		got := parseTask(t, []byte(p.Files[i].Content)).Arguments
		want := `--headless "` + o.Shim + `" jobs ` + j + ` --backsight-home "D:\bs home\Jé & Co" --backsight-state-dir "E:\st ate"`
		if got != want {
			t.Errorf("%s arguments\n%s\nwant\n%s", j, got, want)
		}
		if strings.Contains(decodeTask(t, []byte(p.Files[i].Content)), "System32;") || strings.Contains(got, "PATH") {
			t.Errorf("%s carries another variable:\n%s", j, got)
		}
	}
	o.Env = map[string]string{"PATH": `C:\Windows\System32`}
	p, _ = For("windows", o)
	if got := parseTask(t, []byte(p.Files[0].Content)).Arguments; strings.Contains(got, "--backsight-") {
		t.Errorf("no BACKSIGHT_HOME nor BACKSIGHT_STATE_DIR, yet %s", got)
	}
}

// BACKSIGHT_STATE_DIR is a task's to carry only: on Windows TaskEnv adds
// it, elsewhere the units stay as they are (they inherit nothing of it
// today).
func TestTaskEnvAddsTheStateDirOnWindowsOnly(t *testing.T) {
	get := func(k string) string { return map[string]string{"BACKSIGHT_STATE_DIR": `E:\st`}[k] }
	if env := TaskEnv("windows", map[string]string{"BACKSIGHT_HOME": `D:\bs`}, get); env["BACKSIGHT_STATE_DIR"] != `E:\st` || env["BACKSIGHT_HOME"] != `D:\bs` {
		t.Errorf("windows %q", env)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if env := TaskEnv(goos, map[string]string{}, get); len(env) != 0 {
			t.Errorf("%s %q", goos, env)
		}
	}
	if env := TaskEnv("windows", map[string]string{}, func(string) string { return "" }); len(env) != 0 {
		t.Errorf("unset %q", env)
	}
}

// The recorded task files name their tasks: a project renamed since still
// deletes the tasks it installed.
func TestInstalledTasksFromRecordedFiles(t *testing.T) {
	o := winOptions()
	o.Name = "old-name"
	p, _ := For("windows", o)
	in := InstalledFrom(p.Paths())
	if strings.Join(in.Tasks, " ") != `old-name\tick old-name\maintain` {
		t.Errorf("tasks %q", in.Tasks)
	}
	got := in.Remove("schtasks", 0)
	if len(got) != 2 || strings.Join(got[0], " ") != `schtasks /Delete /F /TN old-name\tick` || strings.Join(got[1], " ") != `schtasks /Delete /F /TN old-name\maintain` {
		t.Errorf("remove %q", got)
	}
	if strings.Join(p.Remove[0], " ") != strings.Join(got[0], " ") {
		t.Errorf("the plan's remove %q differs from the recorded files' %q", p.Remove, got)
	}
	// An XML file elsewhere names no task.
	if in := InstalledFrom([]string{filepath.Join("x", "notes", "a.xml"), "b.xml"}); len(in.Tasks) != 0 {
		t.Errorf("foreign files %q", in.Tasks)
	}
}

// What a task cannot run as written is refused before anything is
// registered: a % in the shim or BACKSIGHT_HOME (Task Scheduler expands
// it, E34 W2), or no user SID for the principal.
func TestSchtasksPlanRefusesWhatATaskCannotCarry(t *testing.T) {
	o := winOptions()
	o.Shim = `C:\Users\100%\bin\current.exe`
	if _, err := For("windows", o); err == nil || !strings.Contains(err.Error(), "BACKSIGHT_HOME") {
		t.Errorf("a %% shim: %v", err)
	}
	o = winOptions()
	o.Env = map[string]string{"BACKSIGHT_HOME": `C:\%USERNAME%\bs`}
	if _, err := For("windows", o); err == nil || !strings.Contains(err.Error(), "BACKSIGHT_HOME") {
		t.Errorf("a %% BACKSIGHT_HOME: %v", err)
	}
	o = winOptions()
	o.UserSID = ""
	if _, err := For("windows", o); err == nil || !strings.Contains(err.Error(), "SID") {
		t.Errorf("no SID: %v", err)
	}
}
