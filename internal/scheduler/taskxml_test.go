package scheduler

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

const testSID = "S-1-5-21-1111111111-2222222222-3333333333-1001"

// decodeTask reads TaskXML's bytes back: the BOM, then UTF-16LE.
func decodeTask(t *testing.T, raw []byte) string {
	t.Helper()
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xFE {
		t.Fatalf("no UTF-16LE byte order mark: % x", raw[:min(len(raw), 4)])
	}
	if len(raw)%2 != 0 {
		t.Fatalf("odd length %d: not UTF-16", len(raw))
	}
	units := make([]uint16, 0, len(raw)/2-1)
	for i := 2; i < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

// parsedTask is the part of a task the tests read back.
type parsedTask struct {
	Triggers struct {
		Logon []struct {
			UserID string `xml:"UserId"`
		} `xml:"LogonTrigger"`
		Time []struct {
			Start    string `xml:"StartBoundary"`
			Interval string `xml:"Repetition>Interval"`
		} `xml:"TimeTrigger"`
		Calendar []struct {
			Start string `xml:"StartBoundary"`
			Days  int    `xml:"ScheduleByDay>DaysInterval"`
		} `xml:"CalendarTrigger"`
	} `xml:"Triggers"`
	Principal struct {
		UserID    string `xml:"UserId"`
		LogonType string `xml:"LogonType"`
		RunLevel  string `xml:"RunLevel"`
	} `xml:"Principals>Principal"`
	Settings struct {
		Multiple       string `xml:"MultipleInstancesPolicy"`
		NoBattery      string `xml:"DisallowStartIfOnBatteries"`
		StopBattery    string `xml:"StopIfGoingOnBatteries"`
		Limit          string `xml:"ExecutionTimeLimit"`
		WhenAvailable  string `xml:"StartWhenAvailable"`
		Enabled        string `xml:"Enabled"`
		Hidden         string `xml:"Hidden"`
		StopOnIdleEnd  string `xml:"IdleSettings>StopOnIdleEnd"`
		RestartOnIdle  string `xml:"IdleSettings>RestartOnIdle"`
		RestartCount   int    `xml:"RestartOnFailure>Count"`
		RestartEvery   string `xml:"RestartOnFailure>Interval"`
		AllowTerminate string `xml:"AllowHardTerminate"`
		Priority       string `xml:"Priority"`
	} `xml:"Settings"`
	Command   string `xml:"Actions>Exec>Command"`
	Arguments string `xml:"Actions>Exec>Arguments"`
}

func parseTask(t *testing.T, raw []byte) parsedTask {
	t.Helper()
	doc := decodeTask(t, raw)
	if !strings.HasPrefix(doc, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Fatalf("no UTF-16 XML declaration:\n%s", doc)
	}
	var p parsedTask
	// encoding/xml refuses a UTF-16 declaration on a decoded string.
	body := strings.TrimPrefix(doc, `<?xml version="1.0" encoding="UTF-16"?>`)
	if err := xml.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("not XML: %v\n%s", err, doc)
	}
	return p
}

func serverSpec(program string, args ...string) TaskSpec {
	return TaskSpec{Description: "backsight memory server (bs)", UserSID: testSID, Program: program, Args: args,
		Triggers: []Trigger{{Kind: TriggerLogon}, {Kind: TriggerTime, Start: "2026-01-01T00:00:00", Every: 5 * time.Minute}}}
}

