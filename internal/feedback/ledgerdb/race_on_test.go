//go:build race

package ledgerdb

// raceEnabled: the race detector slows the property test's sweep several
// times over, so it runs fewer seeds under it.
const raceEnabled = true
