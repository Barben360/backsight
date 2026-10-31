package views

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Barben360/backsight/internal/metrics"
)

// The words of the views: what the text of `stats <view>` and the dashboard
// print for a status, a number or a code of the Result, which holds no free
// text.

// Questions head each view.
var Questions = map[string]string{
	"overview": "Is my memory working?",
	"quality":  "Is it right?",
	"cost":     "Is it worth it?",
	"bank":     "Is the memory healthy?",
	"pipeline": "Is it flowing?",
	"system":   "Is the service up?",
	"releases": "Did a release or a setting help?",
	"projects": "Which projects use memory?",
	"sessions": "Which sessions used memory?",
	"memories": "Which memories were shown?",
	"days":     "How did each day go?",
	"defs":     "What does each number mean?",
}

// FamilyHeadings title the metrics of a family; the first two are the
// verdict's own axes.
var FamilyHeadings = []struct{ Family, Heading string }{
	{"reach", "Found"}, {"quality", "Right"}, {"cost", "Cost"}, {"bank", "Memory"}, {"pipeline", "Flowing"}, {"review", "To review"},
}

// AxisNames are the verdict's axes, in order.
var AxisNames = []struct{ Key, Name string }{{"found", "Found"}, {"right", "Right"}, {"cost", "Cost"}, {"flowing", "Flowing"}}

// AxisStateWords name the state of a verdict axis.
var AxisStateWords = map[metrics.AxisState]string{
	metrics.AxisOK: "ok", metrics.AxisAttention: "attention", metrics.AxisTooEarly: "too early",
	metrics.AxisCantJudge: "can't judge", metrics.AxisLearning: "learning",
}

// NoDataWords say why a metric has no number.
var NoDataWords = map[string]string{
	metrics.ReasonNoEvents:          "nothing recorded yet",
	metrics.ReasonFeedbackOff:       "memory feedback is off",
	metrics.ReasonServerOnlyHost:    "this PC runs the server only",
	metrics.ReasonServerDown:        "memory server not answering",
	metrics.ReasonNotConfigured:     "no memory server set up",
	metrics.ReasonServerUnsupported: "this memory server does not provide it",
	"not_built":                     "not built yet",
}

// ServerErrorWords say why the server did not answer.
var ServerErrorWords = map[string]string{
	"not_configured": "none set up", "timeout": "too slow", "unreachable": "unreachable",
	"unauthorized": "refused the key", "not_found": "does not provide it", "bad_response": "unexpected answer",
	"server_error": "server error",
}

// Heading is the family a metric is listed under: the review counts form
// their own group.
func Heading(d metrics.Def) string {
	if strings.HasPrefix(d.ID, "stale.") {
		return "review"
	}
	return d.Family
}

// Label is the metric's label; the two that share one say which they are.
func Label(d metrics.Def) string {
	switch d.ID {
	case "recall.empty_reasons":
		return "Prompts that found nothing"
	case "extraction.tokens":
		return d.Label + " (tokens)"
	case "extraction.cost":
		return d.Label + " (cost)"
	}
	return d.Label
}

// StatusText is the status word. A metric that is borderline says so
// beside its status ("watch — close to the line"), never instead of it.
func StatusText(d metrics.Def, m metrics.Metric) string {
	switch m.Status {
	case metrics.StatusNoData:
		if m.Reason == metrics.ReasonNotConfigured { // nothing is wrong: there is no server to judge
			return NoDataWords[m.Reason]
		}
		if w := NoDataWords[m.Reason]; w != "" {
			return "no data: " + w
		}
		return "no data"
	case metrics.StatusTooFew:
		s := "too few"
		if m.Den != nil && d.MinN > 0 {
			if need := int64(d.MinN) - *m.Den; need > 0 {
				s += ", " + Grouped(need) + " more needed"
			}
		}
		return s
	}
	s := string(m.Status)
	if s == "" {
		s = "ok"
	}
	if m.Borderline {
		s += " — close to the line"
	}
	return s
}

// DeltaText is the change against the previous window, or why there is none.
func DeltaText(d metrics.Def, m metrics.Metric) string {
	if slices.Contains(m.Tags, "snapshot") {
		return "no history"
	}
	if m.Delta == nil || m.Status == metrics.StatusNoData || m.Status == metrics.StatusTooFew {
		return ""
	}
	vs := "vs previous"
	if slices.Contains(m.Tags, "judged_28d") {
		vs = "vs previous 28 days"
	}
	if *m.Delta == 0 {
		return "no change " + vs
	}
	if d.Unit == "ratio" {
		pts := *m.Delta * 100
		if math.Abs(pts) < 1 {
			return fmt.Sprintf("%+.1f pts %s", pts, vs)
		}
		return fmt.Sprintf("%+d pts %s", int(math.Round(pts)), vs)
	}
	sign := "+"
	if *m.Delta < 0 {
		sign = "-"
	}
	return sign + ValueIn(d.Unit, math.Abs(*m.Delta), m) + " " + vs
}

