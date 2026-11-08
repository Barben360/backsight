// Package sqlitedb holds the SQLite helpers the metrics index and the
// feedback ledger share: connection strings, filesystem detection, the
// migration registry and its runner, the update gate, the exclusive lock
// that takes a file whole, and the class of an error. Its functions are
// pure, their dependencies passed in: each caller keeps its own state
// (its WAL verdicts, its open files, its recovery). Hooks and the MCP
// server must never import it.
package sqlitedb

import _ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"

const driverName = "sqlite"
