package index

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// No free text reaches the index. Three guards stand between a line's
// strings and a column:
//   - closed: a reason, status or kind that could carry text keeps one of
//     the values the code emits, else "other";
//   - shape: an enum-like value the code may extend (a hook, a job, a
//     language) keeps a short lower-case token, else "other", so a value
//     emitted tomorrow is not lost;
//   - name: an id, session, project, document, version or tool name keeps
//     valid UTF-8 of at most nameMax bytes without a control character,
//     else NULL.
// An empty string is no value: NULL.

// other replaces a value outside its closed set or shape.
const other = "other"

// nameMax bounds a name in bytes.
const nameMax = 256

var shapeRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)

// configKeyRE is a config value's key, dotted ("recall.min_score").
var configKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}(\.[a-z][a-z0-9_]{0,47}){0,3}$`)

// The closed sets: every literal the emitters write today (internal/cli,
// internal/hook).
var (
	promptDecisions = []string{"recall", "skip"}
	promptReasons   = []string{"machine", "too_short", "same_topic", "gap", "budget", "topic_shift", "breaker", "maintenance", "first"}
	promptEmpty     = []string{"no_candidates", "below_floor", "all_seen", "capped"}
	detectors       = []string{"rule", "approval"}
	gateDecisions   = []string{"deny", "allow"}
	gateReasons     = []string{"searched", "already_gated", "no_search"}
	retainStatuses  = []string{"ok", "queued", "quarantined", "failed", "retry"}
	failureKinds    = []string{"panic", "fatal", "timeout", "killed", "exit", "invalid_output", "input_too_large", "spawn"}
	jobStatuses     = []string{"done", "skipped", "partial", "failed", "started"}
	feedbackReasons = []string{} // block 2 brings the enum; until then every value is other
	storeResults    = []string{"saved", "updated", "refused"}
	dashActions     = []string{"start", "stop"}
	verdicts        = []string{"used", "none", "wrong", "outdated", "invalid"}
	checkOutcomes   = []string{"declared", "blocked", "undeclared", "check_skipped"}
	retireActions   = []string{"retired", "partly", "hidden", "sources"}
	replyKinds      = []string{"exact", "free", "empty"}
	creditSources   = []string{"tool", "citation"}
	briefingSources = []string{"observations", "raw", "empty"}
	liveKinds       = []string{"record", "stale", "none"}
	// attribution.Source* and retain.SourceExcluded: evidence+mention
	// does not have the shape, so the set is closed.
	attributionSources = []string{"evidence", "mention", "evidence+mention", "continuity", "cwd", "none", "excluded"}
)

// payloadAllow lists, per kind without a table of its own, the fields its
// events row keeps; every other field is dropped (default deny). Never an
// error, a reason or anything that could hold text.
var payloadAllow = map[string][]string{"failure_recall": {"decision", "ms"}}

// closedValue is s when set holds it, other when not, nil when empty.
func closedValue(s string, set []string) any {
	switch {
	case s == "":
		return nil
	case slices.Contains(set, s):
		return s
	}
	return other
}

func shapeOK(s string) bool { return shapeRE.MatchString(s) }

// shapeValue is s when it has the enum shape, other when not, nil when
// empty.
func shapeValue(s string) any {
	switch {
	case s == "":
		return nil
	case shapeOK(s):
		return s
	}
	return other
}

func nameOK(s string) bool {
	if s == "" || len(s) > nameMax || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

// nameValue is s when it is a valid name, nil otherwise.
func nameValue(s string) any {
	if !nameOK(s) {
		return nil
	}
	return s
}

// flagsValue is the flags, each a shape or other, sorted and comma-joined;
// nil when there are none.
func flagsValue(flags []string) any {
	if len(flags) == 0 {
		return nil
	}
	out := make([]string, len(flags))
	for i, f := range flags {
		if shapeOK(f) {
			out[i] = f
		} else {
			out[i] = other
		}
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}