// FormatValue prints a metric's value in its unit (§9): a ratio as a whole
// percent (one decimal under 1 %) with its interval, chars and tokens with
// "k" from 10,000, ms under a second and seconds above, money with two
// decimals and its currency, counts Grouped.
func FormatValue(d metrics.Def, m metrics.Metric) string {
	if m.Value == nil {
		if d.Unit == "money" {
			return "price not set"
		}
		return "—"
	}
	s := ValueIn(d.Unit, *m.Value, m)
	if d.Unit == "ratio" && m.CI != nil {
		s = fmt.Sprintf("%s (%s–%s %%)", s, Percent(m.CI[0]), Percent(m.CI[1]))
	}
	return s
}

// ValueIn is v in unit; the currency of a money value is in m's extras.
func ValueIn(unit string, v float64, m metrics.Metric) string {
	switch unit {
	case "ratio":
		return Percent(v) + " %"
	case "chars":
		return Thousands(v) + " chars"
	case "tokens":
		return Thousands(v) + " tokens"
	case "ms":
		if math.Abs(v) < 1000 {
			return Grouped(int64(math.Round(v))) + " ms"
		}
		return strconv.FormatFloat(v/1000, 'f', 1, 64) + " s"
	case "money":
		cur, _ := m.Extra["currency"].(string)
		return strings.TrimSpace(fmt.Sprintf("%.2f %s", v, cur))
	}
	return Grouped(int64(math.Round(v)))
}

// Percent is a ratio as a whole percent, one decimal under 1 %.
func Percent(r float64) string {
	if r > 0 && r < 0.01 {
		return strconv.FormatFloat(r*100, 'f', 1, 64)
	}
	return strconv.Itoa(int(math.Round(r * 100)))
}

// Thousands is a count with "k" from 10,000 ("12.4 k"), else Grouped.
func Thousands(v float64) string {
	if math.Abs(v) >= 10000 {
		return strconv.FormatFloat(v/1000, 'f', 1, 64) + " k"
	}
	return Grouped(int64(math.Round(v)))
}

// Grouped is n with a comma every three digits.
func Grouped(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// Plain drops the control characters of s (an escape sequence in a
// folder name or a memory would drive the terminal), keeping keep's.
func Plain(s, keep string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !strings.ContainsRune(keep, r) {
			return -1
		}
		return r
	}, s)
}

// Clip cuts a project name to 24 characters with an ellipsis, without
// its control characters; ShortID keeps 8.
func Clip(s string) string {
	const n = 24
	r := []rune(Plain(s, ""))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

// ShortID keeps 8 characters of an id, without control characters.
func ShortID(id string) string {
	id = Plain(id, "")
	r := []rune(id)
	if len(r) > 8 {
		return string(r[:8])
	}
	return id
}

// Age is a duration in milliseconds as "12 min", "3 h", "2 days".
func Age(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return "under 1 min"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}

// EmptyWords and KindWords name reasons and kinds in plain words.
var EmptyWords = strings.NewReplacer("no_candidates", "no candidates", "below_floor", "below the cut-off", "all_seen", "all already seen", "capped", "cut short by the size limit", "unknown", "reason not recorded")

var KindWords = map[string]string{"world": "facts about you and your work", "experience": "facts from sessions", "observation": "summaries"}

// SentWords say how a note went to the server.
var SentWords = map[string]string{
	"ok": "sent", "queued": "waiting", "retry": "retrying", "failed": "failed",
	"quarantined": "set aside after an error", "unknown": "unknown", "": "—",
}

// OutcomeWords say what became of a memory that was shown.
var OutcomeWords = map[string]string{
	"credit": "used by Claude", "confident_miss": "shown, then ignored", "unknown": "not declared",
	"shown": "shown only", "pending": "pending", "undeclared": "not declared",
}

// CheckWords say whether Claude declared what it used.
var CheckWords = map[string]string{"declared": "said what it used", "undeclared": "didn't say", "skipped": "check skipped"}

// VerdictWords name what a reader said of a memory.
var VerdictWords = map[string]string{"wrong": "reported wrong", "outdated": "out of date"}

// UnitWords name the unit of a metric in a heading.
var UnitWords = map[string]string{"ratio": "rate", "ms": "time"}

// PathWords name the recall paths.
var PathWords = map[string]string{
	"prompt": "added to the prompt", "gate": "before a question", "briefing": "at session start", "search": "Claude's search",
	"read": "opened by Claude", "failure": "after an error", "sources": "sources of a summary",
}

// MemoryStateWords name the state of a memory.
var MemoryStateWords = map[string]string{"hidden": "hidden", "wrong": "reported wrong", "never_used": "often shown, never used", "": "—"}

// Word is the word of k in m, k itself when it has none.
func Word(m map[string]string, k string) string {
	if w, ok := m[k]; ok {
		return w
	}
	return k
}