// The task is E34 W1's: the user's SID with their interactive token at
// least privilege, a logon trigger and a 5-minute repetition, no time
// limit, battery allowed, a missed start run, never two at once, and
// conhost --headless running the program.
func TestTaskXMLCarriesW1sSettings(t *testing.T) {
	raw, err := TaskXML(serverSpec(`C:\Users\u\AppData\Local\backsight\bin\current.exe`, "server", "supervise"))
	if err != nil {
		t.Fatal(err)
	}
	p := parseTask(t, raw)
	if p.Principal.UserID != testSID || p.Principal.LogonType != "InteractiveToken" || p.Principal.RunLevel != "LeastPrivilege" {
		t.Errorf("principal %+v", p.Principal)
	}
	if len(p.Triggers.Logon) != 1 || p.Triggers.Logon[0].UserID != testSID {
		t.Errorf("logon trigger %+v, want one for the user's SID", p.Triggers.Logon)
	}
	if len(p.Triggers.Time) != 1 || p.Triggers.Time[0].Interval != "PT5M" || p.Triggers.Time[0].Start != "2026-01-01T00:00:00" {
		t.Errorf("time trigger %+v, want one repeating PT5M", p.Triggers.Time)
	}
	s := p.Settings
	want := map[string][2]string{
		"MultipleInstancesPolicy": {s.Multiple, "IgnoreNew"}, "DisallowStartIfOnBatteries": {s.NoBattery, "false"},
		"StopIfGoingOnBatteries": {s.StopBattery, "false"}, "ExecutionTimeLimit": {s.Limit, "PT0S"},
		"StartWhenAvailable": {s.WhenAvailable, "true"}, "Enabled": {s.Enabled, "true"}, "Hidden": {s.Hidden, "false"},
		"StopOnIdleEnd": {s.StopOnIdleEnd, "false"}, "RestartOnIdle": {s.RestartOnIdle, "false"},
		"RestartOnFailure/Interval": {s.RestartEvery, "PT1M"}, "AllowHardTerminate": {s.AllowTerminate, "true"},
	}
	for k, v := range want {
		if v[0] != v[1] {
			t.Errorf("%s = %q, want %q", k, v[0], v[1])
		}
	}
	if s.RestartCount != 3 {
		t.Errorf("RestartOnFailure/Count = %d", s.RestartCount)
	}
	if p.Command != `%SystemRoot%\System32\conhost.exe` {
		t.Errorf("command %q", p.Command)
	}
	if p.Arguments != `--headless C:\Users\u\AppData\Local\backsight\bin\current.exe server supervise` {
		t.Errorf("arguments %q", p.Arguments)
	}
}

