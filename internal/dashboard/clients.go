package dashboard

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// The client filter and the split by client (block 8,
// https://github.com/Barben360/backsight/issues/283): on a PC where more
// than one coding tool was active, the scope line offers to count one
// tool's activity, and the Overview splits the window by tool.

// Tools is the scope line's choice of coding tool: every tool, or one.
type Tools struct {
	Choices []Choice
}

// toolsOf is the choice of the page d: nil when the scope names no tool
// (one tool on this PC, and no filter in the URL).
func toolsOf(d PageData) *Tools {
	sc := d.Frame.Scope
	if sc == nil || len(sc.Clients) == 0 {
		return nil
	}
	path := without(d.Frame.Path, "client")
	t := &Tools{Choices: []Choice{{Label: "every tool", Href: path, Current: sc.Client == ""}}}
	for _, c := range sc.Clients {
		t.Choices = append(t.Choices, Choice{Label: views.ClientName(c), Href: withParam(path, "client", c), Current: sc.Client == c})
	}
	return t
}

// withParam is path with k=v added (before its fragment).
func withParam(path, k, v string) string {
	path, frag, hasFrag := strings.Cut(path, "#")
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	path += sep + k + "=" + v
	if hasFrag {
		path += "#" + frag
	}
	return path
}

var clientIn = regexp.MustCompile(`[?&;]client=|&amp;client=`)

// keepClient is the page body with client=<id> on every link inside the
// dashboard, as keepMachines keeps machines=all: but the preferences, the
// assets, the memory text pages and the tool choice's own links, which
// say which tool they show (data-client).
func keepClient(body []byte, client string) []byte {
	if client == "" {
		return body
	}
	return anchorTag.ReplaceAllFunc(body, func(tag []byte) []byte {
		if bytes.Contains(tag, []byte(`data-client=`)) {
			return tag
		}
		return hrefAttr.ReplaceAllFunc(tag, func(attr []byte) []byte {
			href := string(hrefAttr.FindSubmatch(attr)[1])
			if strings.HasPrefix(href, "//") || strings.HasPrefix(href, "/prefs") || strings.HasPrefix(href, "/assets/") || isTextRoute(href) || clientIn.MatchString(href) {
				return attr
			}
			path, frag, hasFrag := strings.Cut(href, "#")
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&amp;"
			}
			path += sep + "client=" + client
			if hasFrag {
				path += "#" + frag
			}
			return []byte(` href="` + path + `"`)
		})
	})
}

// ByClient is the Overview's card that splits the window by coding tool.
type ByClient struct {
	Head Head
	Rows []ByClientRow
}

// ByClientRow is one tool's row: its sessions and recalls, the share of
// recalls that added memory, of turns that used it, and the context added
// per session, worded as the tiles word them.
type ByClientRow struct {
	Name, Href                   string
	Sessions, Recalls            int64
	GotMemory, Used, PerSession string
}

// byClientOf is the card of the overview r on the page at path, nil
// without a split; each tool's name links to the page filtered on it.
func byClientOf(r *metrics.Result, path string) *ByClient {
	if r == nil || len(r.Tables.ByClient) == 0 {
		return nil
	}
	c := &ByClient{Head: fixedHead("by-client", "Which coding tool used memory?", "machine", "backsight stats overview --days 7")}
	for _, row := range r.Tables.ByClient {
		word := func(id string) string {
			d, _ := metrics.DefOf(id)
			return views.FormatValue(d, row.Metrics[id])
		}
		c.Rows = append(c.Rows, ByClientRow{Name: views.ClientName(row.Client), Href: withParam(without(path, "client"), "client", row.Client),
			Sessions: row.Sessions, Recalls: row.Recalls,
			GotMemory: word("recall.hit_rate"), Used: word("used.turn_rate"), PerSession: word("context.per_session")})
	}
	return c
}
