package splash

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/termstyle"
)

// builder writes panel lines in one character set.
type builder struct {
	src Sources
	sep string // " · ", or " - " on a console that is not UTF-8
}

func (b builder) dim(s string) Seg   { return Seg{Role: termstyle.Secondary, Text: s} }
func (b builder) plain(s string) Seg { return Seg{Plain: true, Text: s} }
func (b builder) dot() Seg           { return b.dim(b.sep) }

func labelled(name string, segs ...Seg) Line {
	return append(Line{{Role: termstyle.Label, Text: name}, {Plain: true, Text: strings.Repeat(" ", 9-len(name))}}, segs...)
}

// lines turns what the readers found into the panel (decision 0084 D4).
func lines(src Sources, f *facts) []Line {
	b := builder{src: src, sep: " · "}
	rule := "─"
	if src.ASCII {
		b.sep, rule = " - ", "-"
	}
	title := Line{{Role: termstyle.Brand, Text: "backsight"}, b.plain(" " + src.Version)}
	if src.Hindsight != "" {
		title = append(title, b.dot(), b.plain("Hindsight "+src.Hindsight))
	}
	out := []Line{title, {b.dim(strings.Repeat(rule, utf8.RuneCountInString(text(title))))}}
	for _, l := range []Line{b.server(f), b.setup(f), b.queue(f), b.checks(f), b.update(f)} {
		if l != nil {
			out = append(out, l)
		}
	}
	for _, l := range out {
		for i := range l {
			if src.Mask != nil {
				l[i].Text = src.Mask(l[i].Text)
			}
			if src.ASCII {
				l[i].Text = asciiOnly(l[i].Text)
			}
		}
	}
	return out
}

func text(l Line) string {
	var s strings.Builder
	for _, g := range l {
		s.WriteString(g.Text)
	}
	return s.String()
}

func asciiOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x80 {
			return '?'
		}
		return r
	}, s)
}

// remoteHost is the server's host on a client (mode remote), else "".
func remoteHost(f *facts) string {
	if f.config == nil || f.config.Server.Mode != "remote" {
		return ""
	}
	u, err := url.Parse(f.config.Server.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (b builder) server(f *facts) Line {
	if f.reach == nil {
		return nil
	}
	r, now := f.reach, b.src.Now
	host := remoteHost(f)
	var lead []Seg
	if host != "" {
		lead = []Seg{b.plain(host + " ")}
	}
	switch {
	case f.config != nil && f.config.Server.Mode == "remote" && f.state != nil && f.state.RemoteServer != nil && f.state.RemoteServer.Mismatch != "":
		return labelled("Server", Seg{Role: termstyle.Fail, Text: "another server answers at " + cmp.Or(host, "the joined address")})
	case !r.FailingSince.IsZero():
		since := now.Sub(r.FailingSince)
		role := termstyle.Warn
		if since >= unreachableWarn {
			role = termstyle.Fail
		}
		zone := b.src.Zone
		if zone == nil {
			zone = time.Local
		}
		segs := append(slices.Clone(lead), Seg{Role: role, Text: "silent since " + r.FailingSince.In(zone).Format("2006-01-02 15:04")},
			b.dim(" ("+recall.Age(since)+")"))
		if f.breaker != nil && now.Before(f.breaker.OpenUntil) {
			segs = append(segs, b.dot(), b.dim("recall paused"))
		}
		return labelled("Server", segs...)
	case !r.LastOK.IsZero():
		return labelled("Server", append(lead, Seg{Role: termstyle.OK, Text: "answering"}, b.dot(), b.dim("last answer "+recall.Age(now.Sub(r.LastOK))+" ago"))...)
	}
	return labelled("Server", append(lead, b.dim("not recorded yet"))...)
}

func (b builder) setup(f *facts) Line {
	if f.noConfig {
		return labelled("Setup", b.plain("not set up yet"), b.dot(), Seg{Role: termstyle.Command, Text: "backsight setup"})
	}
	if f.config == nil {
		return nil
	}
	s := f.config.Server
	runtime := cmp.Or(s.Runtime, "docker")
	switch {
	case s.Role == "server":
		return labelled("Setup", b.plain("server only"), b.dot(), b.plain(runtime))
	case s.Mode == "remote":
		return labelled("Setup", b.plain("client of "+cmp.Or(remoteHost(f), "a remote server")))
	}
	return labelled("Setup", b.plain("host"), b.dot(), b.plain(runtime))
}

// counted paints a nonzero count as a warning, a zero one dim.
func (b builder) counted(n int, more bool, noun string) Seg {
	s := fmt.Sprintf("%d %s", n, noun)
	switch {
	case more:
		s = fmt.Sprintf("%d+ %ss", n, noun)
	case n != 1:
		s += "s"
	}
	if n == 0 {
		return b.dim(s)
	}
	return Seg{Role: termstyle.Warn, Text: s}
}

func (b builder) queue(f *facts) Line {
	if f.reach == nil || f.notes == nil {
		return nil
	}
	sessions := b.counted(f.reach.SessionsWaiting, false, "session")
	notes := b.counted(f.notes.n, f.notes.more, "note")
	return labelled("Queue", sessions, b.dot(), notes, b.dim(" waiting"))
}

func (b builder) checks(f *facts) Line {
	if f.snapshot == nil {
		return nil
	}
	var ok, warn, fail int
	for _, c := range f.snapshot.Report.Checks {
		switch c.Status {
		case "ok", "info":
			ok++
		case "warn":
			warn++
		case "error":
			fail++
		}
	}
	paint := func(n int, word string, role termstyle.Role) Seg {
		if n == 0 {
			role = termstyle.Secondary
		}
		return Seg{Role: role, Text: fmt.Sprintf("%d %s", n, word)}
	}
	segs := []Seg{paint(ok, "ok", termstyle.OK), b.dot(), paint(warn, "warn", termstyle.Warn), b.dot(), paint(fail, "fail", termstyle.Fail)}
	if at := f.snapshot.GeneratedAt; !at.IsZero() {
		segs = append(segs, b.dim(" (last run "+recall.Age(b.src.Now.Sub(at))+" ago)"))
	}
	return labelled("Checks", segs...)
}

func (b builder) update(f *facts) Line {
	if f.state == nil || f.state.UpdateCheck == nil {
		return nil
	}
	latest := f.state.UpdateCheck.Latest
	if latest == "" || !release.NewerThan(latest, b.src.Version) {
		return nil
	}
	return labelled("Update", Seg{Role: termstyle.Accent, Text: latest + " available"}, b.dot(), Seg{Role: termstyle.Command, Text: "backsight update apply"})
}
