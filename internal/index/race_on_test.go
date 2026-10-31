//go:build race

package index

// raceEnabled: the race detector slows ingest several times over, so the
// time budgets are measured without it.
const raceEnabled = true
