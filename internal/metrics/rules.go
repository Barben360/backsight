package metrics

import (
	"regexp"
	"strings"
	"time"
)

// DoctorCheck is one doctor check as the dashboard reads it. Status is
// ok|info|warn|error; Fix is kept only when it is a single backsight
// command (singleCommand).
type DoctorCheck struct {
	ID     string
	Status string
	Fix    string
}

// commandWord is one word of a single backsight command: a subcommand or
// value (letters, digits, . _ # = + -, never led by a dot) or a flag with
// such a value. No separator, quote, parenthesis, placeholder or path.
var commandWord = regexp.MustCompile(`^(--?[A-Za-z][A-Za-z0-9-]*(=[A-Za-z0-9_.#+-]+)?|[A-Za-z0-9][A-Za-z0-9_.#=+-]*)$`)

// singleCommand reports whether fix is one backsight command and nothing
// else, so that it can be shown as "Run:" and handed to an agent as the
// item's command: "backsight" then words, none of them a second
// "backsight"; no ;, &, |, newline, backtick, $(, prose punctuation or path.
func singleCommand(fix string) bool {
	if fix != strings.TrimSpace(fix) || strings.ContainsAny(fix, "\n\r\t") {
		return false
	}
	words := strings.Split(fix, " ")
	if len(words) < 2 || words[0] != "backsight" {
		return false
	}
	for _, w := range words[1:] {
		if w == "backsight" || !commandWord.MatchString(w) {
			return false
		}
	}
	return true
}

// The error-class rules: one function per metric, on the counts and ages of
// the catalog's status column, applied at once. Ages are strict (an age
// exactly on a limit is not past it); hook counts are "at least".

// ruleConsolidation: any failed consolidation → watch.
func ruleConsolidation(failed int64) Status {
	if failed > 0 {
		return StatusWatch
	}
	return StatusOK
}

// ruleServerQueue: any operation failed in the last 7 days → watch.
func ruleServerQueue(failedLast7d int64) Status {
	if failedLast7d > 0 {
		return StatusWatch
	}
	return StatusOK
}

// rulePipelineFlow: any document failed or set aside → watch; a backlog
// older than 24 h → bad.
func rulePipelineFlow(failed, setAside int64, backlogAge time.Duration) Status {
	switch {
	case backlogAge > day:
		return StatusBad
	case failed > 0 || setAside > 0:
		return StatusWatch
	}
	return StatusOK
}

// ruleLocalBacklog: the oldest waiting item older than 1 h → watch, than
// 24 h → bad.
func ruleLocalBacklog(oldest time.Duration) Status {
	switch {
	case oldest > day:
		return StatusBad
	case oldest > time.Hour:
		return StatusWatch
	}
	return StatusOK
}

// seriousHookFailures are the kinds that watch from the first one.
var seriousHookFailures = map[string]bool{"panic": true, "fatal": true, "exit": true, "spawn": true}

// ruleHookFailures, on the last 24 h counted per kind: panic, fatal, exit or
// spawn ≥ 1, or any kind ≥ 3 → watch; any kind ≥ 10 → bad.
func ruleHookFailures(last24h map[string]int64) Status {
	s := StatusOK
	for kind, n := range last24h {
		switch {
		case n >= 10:
			return StatusBad
		case n >= 3 || (n >= 1 && seriousHookFailures[kind]):
			s = StatusWatch
		}
	}
	return s
}

// ruleServerReach: the server failing or the breaker open → watch; and
// with a backlog older than 24 h → bad.
func ruleServerReach(failing, breakerOpen bool, backlogAge time.Duration) Status {
	switch {
	case !failing && !breakerOpen:
		return StatusOK
	case backlogAge > day:
		return StatusBad
	}
	return StatusWatch
}

// ruleDoctor: any check in error → watch.
func ruleDoctor(checks []DoctorCheck) Status {
	for _, c := range checks {
		if c.Status == "error" {
			return StatusWatch
		}
	}
	return StatusOK
}

// ruleJobs: bad only when the last run is more than twice the cadence ago
// and prompts or tool events happened since the run was expected (a
// sleeping laptop is not a fault).
func ruleJobs(sinceLast, cadence time.Duration, activitySinceExpected bool) Status {
	if sinceLast > 2*cadence && activitySinceExpected {
		return StatusBad
	}
	return StatusOK
}
