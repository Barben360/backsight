package metrics

import (
	"context"
	"strings"
	"time"
)

// Defs is the definitions view: one row per catalog metric (or the one
// q.ID names; NotFoundError{Kind: "metric"} for an id that is not one), in
// the catalog's order, paged. For a rate (unit ratio) n is the denominator
// of the metric over the default 7-day window ending now, computed from
// the index alone (no server read, no doctor run), and mdc the smallest
// change two such windows can show, once n reaches the metric's minimum n;
// any other row has n 0 and mdc null.
func Defs(ctx context.Context, s Source, q Query) (Result, error) {
	id := strings.TrimSpace(q.ID)
	if _, ok := DefOf(id); id != "" && !ok {
		return Result{}, NotFoundError{Kind: "metric", ID: id}
	}
	w, err := NewWindow(s.now(), 7, time.Time{}, time.Time{}, q.Window.Loc, q.Window.TZ)
	if err != nil {
		return Result{}, err
	}
	q.Window, q.Project = w, "" // the catalog's n is for every project
	return runView(ctx, s, q, "defs", nil, func(e env, r *Result) error {
		rows := []DefRow{}
		for _, d := range Catalog {
			if id != "" && d.ID != id {
				continue
			}
			row := DefRow{Def: d}
			if fam := families.byID[d.ID]; fam != nil && d.Unit == "ratio" {
				obs, _, err := e.run(fam, w)
				if err != nil {
					return err
				}
				if o, ok := obs[d.ID]; ok && !o.Absent && o.Den > 0 {
					row.N = o.Den
					if o.Den >= int64(d.MinN) {
						row.MDC = ptr(MDC(o.Den))
					}
				}
			}
			rows = append(rows, row)
		}
		lo, hi, page := pageBounds(e.q, len(rows))
		r.Tables.Defs, r.Tables.Page = rows[lo:hi], &page
		return nil
	})
}
