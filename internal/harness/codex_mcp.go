package harness

// Codex's memory-call identity: block 5
// (https://github.com/Barben360/backsight/issues/280), 0085 D4 (and D6's
// tool hints, which the server declares for every harness). The server's
// command line is codex.go's contract (MCPArgs, MCPEnvVars).

// MCPCallSession is the call's _meta.sessionId (the root session, equal to
// the hooks' session_id; a subagent's threadId is not it); Codex gives
// its server no session in the environment. Block 5; "" until then, so
// every call fails closed where the session matters.
func (Codex) MCPCallSession(meta map[string]any, getenv func(string) string) string { return "" }
