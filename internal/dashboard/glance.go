package dashboard

import (
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// Glance is the "How big is the memory?" card: the total, the memories by
// kind as one bar, and the counts worth a look.
type Glance struct {
	Head          Head
	Total, Growth string
	GrowthNote    string // muted words after Growth; "" none
	Kinds         []KindPart
	Aria, Down    string // Down: the sentence shown while the server does not answer; "" otherwise
	Badges        []Badge
	Nothing       bool // no badge and the server answered: "Nothing to review."
}

// KindPart is one kind of memory: its legend word, its fill class, its
// count, and where its rectangle sits in the bar's 0-100 viewBox.
type KindPart struct {
	Name, Class, Count string
	X, W               float64
}

// Badge is a count of memories worth a look and the page that lists them.
type Badge struct{ Count, Label, Href string }

// kindGap is the space left between two parts of the bar, in viewBox units.
const kindGap = 0.5

// glanceKinds are the kinds of memory in the order the bar draws them: the
// bank's stored key, the word for one, the word for several and the class.
var glanceKinds = []struct{ key, one, many, class string }{
	{"observation", "summary", "summaries", "f-summary"},
	{"world", "fact about you and your work", "facts about you and your work", "f-about"},
	{"experience", "fact from a session", "facts from a session", "f-session"},
}

// glanceHead is the card's frame.
func glanceHead(cli string) Head {
	h := withInfo(fixedHead("glance", "How big is the memory?", "server", cli),
		"About memory at a glance", "Memory at a glance", "bank.size · bank.growth · stale.*",
		"Memories on the server, by kind, and how many were added in the window. The badges count memories worth a look; they never change the verdict.")
	return h
}

// lowerFirst is s with its first letter in lower case.
func lowerFirst(s string) string {
	r := []rune(s)
	if len(r) > 0 {
		r[0] = unicode.ToLower(r[0])
	}
	return string(r)
}

// hundredths rounds v to two decimals, so no length prints in an exponent.
func hundredths(v float64) float64 { return math.Round(v*100) / 100 }

// barOf lays the counts out in the bar: widths are shares of what is left
// of 100 once the gaps between the non-empty parts are taken out.
func barOf(counts []int64) (xs, ws []float64) {
	var total int64
	nonEmpty := 0
	for _, n := range counts {
		total += n
		if n > 0 {
			nonEmpty++
		}
	}
	xs, ws = make([]float64, len(counts)), make([]float64, len(counts))
	if total == 0 {
		return xs, ws
	}
	room := 100 - kindGap*float64(nonEmpty-1)
	x := 0.0
	for i, n := range counts {
		xs[i] = hundredths(x)
		if n == 0 {
			continue
		}
		ws[i] = hundredths(room * float64(n) / float64(total))
		x += room*float64(n)/float64(total) + kindGap
	}
	return xs, ws
}

// glanceOf is the card of the overview r; project is the page's project
// filter, which the memory counts do not follow.
func glanceOf(r *metrics.Result, l format.Locale, loc *time.Location, project string) Glance {
	return glanceFor(r, l, loc, views.Params{Project: project})
}

// growthWindow says over what time the memories were added, from the
// window the result was computed for. days_covered says the server keeps
// less history than the window asks for.
func growthWindow(r *metrics.Result, grow metrics.Metric, p views.Params) string {
	w := r.Window
	custom := p.Since != "" || p.Until != ""
	if covered, ok := extraAs[int](grow, "days_covered"); ok && covered > 0 {
		if custom {
			return "in the " + format.Count(format.Locale{}, int64(covered), "day", "days") + " the server keeps"
		}
		w.Days = covered
		p.Days = covered
	}
	phrase := windowPhrase(p, w)
	switch {
	case phrase == "" || phrase == "last 0 days":
		return "in the window"
	case strings.HasPrefix(phrase, "last"):
		return "in the " + phrase
	case strings.HasPrefix(phrase, "since"):
		return phrase
	}
	return "from " + phrase
}

// glanceFor is glanceOf for the page's parameters.
func glanceFor(r *metrics.Result, l format.Locale, loc *time.Location, p views.Params) Glance {
	g := Glance{Total: dash}
	g.Head = glanceHead(cliFor("bank", "", p))
	project := p.Project

	size := r.Metrics["bank.size"]
	if size.Value != nil {
		g.Total = l.Int(int64(*size.Value))
	}

	// Growth: the memories added over the window, summed from the bank's
	// per-kind counts (the metric's own value is a per-day rate).
	if grow, ok := r.Metrics["bank.growth"]; ok && grow.Value != nil {
		var added int64
		for _, k := range glanceKinds {
			n, _ := extraAs[int64](grow, k.key)
			added += n
		}
		g.Growth = "+" + l.Int(added) + " " + growthWindow(r, grow, p)
		if docs, ok := extraAs[int64](size, "documents"); ok && docs > 0 {
			g.Growth += " · " + l.Int(docs) + " documents"
		}
		if project != "" { // the size above is the project's; the growth is the whole memory's
			g.GrowthNote = "(all projects)"
		}
	}

	// The kinds, when the server counted them.
	counts := make([]int64, len(glanceKinds))
	counted := false
	for i, k := range glanceKinds {
		n, ok := extraAs[int64](size, k.key)
		counts[i], counted = n, counted || ok
	}
	if counted {
		xs, ws := barOf(counts)
		var parts []string
		for i, k := range glanceKinds {
			g.Kinds = append(g.Kinds, KindPart{Name: k.one, Class: k.class, Count: l.Int(counts[i]), X: xs[i], W: ws[i]})
			word := k.many
			if counts[i] == 1 {
				word = k.one
			}
			parts = append(parts, l.Int(counts[i])+" "+word)
		}
		g.Aria = "Memories by kind: " + strings.Join(parts, ", ")
	}

	for _, it := range r.Info {
		label := it.Metric
		if d, ok := metrics.DefOf(it.Metric); ok {
			label = d.Label
		}
		g.Badges = append(g.Badges, Badge{Count: l.Int(it.Count), Label: lowerFirst(label), Href: "/bank#review"})
	}

	down := !r.Scope.ServerOK
	lastOK := r.Scope.ServerOKAt
	if r.Server != nil {
		down, lastOK = !r.Server.OK, r.Server.LastOK
	}
	if down {
		g.Down = "Memory server not answering"
		if lastOK != nil && !lastOK.IsZero() {
			g.Down += " since " + format.Clock(r.GeneratedAt, *lastOK, loc)
		}
		g.Down += "."
	}
	g.Nothing = len(g.Badges) == 0 && !down
	return g
}

// glancePage is glanceOf with the page's own flags on its CLI.
func glancePage(r *metrics.Result, l format.Locale, loc *time.Location, p views.Params) Glance {
	return glanceFor(r, l, loc, p)
}

// latelyHead is the frame of "What happened lately?".
func latelyHead() Head {
	h := fixedHead("lately", "What happened lately?", "machine", "backsight stats sessions --limit 5")
	h.Drill, h.DrillText = "/sessions", "All sessions →"
	return h
}
