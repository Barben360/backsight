package metrics

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// The verdict's and the attention list's sentences: the user reads them
// on the overview banner and in `stats overview`. They name counts,
// rates and commands only, never memory, prompt or error text.

// noDataSentence is the sentence of a no_data reason, for an axis too
// early because its metrics have nothing to read.
func noDataSentence(reason string) string {
	switch reason {
	case ReasonServerOnlyHost:
		return "This PC runs the server only: activity is recorded on the PCs that use it."
	case ReasonFeedbackOff:
		return "Memory feedback is off (`feedback.enabled`)."
	}
	return "Nothing yet: memory starts recording at your next Claude Code prompt."
}

// countUnits name what a judged metric's n counts, in the too-early
// sentence: the singular, then the plural.
var countUnits = map[string][2]string{
	"recall.hit_rate":     {"prompt", "prompts"},
	"used.turn_rate":      {"declared turn", "declared turns"},
	"declare.coverage":    {"turn", "turns"},
	"verdict.wrong_rate":  {"verdict", "verdicts"},
	"context.per_session": {"session", "sessions"},
	"recall.latency":      {"prompt", "prompts"},
}

// count is n with its noun, singular for 1 ("1 step", "2 steps").
func count(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return grouped(n) + " " + many
}

func learningSentence(day int, prompts int64) string {
	return fmt.Sprintf("Learning: day %d of about 7, %s so far.", day, count(prompts, "prompt", "prompts"))
}

func tooEarlySentence(n int64, unit [2]string, minN int) string {
	return fmt.Sprintf("Not enough yet: %s, %s needed.", count(n, unit[0], unit[1]), grouped(int64(minN)))
}

// cantJudgeSentence prints the coverage rounded down: a coverage just
// under the 80% the rule reads never prints as 80.
func cantJudgeSentence(coverage float64) string {
	return fmt.Sprintf("Can't judge yet: Claude said what it used on only %d %% of turns.", int(math.Floor(coverage*100+1e-9)))
}

const judged28Sentence = "Judged on the last 28 days: too few turns in 7."

// rootCauseSentence: the server does not answer and what waits for it.
// since is zero when this PC never recorded an answer nor a failure.
func rootCauseSentence(since, now time.Time, loc *time.Location, notes, sessions int64) string {
	wait := fmt.Sprintf("%s and %s wait on this PC, nothing is lost.", count(notes, "note", "notes"), count(sessions, "session", "sessions"))
	if since.IsZero() {
		return "Memory server unreachable: " + wait
	}
	return fmt.Sprintf("Memory server unreachable since %s: %s", clock(since, now, loc), wait)
}

// clock is t as 15:04 in loc when it is today there, else 2006-01-02 15:04.
func clock(t, now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	t, now = t.In(loc), now.In(loc)
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

func doctorErrorSentence(id string) string { return fmt.Sprintf("Health check %s failed.", id) }

func doctorWarnSentence(id string) string {
	return fmt.Sprintf("Health check %s reports a warning.", id)
}

// itemText is the attention sentence of metric m judged over days days:
// the copy deck's where it has one, else "{label}: {value}." median is the
// baseline median of a relative metric (ok false: none).
func itemText(d Def, m Metric, days int, median float64, hasMedian bool) string {
	v := m.Value
	switch d.ID {
	case "recall.hit_rate":
		if v != nil && hasMedian {
			return fmt.Sprintf("Fewer prompts get memory than usual: %s %% vs %s %% before.", percent(*v), percent(median))
		}
	case "used.turn_rate":
		if v != nil && hasMedian {
			return fmt.Sprintf("Claude uses the memory shown less than usual: %s %% of turns vs %s %%.", percent(*v), percent(median))
		}
	case "verdict.wrong_rate":
		if m.Num != nil {
			if days == 7 {
				return fmt.Sprintf("%s reported wrong or out of date this week.", count(*m.Num, "memory", "memories"))
			}
			return fmt.Sprintf("%s reported wrong or out of date in the last %d days.", count(*m.Num, "memory", "memories"), days)
		}
	case "recall.latency":
		if v != nil {
			return fmt.Sprintf("Memory adds %s s to the slowest prompts (limit 2.5 s).", seconds(*v))
		}
	case "context.per_session":
		if v != nil {
			return fmt.Sprintf("Memory adds more context than its budget: %s per session.", count(int64(math.Round(*v)), "character", "characters"))
		}
	case "hooks.failures":
		if last, ok := m.Extra["last_24h"].(map[string]int64); ok {
			var n int64
			for _, c := range last {
				n += c
			}
			if n > 0 {
				return fmt.Sprintf("%s crashed in the last 24 h.", count(n, "step", "steps"))
			}
		}
	case "pipeline.flow":
		failed, _ := m.Extra["failed"].(int64)
		setAside, _ := m.Extra["set_aside"].(int64)
		if failed+setAside > 0 {
			return fmt.Sprintf("%s could not be sent; %s set aside after an error.", count(failed, "session", "sessions"), grouped(setAside))
		}
	}
	if v == nil {
		return d.Label + "."
	}
	return fmt.Sprintf("%s: %s.", d.Label, formatValue(d.Unit, *v))
}

// formatValue prints a metric's value in its unit.
func formatValue(unit string, v float64) string {
	switch unit {
	case "ratio":
		return percent(v) + " %"
	case "ms":
		return duration(time.Duration(v) * time.Millisecond)
	case "money":
		return fmt.Sprintf("%.2f", v)
	}
	return grouped(int64(math.Round(v)))
}

// duration is d in seconds under a minute, else minutes, hours, days.
func duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return seconds(float64(d.Milliseconds())) + " s"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 2*day:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days", int(d/day))
}

// percent is a ratio as a whole percentage ("64").
func percent(r float64) string { return strconv.Itoa(int(math.Round(r * 100))) }

// seconds is a millisecond figure in seconds, one decimal ("2.7").
func seconds(ms float64) string { return strconv.FormatFloat(ms/1000, 'f', 1, 64) }

// grouped is n with a comma every three digits ("72,000").
func grouped(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := n < 0
	if neg {
		s = s[1:]
	}
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
