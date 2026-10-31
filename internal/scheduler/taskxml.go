package scheduler

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Task Scheduler tasks as backsight registers them on Windows (decision
// 0080, E34 W1/W2), through `schtasks /Create /XML`: the server's
// supervisor (\<project>\server) and the jobs (\<project>\tick,
// \<project>\maintain).
//
// Every task is the user's own, run with their interactive token at least
// privilege (a standard user can register it; its principal is the
// user's SID, since a name does not resolve over SSH), has no time limit,
// runs on battery, runs a missed start when it can, never runs twice at
// once, and runs its program under conhost --headless: a console the
// program and its children share (CTRL_BREAK reaches them), with no
// window.

// Conhost is every task's program, as the task names it: Task Scheduler
// expands %SystemRoot% when it starts it.
const Conhost = `%SystemRoot%\System32\conhost.exe`

// Trigger kinds.
const (
	TriggerLogon = "logon" // at the user's logon
	TriggerTime  = "time"  // at Start, then every Every (0: once)
	TriggerDaily = "daily" // every day at Start's time of day
)

// Trigger starts a task.
type Trigger struct {
	Kind string
	// Start is the first start, local time "2006-01-02T15:04:05"
	// (TriggerTime, TriggerDaily).
	Start string
	// Every repeats a TriggerTime trigger forever, at least a minute apart.
	Every time.Duration
}

// TaskSpec is one task.
type TaskSpec struct {
	Description string
	UserSID     string // the principal, "S-1-5-21-…"
	Triggers    []Trigger
	Program     string   // what conhost --headless runs
	Args        []string // its arguments
}

// HeadlessArgs is conhost's argv for program and its args: what a task's
// action runs, and what a caller starting the same program without the
// task passes to conhost.exe.
func HeadlessArgs(program string, args ...string) []string {
	return append([]string{"--headless", program}, args...)
}

// TaskXML is spec as the XML schtasks /Create /XML reads: UTF-16LE with
// its byte order mark. The arguments are quoted as Windows programs split
// them (CommandLineToArgv) and XML-escaped. A % anywhere in them is
// refused: Task Scheduler expands %VAR% in an action's arguments, so the
// program would get another path (E34 W2).
func TaskXML(spec TaskSpec) ([]byte, error) {
	if !sidPattern(spec.UserSID) {
		return nil, fmt.Errorf("task principal %q is not a user SID", spec.UserSID)
	}
	if spec.Program == "" {
		return nil, errors.New("a task needs a program")
	}
	if len(spec.Triggers) == 0 {
		return nil, errors.New("a task needs a trigger")
	}
	argv := HeadlessArgs(spec.Program, spec.Args...)
	for _, s := range append(argv, spec.Description) {
		if strings.Contains(s, "%") {
			return nil, fmt.Errorf("%q holds a %%, which Task Scheduler would expand: use a path without one (BACKSIGHT_HOME)", s)
		}
		if i := strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
			return nil, fmt.Errorf("%q holds a control character, which a task cannot carry", s)
		}
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = EscapeArg(a)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>` + "\n")
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\n")
	b.WriteString("  <RegistrationInfo>\n    <Description>" + xmlEscape(spec.Description) + "</Description>\n  </RegistrationInfo>\n")
	b.WriteString("  <Triggers>\n")
	for _, t := range spec.Triggers {
		x, err := triggerXML(t, spec.UserSID)
		if err != nil {
			return nil, err
		}
		b.WriteString(x)
	}
	b.WriteString("  </Triggers>\n")
	b.WriteString("  <Principals>\n    <Principal id=\"Author\">\n      <UserId>" + spec.UserSID + "</UserId>\n" +
		"      <LogonType>InteractiveToken</LogonType>\n      <RunLevel>LeastPrivilege</RunLevel>\n    </Principal>\n  </Principals>\n")
	b.WriteString(taskSettings)
	b.WriteString("  <Actions Context=\"Author\">\n    <Exec>\n      <Command>" + Conhost + "</Command>\n" +
		"      <Arguments>" + xmlEscape(strings.Join(quoted, " ")) + "</Arguments>\n    </Exec>\n  </Actions>\n</Task>\n")
	return utf16LE(b.String()), nil
}

// taskSettings are E34 W1's, which a standard user's task ran with.
const taskSettings = `  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <StartWhenAvailable>true</StartWhenAvailable>
    <AllowHardTerminate>true</AllowHardTerminate>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
  </Settings>
`

func triggerXML(t Trigger, sid string) (string, error) {
	switch t.Kind {
	case TriggerLogon:
		return "    <LogonTrigger>\n      <Enabled>true</Enabled>\n      <UserId>" + sid + "</UserId>\n    </LogonTrigger>\n", nil
	case TriggerTime, TriggerDaily:
		if _, err := time.Parse("2006-01-02T15:04:05", t.Start); err != nil {
			return "", fmt.Errorf("trigger start %q is not a local time 2006-01-02T15:04:05", t.Start)
		}
	default:
		return "", fmt.Errorf("unknown trigger kind %q", t.Kind)
	}
	if t.Kind == TriggerDaily {
		return "    <CalendarTrigger>\n      <Enabled>true</Enabled>\n      <StartBoundary>" + t.Start + "</StartBoundary>\n" +
			"      <ScheduleByDay>\n        <DaysInterval>1</DaysInterval>\n      </ScheduleByDay>\n    </CalendarTrigger>\n", nil
	}
	x := "    <TimeTrigger>\n      <Enabled>true</Enabled>\n      <StartBoundary>" + t.Start + "</StartBoundary>\n"
	if t.Every != 0 {
		if t.Every < time.Minute || t.Every%time.Second != 0 {
			return "", fmt.Errorf("trigger repetition %s: at least a minute, in whole seconds", t.Every)
		}
		x += "      <Repetition>\n        <Interval>" + isoDuration(t.Every) + "</Interval>\n" +
			"        <StopAtDurationEnd>false</StopAtDurationEnd>\n      </Repetition>\n"
	}
	return x + "    </TimeTrigger>\n", nil
}

// isoDuration is d as an XML duration: PT5M, PT1H, PT90S.
func isoDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return "PT" + strconv.Itoa(int(d/time.Hour)) + "H"
	case d%time.Minute == 0:
		return "PT" + strconv.Itoa(int(d/time.Minute)) + "M"
	}
	return "PT" + strconv.Itoa(int(d/time.Second)) + "S"
}

// sidPattern: s looks like a SID's string form, S-1-<digits>(-<digits>)*,
// which needs no XML escaping.
func sidPattern(s string) bool {
	rest, ok := strings.CutPrefix(s, "S-1-")
	if !ok || rest == "" {
		return false
	}
	for _, part := range strings.Split(rest, "-") {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return false
		}
	}
	return true
}

// utf16LE is s in UTF-16 little-endian, after its byte order mark.
func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// EscapeArg is syscall.EscapeArg on any OS (the tasks are built and
// tested on Linux too): s as one argument of a Windows command line, as
// CommandLineToArgv splits it back.
func EscapeArg(s string) string {
	if s == "" {
		return `""`
	}
	needsBackslash, hasSpace := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			needsBackslash = true
		case ' ', '\t':
			hasSpace = true
		}
	}
	if !needsBackslash && !hasSpace {
		return s
	}
	if !needsBackslash {
		return `"` + s + `"`
	}
	var b []byte
	if hasSpace {
		b = append(b, '"')
	}
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		default:
			slashes = 0
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b = append(b, '\\')
			}
			b = append(b, '\\')
		}
		b = append(b, c)
	}
	if hasSpace {
		for ; slashes > 0; slashes-- {
			b = append(b, '\\')
		}
		b = append(b, '"')
	}
	return string(b)
}
