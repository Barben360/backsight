package dashboard

import (
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// helpedShown is how many memories the card lists when the window has that
// many; fewer is said in a sentence.
const helpedShown = 3

// Helped is the "Which memories helped most?" card: the memories Claude said
// it used most often in the window, each with a link to its page and, while
// the memory server answers, to its text.
type Helped struct {
	Head  Head
	Intro string       // "Used most in the last 7 days."
	Down  string       // the server's state line while it does not answer; "" otherwise
	Rows  []HelpedLine // the ranked memories
	Few   string       // "Only 2 memories were used in the last 7 days."; "" with 3 or more
	None  string       // the empty sentence; "" when some memory was used
	Foot  string       // the sentence under the rows
}

// HelpedLine is one ranked memory. TextHref is "" while the server does not
// answer: the row then says the text is unavailable.
type HelpedLine struct {
	Rank                int
	ID, Project         Ref
	KindWord, KindClass string // "" for a memory whose kind is not known yet
	Used, Shown         string
	TextHref, TextAria  string
	Unavailable         string // "text unavailable" while the server does not answer
}

// helpedFoot is the sentence under the rows.
const helpedFoot = "Show text opens the memory's page with its text, read live from the server and never kept."

// windowIn says a window phrase after "in": "in the last 7 days" or "from
// Oct 1 to Oct 7".
func windowIn(phrase string) string {
	switch {
	case phrase == "":
		return "in the window"
	case strings.HasPrefix(phrase, "last "):
		return "in the " + phrase
	case strings.HasPrefix(phrase, "since "):
		return phrase
	}
	return "from " + phrase
}

// serverDown says whether the memory server is not answering, and since when
// it last did.
func serverDown(r *metrics.Result) (down bool, lastOK *time.Time) {
	down, lastOK = !r.Scope.ServerOK, r.Scope.ServerOKAt
	if r.Server != nil {
		down, lastOK = !r.Server.OK, r.Server.LastOK
	}
	return down, lastOK
}

// helpedHead is the card's frame, its ⓘ and the command that prints the rows.
func helpedHead(p views.Params) Head {
	h := fixedHead("helped", "Which memories helped most?", "machine", cliFor("overview", "", p.ThisPC())) // stays this PC's: no flag
	h.Info = Info{
		Label: "About helped most", Name: "Which memories helped most?",
		What:  "The 3 memories Claude said it used most often in the window, with how often each was shown.",
		Paras: []string{"The text is read live from the memory server when you ask, on the memory's own page. The dashboard never keeps it."},
		Term:  "overview view · helped",
	}
	h.Drill, h.DrillText = "/memories?sort=used", "All memories, by use →"
	return h
}

// helpedOf is the card of the overview r; window is the page's window phrase.
func helpedOf(r *metrics.Result, l format.Locale, loc *time.Location, p views.Params, window string) Helped {
	in := windowIn(window)
	h := Helped{Head: helpedHead(p), Intro: "Used most " + in + ".", Foot: helpedFoot}
	down, lastOK := serverDown(r)
	if down {
		h.Down = "Memory server not answering"
		if lastOK != nil && !lastOK.IsZero() {
			h.Down += " since " + format.Clock(r.GeneratedAt, *lastOK, loc)
		}
		h.Down += ": the counts are from this PC, the text can't be read until it answers."
	}
	rows := r.Tables.Helped
	if len(rows) > helpedShown {
		rows = rows[:helpedShown]
	}
	for i, m := range rows {
		line := HelpedLine{
			Rank: i + 1, ID: memlink(m.Memory), Project: Ref{None: true},
			Used: l.Int(m.Used), Shown: l.Int(m.Shown),
		}
		if m.Project != "" {
			line.Project = projref(m.Project)
		}
		for _, k := range glanceKinds {
			if k.key == m.FactType {
				line.KindWord, line.KindClass = k.one, k.class
			}
		}
		if down {
			line.Unavailable = "text unavailable"
		} else if line.ID.Href != "" {
			line.TextHref = line.ID.Href + "/text"
			line.TextAria = "Show text of memory " + line.ID.Text
		}
		h.Rows = append(h.Rows, line)
	}
	switch n := len(rows); {
	case n == 0:
		h.None = "No memory was used " + in + " yet."
	case n < helpedShown:
		h.Few = "Only " + l.Int(int64(n)) + " " + nounFor(int64(n), "memory was", "memories were") + " used " + in + "."
	}
	return h
}
