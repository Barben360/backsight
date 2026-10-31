package feedback

import (
	"slices"
	"strings"
	"time"
)

// StaleHidden: a memory hidden on this machine is listed once it stayed
// hidden this long without the server rewriting it; before, it waits.
const StaleHidden = 7 * 24 * time.Hour

// StaleLists are the three lists of memory stale, as memory ids, each list
// most shown first, then by id.
type StaleLists struct{ NeverUsed, Hidden, Reported []string }

// Stale sorts l's memories into what needs review, each once: hidden for
// StaleHidden without a rewrite, else reported wrong or outdated, else
// shown live at least shows times and never used. A memory hidden for
// less is left out: the server may still rewrite it. The one definition
// of `memory stale` and of the dashboard's stale counts.
func Stale(l Ledger, shows int, at time.Time) StaleLists {
	res := StaleLists{NeverUsed: []string{}, Hidden: []string{}, Reported: []string{}}
	for id, m := range l.Memories {
		switch {
		case m.Hidden != nil && at.Sub(m.Hidden.Since) >= StaleHidden:
			res.Hidden = append(res.Hidden, id)
		case m.Hidden != nil:
		case m.Wrong > 0:
			res.Reported = append(res.Reported, id)
		case m.Shown-m.Imported.Shown >= shows && m.Credits == 0:
			// Live shows only: an import never counts a miss, so its
			// shows alone say nothing of use.
			res.NeverUsed = append(res.NeverUsed, id)
		}
	}
	for _, ids := range [][]string{res.NeverUsed, res.Hidden, res.Reported} {
		slices.SortFunc(ids, func(a, b string) int {
			if sa, sb := l.Memories[a].Shown, l.Memories[b].Shown; sa != sb {
				return sb - sa
			}
			return strings.Compare(a, b)
		})
	}
	return res
}
