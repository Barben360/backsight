package ledgerdb

import (
	"database/sql"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// row is one memories row as stored: times in unix nanoseconds (0 for
// the zero time), floats as REAL (a float64, exactly), and hidden_since
// NULL for an entry that is not hidden.
type row struct {
	id              string
	shown           int64
	credits         float64
	creditsAt       int64
	misses          float64
	missesAt        int64
	lastShown       int64
	lastCredited    int64
	wrong           int64
	hiddenSince     sql.NullInt64
	hiddenVerdict   string
	hiddenNote      string
	unhiddenAt      int64
	importedShown   int64
	importedCredits float64
}

// memoryColumns are the memories columns in row's order.
const memoryColumns = `memory_id, shown, credits, credits_at, misses, misses_at, last_shown,
	last_credited, wrong, hidden_since, hidden_verdict, hidden_note, unhidden_at,
	imported_shown, imported_credits`

// upsertMemory writes one row whole (args).
const upsertMemory = `INSERT OR REPLACE INTO memories(` + memoryColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// args are r's values in memoryColumns order.
func (r row) args() []any {
	return []any{
		r.id, r.shown, r.credits, r.creditsAt, r.misses, r.missesAt, r.lastShown,
		r.lastCredited, r.wrong, r.hiddenSince, r.hiddenVerdict, r.hiddenNote, r.unhiddenAt,
		r.importedShown, r.importedCredits,
	}
}

// dest are the scan destinations of a SELECT of memoryColumns.
func (r *row) dest() []any {
	return []any{
		&r.id, &r.shown, &r.credits, &r.creditsAt, &r.misses, &r.missesAt, &r.lastShown,
		&r.lastCredited, &r.wrong, &r.hiddenSince, &r.hiddenVerdict, &r.hiddenNote, &r.unhiddenAt,
		&r.importedShown, &r.importedCredits,
	}
}

// rowFromEntry is memory id's entry e as a row.
func rowFromEntry(id string, e feedback.Entry) row {
	r := row{
		id:              id,
		shown:           int64(e.Shown),
		credits:         e.Credits,
		creditsAt:       toNS(e.CreditsAt),
		misses:          e.Misses,
		missesAt:        toNS(e.MissesAt),
		lastShown:       toNS(e.LastShown),
		lastCredited:    toNS(e.LastCredited),
		wrong:           int64(e.Wrong),
		unhiddenAt:      toNS(e.UnhiddenAt),
		importedShown:   int64(e.Imported.Shown),
		importedCredits: e.Imported.Credits,
	}
	if h := e.Hidden; h != nil {
		r.hiddenSince = sql.NullInt64{Int64: toNS(h.Since), Valid: true}
		r.hiddenVerdict, r.hiddenNote = h.Verdict, h.Note
	}
	return r
}

// entryFromRow is r's memory id and entry, rowFromEntry's inverse.
func entryFromRow(r row) (string, feedback.Entry) {
	e := feedback.Entry{
		Shown:        int(r.shown),
		Credits:      r.credits,
		CreditsAt:    fromNS(r.creditsAt),
		Misses:       r.misses,
		MissesAt:     fromNS(r.missesAt),
		LastShown:    fromNS(r.lastShown),
		LastCredited: fromNS(r.lastCredited),
		Wrong:        int(r.wrong),
		UnhiddenAt:   fromNS(r.unhiddenAt),
		Imported:     feedback.Imported{Shown: int(r.importedShown), Credits: r.importedCredits},
	}
	if r.hiddenSince.Valid {
		e.Hidden = &feedback.Hidden{Since: fromNS(r.hiddenSince.Int64), Verdict: r.hiddenVerdict, Note: r.hiddenNote}
	}
	return r.id, e
}

// toNS is t in unix nanoseconds, 0 for the zero time.
func toNS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromNS is toNS's inverse, in UTC.
func fromNS(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}
