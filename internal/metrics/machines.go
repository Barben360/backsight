package metrics

import (
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/machines"
)

// The "several PCs" switch: machines "this" (this PC's figures) or "all"
// (merged with the other PCs' published days, ingested into the pc_* tables
// of the index). The views that merge are mergingViews; the verdict and
// every list stay this PC's (assess runs on env.this()).

// mergingViews are the views whose figures merge with the other PCs'
// (overview, quality, cost, pipeline, days, releases). Bank and system have
// nothing to merge; projects, sessions and memories answer this PC's.
var mergingViews = []string{"overview", "quality", "cost", "pipeline", "days", "releases"}

// thisOnlyViews always answer this PC's figures and say so.
var thisOnlyViews = []string{"projects", "sessions", "memories"}

// sharingOn: this PC publishes its figures and reads the others' (feedback
// on and the ledger shared).
func sharingOn(c config.Config) bool { return c.Feedback.Enabled && c.Feedback.ShareLedger }

// machines is the env's switch, "this" when unset.
func (e env) machines() string {
	if e.q.Machines == "all" {
		return "all"
	}
	return "this"
}

// this is e answering this PC alone; it shares e's memo, whose key holds
// the switch.
func (e env) this() env {
	e.q.Machines = "this"
	return e
}

// merging reports whether the figures of this view merge the other PCs':
// all PCs asked for, a view that merges, sharing on.
func (e env) merging() bool {
	return e.q.Machines == "all" && slices.Contains(mergingViews, e.view) && sharingOn(e.src.Config)
}

// othersAt is the other PCs whose days a merged window adds, as their full
// machine ids. Task 8's merge fills it; until then no other PC merges.
func othersAt(e env, w Window) []string { return nil }

// short is the label of machine id: its first 8 characters, or 13 when
// another counted PC shares them (R47).
func short(id string, ids []string) string {
	if len(id) < 13 {
		return id
	}
	for _, o := range ids {
		if o != id && len(o) >= 8 && o[:8] == id[:8] {
			return id[:13]
		}
	}
	return id[:8]
}

// scopePCs reads the scope of the PCs (M9) and whether at least one other
// PC is counted.
func scopePCs(e env) (*PCs, bool, error) {
	p := &PCs{Sharing: sharingOn(e.src.Config), List: []PCRow{}}
	if !p.Sharing {
		return p, false, nil
	}
	loc := viewLoc(e)
	var self SelfPC
	if e.src.Self != nil {
		self = e.src.Self()
	}
	thisCounts := !e.src.Config.ServerOnly()

	type other struct {
		machine, os, first string
		at                 int64
		until              sql.NullString
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT machine, COALESCE(os, ''), COALESCE(first_seen, ''), at, until_day FROM pc_docs
WHERE at >= ? AND at <= ? ORDER BY first_seen, machine`,
		e.now.Add(-machines.DropAfter).UnixMilli(), e.now.Add(machines.MaxSkew).UnixMilli())
	if err != nil {
		return nil, false, err
	}
	var others []other
	for rows.Next() {
		var o other
		if err := rows.Scan(&o.machine, &o.os, &o.first, &o.at, &o.until); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		others = append(others, o)
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	var ids []string
	if thisCounts && self.Machine != "" {
		ids = append(ids, self.Machine)
	}
	for _, o := range others {
		ids = append(ids, o.machine)
	}

	w := e.q.Window
	if thisCounts {
		row := PCRow{Machine: "", This: true, OS: self.OS, FirstSeen: self.FirstSeen}
		if self.Machine != "" {
			row.Machine = short(self.Machine, ids)
		}
		p.List = append(p.List, row)
		var n int
		if err := e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(*) FROM tel_prompt WHERE ts >= ? AND ts < ? AND (reason IS NULL OR reason <> 'machine')`,
			w.Since.UnixMilli(), w.Until.UnixMilli()).Scan(&n); err != nil {
			return nil, false, err
		}
		if n > 0 {
			p.Active++
		}
	}
	for _, o := range others {
		row := PCRow{Machine: short(o.machine, ids), OS: o.os, FirstSeen: o.first,
			Stale: time.UnixMilli(o.at).Before(e.now.Add(-machines.StaleAfter))}
		if o.until.Valid && o.until.String != "" {
			u := o.until.String
			row.Until = &u
			if p.Until == nil || u < *p.Until {
				p.Until = &u
			}
		}
		if row.Stale {
			p.Stale++
		}
		var n int
		if err := e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(*) FROM pc_days WHERE machine = ? AND version = '' AND prompts > 0 AND noon >= ? AND noon < ?`,
			o.machine, w.Since.UnixMilli(), w.Until.UnixMilli()).Scan(&n); err != nil {
			return nil, false, err
		}
		if n > 0 {
			p.Active++
		}
		p.List = append(p.List, row)
	}
	p.Count = len(p.List)

	if err := e.tx.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM pc_ignored`).Scan(&p.Ignored); err != nil {
		return nil, false, err
	}
	var fetched sql.NullInt64
	switch err := e.tx.QueryRowContext(e.ctx, `SELECT fetched_at FROM pc_fetch WHERE id = 1`).Scan(&fetched); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, false, err
	case fetched.Valid:
		t := time.UnixMilli(fetched.Int64).In(loc)
		p.FetchedAt = &t
	}
	return p, len(others) > 0, nil
}
