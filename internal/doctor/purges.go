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

// purgeLookStale: no tick looked for this long: a recreated document the
// tick has not seen yet is an error. Ticks record their look at least every
// retain.LookRecordEvery, 15 minutes apart.
const purgeLookStale = 2 * retain.LookRecordEvery

// checkPurged looks, as jobs tick does, for purged documents the server
// holds again (backsight issue 5): an older client, or any client unaware
// of tombstones, appended to a purged id and Hindsight 1.1.0 recreated it.
// The tombstones are this machine's and the control bank's. It never looks
// at every tombstone itself: it lists the documents created since the
// tick's last look, and reads the ones recorded as waiting; before any
// look was recorded, it says so. Read-only.
//
// A document waiting for the user (consent, an older client that keeps
// writing to it) is an error; one waiting for the queue or the next look
// is a warning, unless the tick has not looked for an hour.
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
	at := now().UTC()
	since, recheck := retain.LookSince(st, bank, at)
	if since.IsZero() {
		c.Status = Info
		c.Summary = fmt.Sprintf("%d purged document(s), not looked for on the server yet", len(docs))
		c.Fix = "jobs maintain looks for every one once a day; backsight retain purge-again --plan looks now"
		return c
	}
	sc, err := retain.FindRecreated(ctx, cl, bank, all.Tombstones, retain.Lookup{Since: since, Recheck: recheck, Documents: -1})
	if err != nil {
		c.Status, c.Summary, c.Detail = Info, "cannot look for purged documents on the server again", err.Error()
		return c
	}
	if len(sc.Found) == 0 {
		c.Status, c.Summary = OK, fmt.Sprintf("none of the %d purged document(s) is on the server again", len(docs))
		return c
	}
	waiting := map[string]state.Recreated{}
	for _, w := range st.PurgeGuard.Waiting {
		waiting[w.Document] = w
	}
	stale := at.Sub(st.PurgeGuard.CheckedAt) >= purgeLookStale
	c.Status = Warn
	var names, details []string
	for i, t := range sc.Found {
		if i < 5 {
			names = append(names, fmt.Sprintf("%s (session %s)", t.Document, t.Session))
		}
		w, ok := waiting[t.Document]
		if w.Why != "" {
			details = append(details, t.Document+": "+w.Why)
		}
		if w.NeedsUser || (!ok && stale) {
			c.Status = Error
		}
	}
	if len(sc.Found) > 5 {
		names = append(names, fmt.Sprintf("and %d more", len(sc.Found)-5))
	}
	c.Summary = fmt.Sprintf("%d purged document(s) of bank %s are on the server again, not purged again yet: %s", len(sc.Found), bank, strings.Join(names, ", "))
	c.Detail = "a client unaware of purges (an older backsight on another machine, or a rollback) appended to their ids, and the server recreated them with what it sent; until purged again, recall can return it"
	if len(details) > 0 {
		c.Detail += "; " + strings.Join(details, "; ")
	}
	if c.Status == Error {
		// The turns such a client wrote after the purge are sent again to
		// the session's current document at the next send, so none is
		// lost; until the recreated one is purged again, recall can find
		// them twice.
		c.Detail += "; the turns written after the purge are also in the session's current document, so recall can return them twice until it is purged again"
	}
	cfg, cerr := config.Load(env.Dirs.ConfigFile(), env.Dirs, env.Home)
	switch {
	case c.Status == Warn:
		c.Fix = "the next backsight jobs tick purges them again; backsight retain purge-again does it now"
	case cerr == nil && (cfg.LLM.Provider == "claude-code" || cfg.Server.Mode == "remote"):
		c.Fix = "backsight retain purge-again --allow-subscription (the server consolidates again after a purge: on your Claude subscription with the claude-code provider, or on its owner's provider for a server this machine does not run); update backsight on every machine that writes to this bank"
	default:
		c.Fix = "backsight retain purge-again; update backsight on every machine that writes to this bank"
	}
	return c
}
