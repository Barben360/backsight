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
			out = append(out, ShowsOf(e)...)
		}
	}
	return out
}

// Retirements counts the memories session retired: the ids of its retire
// and hide events, whatever turn they came in (feedback.max_retirements).
func Retirements(events []Event, session string) int {
	n := 0
	for _, e := range events {
		if (e.Kind == KindRetire || e.Kind == KindHide) && e.Session == session {
			n += len(e.IDs)
		}
	}
	return n
}

// ShowsOf is the shows of one shows event.
func ShowsOf(e Event) []Show {
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
// BlockPaths and BlockShown: the paths of the shows before the turn's
// first block, each once, in the order first shown, and how many
// memories they showed; Verdicts: the verdicts it declared, each once.
type Check struct {
	Shown             []Show
	Declared, Blocked bool
	None              bool     // the turn's only declaration was none
	Credited          []string // the ids its used credits named
	BlockPaths        []string
	BlockShown        int
	Verdicts          []string
}

// distinctIDs counts the memories of shown, each once.
func distinctIDs(shown []Show) int {
	seen := map[string]bool{}
	for _, s := range shown {
		seen[s.ID] = true
	}
	return len(seen)
}

// CheckTurn is turn's check for agent ("" is the main agent) of session;
// withBriefing adds the main agent's briefing shows (turn ""), shown
// before the first prompt. A store's similar memory is left out: it can
// be credited, but a store asks for no declaration.
func CheckTurn(events []Event, session, turn, agent string, withBriefing bool) Check {
	var c Check
	var paths []string // of the shows so far
	none, credited := false, false
	for _, e := range events {
		if e.Session != session {
			continue
		}
		if e.Kind == "shows" {
			if e.Path == PathStore || e.Agent != agent || (e.Turn != turn && (!withBriefing || agent != "" || e.Turn != "")) {
				continue
			}
			shows := ShowsOf(e)
			c.Shown = append(c.Shown, shows...)
			if len(shows) > 0 && !slices.Contains(paths, e.Path) {
				paths = append(paths, e.Path)
			}
			continue
		}
		if e.Turn != turn || e.Agent != agent {
			continue
		}
		switch {
		case e.Kind == "credit":
			c.Declared = true
			if !slices.Contains(c.Verdicts, e.Verdict) {
				c.Verdicts = append(c.Verdicts, e.Verdict)
			}
			none = none || e.Verdict == "none"
			credited = credited || len(e.IDs) > 0
			if e.Verdict != "used" {
				continue // a wrong/outdated report need not echo the memory
			}
			for _, id := range e.IDs {
				if !slices.Contains(c.Credited, id) {
					c.Credited = append(c.Credited, id)
				}
			}
		case e.Kind == "stop" && e.Outcome == "blocked":
			if !c.Blocked {
				c.BlockPaths, c.BlockShown = slices.Clone(paths), distinctIDs(c.Shown)
			}
			c.Blocked = true
		}
	}
	c.None = none && !credited // a credit beside a none stands: the turn is not a none
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
// shows and credits; a reply or a memory without a word to compare (a
// blank reply, an "OK.") or a memory without a text flags nothing.
func (c Check) Flags(reply string, texts map[string]string) []string {
	if len(words(reply)) == 0 {
		return nil
	}
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
