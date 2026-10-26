package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// checkPurged looks, as jobs tick does, for purged documents the server
// holds again (backsight issue 5): an older client, or any client unaware
// of tombstones, appended to a purged id and Hindsight 1.1.0 recreated it.
// The tombstones are this machine's and the control bank's; the look
// starts where the tick's last one did (a full one when it never ran or a
// day went by), so it costs one listing most days. Read-only.
func checkPurged(ctx context.Context, cl *hindsight.Client, bank string, env Env) Check {
	c := Check{ID: "purged-documents"}
	st, err := state.Load(env.Dirs.StateFile())
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot read the purge tombstones of this machine", err.Error()
		return c
	}
	all := state.State{Tombstones: st.TombstonesOf(bank)}
	if remote, err := retain.ReadTombstones(ctx, cl, bank); err == nil {
		all.AddTombstones(remote...)
	}
	docs := map[string]bool{}
	for _, t := range all.Tombstones {
		docs[t.Document] = true
	}
	if len(docs) == 0 {
		c.Status, c.Summary = OK, "no purged document"
		return c
	}
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	since, recheck := retain.LookSince(st, bank, len(docs), now().UTC(), false)
	sc, err := retain.FindRecreated(ctx, cl, bank, all.Tombstones, since, recheck)
	if err != nil {
		c.Status, c.Summary, c.Detail = Info, "cannot look for purged documents on the server again", err.Error()
		return c
	}
	if len(sc.Found) == 0 {
		c.Status, c.Summary = OK, fmt.Sprintf("none of the %d purged document(s) is on the server again", len(docs))
		return c
	}
	why := map[string]string{}
	if st.PurgeGuard != nil && st.PurgeGuard.Bank == bank {
		for _, w := range st.PurgeGuard.Waiting {
			why[w.Document] = w.Why
		}
	}
	var names, details []string
	for i, t := range sc.Found {
		if i < 5 {
			names = append(names, fmt.Sprintf("%s (session %s)", t.Document, t.Session))
		}
		if w := why[t.Document]; w != "" {
			details = append(details, t.Document+": "+w)
		}
	}
	if len(sc.Found) > 5 {
		names = append(names, fmt.Sprintf("and %d more", len(sc.Found)-5))
	}
	c.Status = Error
	c.Summary = fmt.Sprintf("%d purged document(s) of bank %s are on the server again, not purged again yet: %s", len(sc.Found), bank, strings.Join(names, ", "))
	c.Detail = "a client unaware of purges (an older backsight on another machine, or a rollback) appended to their ids, and the server recreated them with what it sent"
	if len(details) > 0 {
		c.Detail += "; " + strings.Join(details, "; ")
	}
	c.Fix = "backsight retain purge-again (jobs tick also does it every 15 minutes); update backsight on every machine that writes to this bank"
	if cfg, err := config.Load(env.Dirs.ConfigFile(), env.Dirs, env.Home); err == nil && cfg.LLM.Provider == "claude-code" {
		c.Fix = "backsight retain purge-again --allow-subscription (the server consolidates again after a purge, on your Claude subscription with the claude-code provider); update backsight on every machine that writes to this bank"
	}
	return c
}
