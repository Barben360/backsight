package feedback

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Show is one memory put in front of the agent, read back from a shows
// event. Key names it (session|turn|<event ts in Unix ns>|id): the hooks
// get no tool call id, and seq is counted per process. Project is the
// memory's, from its item; the session's when the item names none (a read,
// an older line).
type Show struct {
	ID, Turn, Agent, Path, Key, Project string
	Rank                                int
	At                                  time.Time
}

// Shows lists every item shown in session, in journal order.
func Shows(events []Event, session string) []Show {
	var out []Show
	for _, e := range events {
		if e.Kind == "shows" && e.Session == session {
			out = append(out, showsOf(e)...)
		}
	}
	return out
}

// showsOf is the shows of one shows event.
func showsOf(e Event) []Show {
	at := strconv.FormatInt(e.TS.UnixNano(), 10)
	out := make([]Show, 0, len(e.Items))
	for _, it := range e.Items {
		project := it.Project
		if project == "" {
			project = e.Project
		}
		out = append(out, Show{ID: it.ID, Turn: e.Turn, Agent: e.Agent, Path: e.Path, Project: project, Rank: it.Rank, At: e.TS,
			Key: e.Session + "|" + e.Turn + "|" + at + "|" + it.ID})
	}
	return out
}

var (
	// ErrUnknown: no shown id is, or starts with, the reference.
	ErrUnknown = errors.New("not shown in this session")
	// ErrAmbiguous: the reference starts several shown ids.
	ErrAmbiguous = errors.New("matches several memories shown in this session")
)

// prefixRE: a short id, the first 8 characters of an id or more.
var prefixRE = regexp.MustCompile(`^[0-9a-f]{8}[0-9a-f-]*$`)

// Resolve is the shown id ref names: the id itself, or a prefix of at
// least 8 lowercase hex characters that exactly one shown id starts with.
func Resolve(ref string, shown []Show) (string, error) {
	for _, s := range shown {
		if s.ID == ref {
			return ref, nil
		}
	}
	if !prefixRE.MatchString(ref) {
		return "", ErrUnknown
	}
	match := ""
	for _, s := range shown {
		if strings.HasPrefix(s.ID, ref) && s.ID != match {
			if match != "" {
				return "", ErrAmbiguous
			}
			match = s.ID
		}
	}
	if match == "" {
		return "", ErrUnknown
	}
	return match, nil
}

// Check is what the end of a turn needs to know about one agent: the
// memories shown to it, whether it declared (a memory_credit call of the
// turn), whether it was already asked to, and what it declared.
type Check struct {
	Shown             []Show
	Declared, Blocked bool
	None              bool     // a credit of the turn said none
	Credited          []string // the ids its used credits named
}

// CheckTurn is turn's check for agent ("" is the main agent) of session;
// withBriefing adds the main agent's briefing shows (turn ""), shown
// before the first prompt.
func CheckTurn(events []Event, session, turn, agent string, withBriefing bool) Check {
	var c Check
	for _, s := range Shows(events, session) {
		if s.Agent == agent && (s.Turn == turn || withBriefing && agent == "" && s.Turn == "") {
			c.Shown = append(c.Shown, s)
		}
	}
	for _, e := range events {
		if e.Session != session || e.Turn != turn || e.Agent != agent {
			continue
		}
		switch {
		case e.Kind == "credit":
			c.Declared = true
			c.None = c.None || e.Verdict == "none"
			for _, id := range e.IDs {
				if !slices.Contains(c.Credited, id) {
					c.Credited = append(c.Credited, id)
				}
			}
		case e.Kind == "stop" && e.Outcome == "blocked":
			c.Blocked = true
		}
	}
	return c
}

// Honesty flags (audit only, never fed to the ranking): a declaration the
// final reply seems to contradict.
const (
	FlagNoneWithOverlap      = "none_with_overlap"      // declared none, but the reply repeats a shown memory
	FlagCreditWithoutOverlap = "credit_without_overlap" // a credited memory the reply does not echo
	noneOverlapMin           = 0.5
	creditOverlapMin         = 0.2
)

// Flags compares reply with texts (memory id to text) of the memories c
// shows and credits; a memory without a text, or without a word to
// compare, flags nothing.
func (c Check) Flags(reply string, texts map[string]string) []string {
	var flags []string
	if c.None && slices.ContainsFunc(c.Shown, func(s Show) bool {
		text := texts[s.ID]
		return len(words(text)) > 0 && Overlap(reply, text) >= noneOverlapMin
	}) {
		flags = append(flags, FlagNoneWithOverlap)
	}
	if slices.ContainsFunc(c.Credited, func(id string) bool {
		text := texts[id]
		return len(words(text)) > 0 && Overlap(reply, text) < creditOverlapMin
	}) {
		flags = append(flags, FlagCreditWithoutOverlap)
	}
	return flags
}

// Overlap is the share of memory's distinct words (lowercase, 4 letters or
// digits or more) that reply contains; 0 when memory has none.
func Overlap(reply, memory string) float64 {
	mem := words(memory)
	if len(mem) == 0 {
		return 0
	}
	in := words(reply)
	n := 0
	for w := range mem {
		if in[w] {
			n++
		}
	}
	return float64(n) / float64(len(mem))
}

// words is the set of s's lowercase words of at least 4 letters or digits.
func words(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) >= 4 {
			set[w] = true
		}
	}
	return set
}
