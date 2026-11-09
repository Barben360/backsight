//go:build race

package cli

// raceEnabled: the race detector slows everything several times over, so
// the time budgets are not checked under it.
const raceEnabled = true
