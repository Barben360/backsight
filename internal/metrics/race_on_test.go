//go:build race

package metrics

// raceEnabled: the race detector slows the views several times over, so
// their time budgets are measured without it.
const raceEnabled = true
