//go:build race

package dashboard

// raceEnabled: the race detector slows the pages several times over, so
// their time budgets are measured without it.
const raceEnabled = true
