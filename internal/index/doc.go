// Package index is the local SQLite metrics index behind the memory
// dashboard. Hooks and the MCP server must never import it.
package index

import _ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"

const driverName = "sqlite"