// A path with a space is quoted, &, < and > are escaped for XML and come
// back as written, and non-ASCII survives UTF-16.
func TestTaskXMLQuotesAndEscapes(t *testing.T) {
	shim := `C:\Users\Jé & Co <x>\AppData\Local\backsight\bin\current.exe`
	raw, err := TaskXML(serverSpec(shim, "server", "supervise", "--backsight-home", `C:\a "b"\`))
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeTask(t, raw)
	if !strings.Contains(doc, `Jé &amp; Co &lt;x&gt;`) {
		t.Errorf("the path is not XML-escaped:\n%s", doc)
	}
	p := parseTask(t, raw)
	want := `--headless "` + shim + `" server supervise --backsight-home "C:\a \"b\"\\"`
	if p.Arguments != want {
		t.Errorf("arguments\n%s\nwant\n%s", p.Arguments, want)
	}
}

// Task Scheduler expands %VAR% in the arguments (E34 W2): a % path is
// refused, naming BACKSIGHT_HOME, never registered as another path.
func TestTaskXMLRefusesAPercent(t *testing.T) {
	for _, spec := range []TaskSpec{
		serverSpec(`C:\Users\a%USERNAME%b\current.exe`, "server", "supervise"),
		serverSpec(`C:\x\current.exe`, "server", "supervise", "--backsight-home", `C:\100%`),
	} {
		raw, err := TaskXML(spec)
		if err == nil || !strings.Contains(err.Error(), "BACKSIGHT_HOME") {
			t.Errorf("%v: %v (%d bytes), want a refusal naming BACKSIGHT_HOME", spec.Args, err, len(raw))
		}
	}
}

func TestTaskXMLRefusesWhatATaskCannotCarry(t *testing.T) {
	ok := serverSpec(`C:\x\current.exe`, "server", "supervise")
	cases := map[string]func(s *TaskSpec){
		"a name as principal": func(s *TaskSpec) { s.UserSID = `WORKGROUP\bstest` },
		"an empty principal":  func(s *TaskSpec) { s.UserSID = "" },
		"a SID with XML":      func(s *TaskSpec) { s.UserSID = "S-1-5-<x>" },
		"no program":          func(s *TaskSpec) { s.Program = "" },
		"no trigger":          func(s *TaskSpec) { s.Triggers = nil },
		"an unknown trigger":  func(s *TaskSpec) { s.Triggers = []Trigger{{Kind: "boot"}} },
		"a bad start":         func(s *TaskSpec) { s.Triggers = []Trigger{{Kind: TriggerTime, Start: "tomorrow"}} },
		"a repetition too short": func(s *TaskSpec) {
			s.Triggers = []Trigger{{Kind: TriggerTime, Start: "2026-01-01T00:00:00", Every: 30 * time.Second}}
		},
		"a line break":        func(s *TaskSpec) { s.Args = []string{"server\nsupervise"} },
		"a priority above 10": func(s *TaskSpec) { s.Priority = 11 },
		"a negative priority": func(s *TaskSpec) { s.Priority = -1 },
	}
	if _, err := TaskXML(ok); err != nil {
		t.Fatalf("the valid spec: %v", err)
	}
	for name, change := range cases {
		s := ok
		s.Args = append([]string{}, ok.Args...)
		change(&s)
		if raw, err := TaskXML(s); err == nil {
			t.Errorf("%s: accepted (%d bytes)", name, len(raw))
		}
	}
}

// Priority is set when asked (the server: 5, normal), else left to Task
// Scheduler's default (7, below normal: the jobs).
func TestTaskXMLPriority(t *testing.T) {
	spec := serverSpec(`C:\x\current.exe`, "server", "supervise")
	raw, err := TaskXML(spec)
	if err != nil {
		t.Fatal(err)
	}
	if p := parseTask(t, raw).Settings.Priority; p != "" {
		t.Errorf("default priority written as %q, want none", p)
	}
	spec.Priority = 5
	if raw, err = TaskXML(spec); err != nil {
		t.Fatal(err)
	}
	if p := parseTask(t, raw).Settings.Priority; p != "5" {
		t.Errorf("priority %q, want 5", p)
	}
}

// The jobs' triggers (Task 12): every 15 minutes, and daily at 04:00.
func TestTaskXMLJobTriggers(t *testing.T) {
	spec := TaskSpec{Description: "backsight tick", UserSID: testSID, Program: `C:\x\current.exe`, Args: []string{"jobs", "tick"},
		Triggers: []Trigger{{Kind: TriggerTime, Start: "2026-01-01T00:00:00", Every: 15 * time.Minute}, {Kind: TriggerDaily, Start: "2026-01-01T04:00:00"},
			{Kind: TriggerTime, Start: "2026-01-01T00:00:00", Every: 2 * time.Hour}, {Kind: TriggerTime, Start: "2026-01-02T00:00:00"}}}
	raw, err := TaskXML(spec)
	if err != nil {
		t.Fatal(err)
	}
	p := parseTask(t, raw)
	if len(p.Triggers.Logon) != 0 {
		t.Errorf("a logon trigger nobody asked for: %+v", p.Triggers.Logon)
	}
	if len(p.Triggers.Time) != 3 || p.Triggers.Time[0].Interval != "PT15M" || p.Triggers.Time[1].Interval != "PT2H" ||
		p.Triggers.Time[2].Interval != "" || p.Triggers.Time[2].Start != "2026-01-02T00:00:00" {
		t.Errorf("time triggers %+v", p.Triggers.Time)
	}
	if len(p.Triggers.Calendar) != 1 || p.Triggers.Calendar[0].Days != 1 || p.Triggers.Calendar[0].Start != "2026-01-01T04:00:00" {
		t.Errorf("daily trigger %+v", p.Triggers.Calendar)
	}
}

// EscapeArg is syscall.EscapeArg's algorithm (compared with it on
// Windows: taskxml_windows_test.go).
func TestEscapeArg(t *testing.T) {
	for in, want := range map[string]string{
		"":                  `""`,
		`C:\x\current.exe`:  `C:\x\current.exe`,
		`C:\Jé & Co\a.exe`:  `"C:\Jé & Co\a.exe"`,
		"a\tb":              "\"a\tb\"",
		`say "hi"`:          `"say \"hi\""`,
		`C:\dir with sp\`:   `"C:\dir with sp\\"`,
		`back\\"quote`:      `back\\\\\"quote`,
		`plain`:             `plain`,
		`trailing\`:         `trailing\`,
		`two words\\`:       `"two words\\\\"`,
		`q"`:                `q\"`,
		`C:\Program Files`:  `"C:\Program Files"`,
		`with"space and\\"`: `"with\"space and\\\\\""`,
	} {
		if got := EscapeArg(in); got != want {
			t.Errorf("EscapeArg(%q) = %s, want %s", in, got, want)
		}
	}
}
