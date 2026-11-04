package metrics

import (
	"database/sql"

	"github.com/Barben360/backsight/internal/feedback"
)

// helpedRows is how many memories "Helped most" lists.
const helpedRows = 3

// helped is the overview's "Helped most": the memories with the most
// credits of verdict used in w (the window the view is read over), ties
// broken by more shows in w (sources answers are not shows), then by id. A
// window with none reads an empty list. The project filter is the credit's
// and the show's own.
func helped(e env, w Window) ([]HelpedRow, error) {
	since, until, p := w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT c.memory_id, COUNT(*) AS used,
       (SELECT COUNT(*) FROM shows s WHERE s.memory_id = c.memory_id AND s.ts >= ? AND s.ts < ? AND COALESCE(s.path, '') <> ?
          AND (? = '' OR s.project = ?)) AS shown
FROM credits c
WHERE c.verdict = 'used' AND c.memory_id IS NOT NULL AND c.memory_id <> '' AND c.ts >= ? AND c.ts < ? AND (? = '' OR c.project = ?)
GROUP BY c.memory_id ORDER BY used DESC, shown DESC, c.memory_id LIMIT ?`,
		since, until, feedback.PathSources, p, p, since, until, p, p, helpedRows)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HelpedRow{}
	for rows.Next() {
		var r HelpedRow
		if err := rows.Scan(&r.Memory, &r.Used, &r.Shown); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Project = memoryProject(e.tx, out[i].Memory)
		var kind sql.NullString
		if err := e.tx.QueryRowContext(e.ctx, `SELECT fact_type FROM memory_doc WHERE memory_id = ?`, out[i].Memory).Scan(&kind); err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		out[i].FactType = kind.String
	}
	return out, nil
}
