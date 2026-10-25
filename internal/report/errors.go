package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/stats"
)

// MaxRecent is how many recent errors the diagnostic lists.
const MaxRecent = 10

// Errors classifies the telemetry's error events: counts by class and the
// newest MaxRecent, newest first. Messages are raw here; Build scrubs them.
// Only fields backsight writes itself are read: never a prompt (prompt
// events carry its length only), a memory or a project name.
func Errors(events []stats.Event) (map[string]int, []ErrorEvent) {
	counts := map[string]int{}
	var recent []ErrorEvent
	for _, e := range events {
		class, msg := classify(e)
		if class == "" {
			continue
		}
		counts[class]++
		recent = append(recent, ErrorEvent{TS: e.TS, Class: class, Message: msg})
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].TS.After(recent[j].TS) })
	if len(recent) > MaxRecent {
		recent = recent[:MaxRecent]
	}
	return counts, recent
}

// classify names an error event's class ("" for an event that is no error).
func classify(e stats.Event) (class, msg string) {
	switch e.Event {
	case "hook_failure":
		return "hook_failure/" + word(e.Hook) + "/" + word(e.Kind), ""
	case "job":
		switch e.Status {
		case "done", "skipped", "started", "ok", "":
			return "", ""
		}
		reason, _ := e.Details["reason"].(string)
		return "job/" + word(e.Name) + "/" + word(e.Status), reason
	case "retain":
		if e.Status == "failed" || e.Status == "quarantined" {
			return "retain/" + e.Status, ""
		}
	case "tool":
		// memory_store records a refusal (a secret, a pause) as not OK too.
		if !e.OK && e.Name != "memory_store" {
			return "tool/" + word(e.Name) + "/failed", ""
		}
	case "prompt":
		var failed []string
		for name, s := range e.Scopes {
			if !s.OK {
				failed = append(failed, word(name))
			}
		}
		if len(failed) > 0 {
			sort.Strings(failed)
			return "recall/scope_failed", fmt.Sprintf("scopes: %s", strings.Join(failed, ", "))
		}
	case "transcript_unknown":
		return "transcript/unknown_records", ""
	default:
		if strings.HasSuffix(e.Event, "_error") || strings.HasSuffix(e.Event, "_failed") {
			return word(e.Event), e.Error
		}
	}
	return "", ""
}

// word keeps a class part to identifier characters, so a class is the same
// token for every user and never carries free text.
func word(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '_' || r == '-' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
