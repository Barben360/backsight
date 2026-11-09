package dashboard

import (
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// summaryWidth is the longest line of the summary text, in characters: it
// pastes cleanly into an issue, a chat or a terminal.
const summaryWidth = 72

// summaryText is the Overview as plain text, to paste into an issue or a
// chat: counts, rates, statuses and commands, never a memory id, a session id
// or any text of a memory or a prompt. It is written in English whatever the
// browser's language, so that what is pasted does not depend on it. window is
// the page's window phrase; version is this binary's.
//
// On all PCs (r adds them up) the verdict is this PC's: own is this PC's
// answer to the same view, which the banner's title and the counts of the
// tiles' links read.
func summaryText(r *metrics.Result, p views.Params, loc *time.Location, window, version string, own ...*metrics.Result) string {
	l := format.Locale{}
	loc = zoneOr(loc)
	var b []string
	add := func(s ...string) { b = append(b, s...) }
	var thisPC *metrics.Result
	if len(own) > 0 {
		thisPC = own[0]
	}
	all := r.Scope.Machines == "all" && r.Scope.PCs != nil
	banner := r
	if all && thisPC != nil {
		banner = thisPC // the verdict and its title are this PC's
	}

	segs := []string{"backsight", "Is my memory working?", windowWithDays(r, window, loc)}
	if p.Project != "" {
		segs = append(segs, "project "+views.Clip(p.Project))
	}
	add(joinSegments(segs)...)
	if all {
		add("Activity on all PCs on this server (" + strconv.Itoa(r.Scope.PCs.Count) + ") · " + memoryOn(r.Scope.Server))
		if notes := pcNotes(r.Scope, r.GeneratedAt); notes != "" {
			add(notes)
		}
	} else {
		add("Activity on this PC · " + memoryOn(r.Scope.Server))
	}
	add("")

	title := bannerOf(banner, l, loc, p).Title
	if all {
		title = strings.TrimSuffix(title, ".") + " (this PC)."
	}
	add(wrap(title, "", ""))
	b = append(b, axisLines(r, l, p, loc, thisPC)...)
	add("")

	if s := summaryPage(r, l, window); s != nil {
		line := s.When + s.Who + ": " + s.Prompts + " " + s.PromptsNoun + " got memory, Claude used it in " + s.Turns + " " + s.TurnsNoun +
			", about " + s.Tokens + " tokens added; extraction cost " + s.Cost + " " + strings.TrimSuffix(s.Scope, ".") + "."
		add(wrap(line, "", ""))
		add("")
	}

	add(wrap(reportedWrong(r, p, l, loc, thisPC), "", "  "))
	if len(r.Attention) == 0 {
		add("Needs attention: nothing.")
	} else {
		add("Needs attention:")
		for i, it := range r.Attention {
			text := strings.TrimSpace(it.Text)
			if it.Command != "" {
				text = strings.TrimSuffix(text, ".") + ": " + it.Command
			}
			add(wrap(strconv.Itoa(i+1)+". "+text, "  ", "     "))
		}
		if r.AttentionMore > 0 {
			add("  and " + l.Int(int64(r.AttentionMore)) + " more on the System page")
		}
	}
	add(wrap(memoryLine(r, l, window), "", "  "))
	add("")
	add("backsight " + version + " · " + r.GeneratedAt.In(loc).Format("2006-01-02 15:04"))
	return strings.Join(b, "\n")
}

// windowWithDays is the window phrase with the days it spans: "last 7 days
// (Oct 24 to Oct 30)". A window given by dates is already those dates.
func windowWithDays(r *metrics.Result, window string, loc *time.Location) string {
	if window == "" || !strings.HasPrefix(window, "last ") || r.Window.Until.IsZero() {
		return window
	}
	const f = "Jan 2"
	last := r.Window.Until.In(loc)
	first := last.AddDate(0, 0, 1-max(1, r.Window.Days))
	if r.Window.Days <= 1 { // 24 h: the days it touches
		first = r.Window.Since.In(loc)
	}
	if first.Format(f) == last.Format(f) {
		return window + " (" + last.Format(f) + ")"
	}
	return window + " (" + first.Format(f) + " to " + last.Format(f) + ")"
}

// joinSegments writes segments with " · " between them, a segment that would
// pass the width starting a line of its own, indented.
func joinSegments(segs []string) []string {
	var lines []string
	cur := segs[0]
	for _, s := range segs[1:] {
		if utf8.RuneCountInString(cur)+3+utf8.RuneCountInString(s) > summaryWidth {
			lines = append(lines, cur)
			cur = "  " + s
			continue
		}
		cur += " · " + s
	}
	return append(lines, cur)
}

// memoryHost names the memory server: its address when it is on this PC (a
// loopback address or localhost), else "a remote server". The text is made
// to be pasted, and a LAN host or address is private.
func memoryHost(server string) string {
	host := strings.TrimSpace(server)
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if ip := net.ParseIP(name); name == "localhost" || ip != nil && ip.IsLoopback() {
		return host
	}
	return "a remote server"
}

// axisLines are the four axes, one line each: the name, the state word and
// the note, aligned in columns. On all PCs the notes read the added-up
// figures and say so, and Cost's line becomes the Speed and Context lines of
// the tiles, whose words are the tiles'.
func axisLines(r *metrics.Result, l format.Locale, p views.Params, loc *time.Location, own *metrics.Result) []string {
	if r.Verdict == nil {
		return nil
	}
	type row struct{ name, word, note string }
	var rows []row
	cause := rootCause(r.Verdict)
	wordWidth := 6
	all := r.Scope.Machines == "all" && r.Scope.PCs != nil
	vr := verdictView(r) // the lines quote the verdict's last 7 days, as the banner says
	var tiles map[string]Tile
	if all {
		tiles = map[string]Tile{}
		for _, g := range tilesOf(r, p, l, loc, false, own) {
			for _, t := range g.Tiles {
				tiles[t.Name] = t
			}
		}
	}
	for _, an := range views.AxisNames {
		ax, ok := r.Verdict.Axes[an.Key]
		if !ok {
			continue
		}
		w, _ := axisWord(ax)
		wordWidth = max(wordWidth, utf8.RuneCountInString(w))
		if all && an.Key == "cost" && (ax.State == metrics.AxisOK || ax.State == metrics.AxisAttention) {
			for _, name := range []string{"Speed", "Context"} {
				t := tiles[name]
				wordWidth = max(wordWidth, utf8.RuneCountInString(t.Word))
				rows = append(rows, row{name, t.Word, mergedCostLine(vr, l, name)})
			}
			continue
		}
		note := axisSentence(vr, l, an.Key, ax, cause)
		if all && (ax.State == metrics.AxisOK || ax.State == metrics.AxisAttention) && mergedAxis(an.Key, note) {
			note += ", all PCs"
		}
		rows = append(rows, row{an.Name, w, note})
	}
	var out []string
	for _, x := range rows {
		head := "  " + padRight(x.name, 11) + padRight(x.word, wordWidth+2)
		out = append(out, wrap(x.note, head, strings.Repeat(" ", utf8.RuneCountInString(head))))
	}
	return out
}

// mergedAxis says the note of axis key reads figures that add up the PCs:
// Found's and Right's, and Flowing's share sent (its failures stay this PC's).
func mergedAxis(key, note string) bool {
	switch key {
	case "found", "right":
		return true
	case "flowing":
		return strings.HasSuffix(note, " sent")
	}
	return false
}

// mergedCostLine is the text of the Speed or Context line on all PCs: the
// range of the slowest 1 in 20 and the average per session, as the tiles say
// them, with this PC's exact high value beside the average.
func mergedCostLine(r *metrics.Result, l format.Locale, name string) string {
	if name == "Speed" {
		m := r.Metrics["recall.latency"]
		if lo, hi, ok := views.RangeMS(m, "range_ms"); ok && slices.Contains(m.Tags, "approximate") {
			text := l.Range(lo, hi)
			if hi == nil {
				return "slowest 1 in 20 " + text + " (approximate), all PCs"
			}
			return "slowest 1 in 20 between " + strings.Replace(text, "–", " and ", 1) + " (approximate), all PCs"
		}
		if m.Value != nil {
			return "slowest 1 in 20 " + l.Duration(*m.Value) + ", all PCs"
		}
		return "no reading yet, all PCs"
	}
	m := r.Metrics["context.per_session"]
	if m.Value == nil {
		return "no reading yet, all PCs"
	}
	s := l.Size(*m.Value, "chars") + " per session"
	if slices.Contains(m.Tags, "average") {
		s += " on average"
	}
	s += ", all PCs"
	if p95, ok := extraAs[float64](m, "p95"); ok && slices.Contains(m.Tags, "average") {
		s += "; high value " + l.Size(p95, "") + " on this PC"
	}
	return s
}

// pcNotes is the line under the scope line of the text on all PCs: the PCs
// that went quiet, the documents left out and the filter that spares the
// other PCs; "" when there is none.
func pcNotes(sc metrics.Scope, now time.Time) string {
	var notes []string
	if st := sc.PCs.Stale; st > 0 {
		var oldest *time.Time
		for _, row := range sc.PCs.List {
			if row.Stale && row.SentAt != nil && (oldest == nil || row.SentAt.Before(*oldest)) {
				oldest = row.SentAt
			}
		}
		since := "over 2 days"
		if oldest != nil {
			since = sentFor(*oldest, now)
		}
		notes = append(notes, plural(st, "PC", "PCs")+" sent nothing for "+since)
	}
	if n := sc.PCs.Ignored; n > 0 {
		notes = append(notes, plural(n, "document", "documents")+" ignored")
	}
	if sc.OthersUnfiltered {
		notes = append(notes, "other PCs not filtered")
	}
	return strings.Join(notes, " · ")
}

// axisSentence is an axis's note as the text writes it: the note the banner
// has, with the interval and the change of the rates, and Flowing's time to
// recall.
func axisSentence(r *metrics.Result, l format.Locale, key string, ax metrics.Axis, cause bool) string {
	note := axisNote(r, l, key, ax, cause)
	if ax.State != metrics.AxisOK && ax.State != metrics.AxisAttention {
		return note
	}
	switch key {
	case "found":
		note += rateTail(r, l, "recall.hit_rate")
	case "right":
		m := r.Metrics["used.turn_rate"]
		if m.Value != nil {
			if slices.Contains(m.Tags, "judged_28d") {
				note = "used in " + l.Percent(*m.Value) + " of turns" + intervalOf(l, m) + ", judged on 28 days"
			} else {
				note = "used in " + l.Percent(*m.Value) + " of turns" + intervalOf(l, m) + deltaOf(l, "used.turn_rate", m)
			}
		}
	case "cost":
		if c := r.Metrics["context.per_session"]; c.Value != nil {
			note += "; context high " + l.Size(*c.Value, "chars")
		}
	case "flowing":
		if t := r.Metrics[timeToRecallID]; t.Value != nil && !cause {
			note += "; memory back in ~" + longDuration(l, *t.Value)
		}
	}
	return note
}

// intervalOf is " (21–35 %)", the metric's interval; "" without one.
func intervalOf(l format.Locale, m metrics.Metric) string {
	if m.CI == nil {
		return ""
	}
	return " " + l.Interval(*m.CI)
}

// deltaOf is ", +2 pts", the metric's change against the window before;
// "" when it has none to show.
func deltaOf(l format.Locale, id string, m metrics.Metric) string {
	d, _ := metrics.DefOf(id)
	dl, ok := l.Delta(d, m)
	if !ok {
		return ""
	}
	text, _, _ := strings.Cut(dl.Text, ", ")
	return ", " + text
}

// rateTail is the interval and change of a rate, written after its note.
func rateTail(r *metrics.Result, l format.Locale, id string) string {
	m := r.Metrics[id]
	if m.Value == nil {
		return ""
	}
	return intervalOf(l, m) + deltaOf(l, id, m)
}

// reportedWrong is the line about the verdicts: the Reported wrong tile's
// word or value, and the count behind it.
func reportedWrong(r *metrics.Result, p views.Params, l format.Locale, loc *time.Location, own *metrics.Result) string {
	var t Tile
	for _, g := range tilesOf(r, p, l, loc, false, own) {
		for _, x := range g.Tiles {
			if x.Name == "Reported wrong" {
				t = x
			}
		}
	}
	m := r.Metrics["verdict.wrong_rate"]
	num, den, ok := numDen(m)
	var what string
	switch {
	case t.Word == "learning" || t.Word == "unreliable":
		what = t.Word
	case m.Status == metrics.StatusTooFew:
		what = "too few to judge"
	case m.Value != nil:
		what = l.Percent(*m.Value)
	default:
		return "Reported wrong: nothing judged yet."
	}
	s := "Reported wrong: " + what
	if ok {
		s += " (" + l.Int(num) + " of " + format.Count(l, den, "verdict", "verdicts") + ")"
	}
	if r.Scope.Machines == "all" && r.Scope.PCs != nil && m.Value != nil {
		s += ", all PCs"
	}
	return s + "."
}

// memoryLine is the size of the memory and how much it grew in the window.
func memoryLine(r *metrics.Result, l format.Locale, window string) string {
	size := r.Metrics["bank.size"]
	if size.Value == nil {
		return "Memory: size not known (" + serverAbsent(r) + ")."
	}
	s := "Memory: " + l.Int(int64(*size.Value)) + " memories"
	if grow, ok := r.Metrics["bank.growth"]; ok && grow.Value != nil {
		var added int64
		for _, k := range glanceKinds {
			n, _ := extraAs[int64](grow, k.key)
			added += n
		}
		s += ", +" + l.Int(added) + " " + windowIn(window)
	}
	return s + "."
}

// wrap breaks s at spaces into lines of at most summaryWidth characters, the
// first starting with first and the others with rest.
func wrap(s, first, rest string) string {
	var lines []string
	cur := first
	empty := true
	for _, w := range strings.Fields(s) {
		if !empty && utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) > summaryWidth {
			lines = append(lines, cur)
			cur, empty = rest+w, false
			continue
		}
		if empty {
			cur += w
		} else {
			cur += " " + w
		}
		empty = false
	}
	return strings.Join(append(lines, cur), "\n")
}

// padRight is s followed by spaces up to n characters.
func padRight(s string, n int) string {
	if k := utf8.RuneCountInString(s); k < n {
		return s + strings.Repeat(" ", n-k)
	}
	return s
}
