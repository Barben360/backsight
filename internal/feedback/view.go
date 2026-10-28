package feedback

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Show is one memory put in front of the agent, read back from a shows
// event. Key names it (session|turn|<event ts in Unix ns>|id): the hooks
// get no tool call id, and seq is counted per process. Project is the
// session's project when it was shown.
type Show struct {
	ID, Turn, Agent, Path, Key, Project string
	Rank                                int
	At                                  time.Time
}

// Shows lists every item shown in session, in journal order.
func Shows(events []Event, session string) []Show {
	var out []Show
	for _, e := range events {
		if e.Kind != "shows" || e.Session != session {
			continue
		}
		at := strconv.FormatInt(e.TS.UnixNano(), 10)
		for _, it := range e.Items {
			out = append(out, Show{ID: it.ID, Turn: e.Turn, Agent: e.Agent, Path: e.Path, Project: e.Project, Rank: it.Rank, At: e.TS,
				Key: session + "|" + e.Turn + "|" + at + "|" + it.ID})
		}
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
