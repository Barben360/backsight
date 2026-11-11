package harness

// codex_mcp.go's ledger: block 5, https://github.com/Barben360/backsight/issues/280.
func init() {
	codexOwns("codex_mcp.go", codexPart{
		stubs: map[string]func() bool{
			// A real implementation reads the call's _meta.sessionId.
			"MCPCallSession": func() bool {
				return (Codex{}).MCPCallSession(map[string]any{"sessionId": "01a1c72b-9533-7bb0-9002-52dca295a387"}, func(string) string { return "" }) == ""
			},
		},
	})
}
