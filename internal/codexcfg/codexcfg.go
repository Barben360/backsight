package codexcfg

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/claudecfg"
)

// The edits backsight makes to Codex's config.toml (decision 0085, D6,
// D8, D10): each an Apply for a claudecfg.Edit.

// The plugins whose hook trust Codex keeps under [hooks.state."<plugin
// id>:…"] (0085 D2, D5).
var trustPlugins = []string{"backsight@backsight", "backsight-hooks@backsight"}

// IsTrustTable reports a [hooks.state."<key>"] table of backsight's plugins:
// what `codex plugin remove` leaves behind (0085 D8).
func IsTrustTable(path []string) bool {
	if len(path) != 3 || path[0] != "hooks" || path[1] != "state" {
		return false
	}
	for _, p := range trustPlugins {
		if strings.HasPrefix(path[2], p+":") {
			return true
		}
	}
	return false
}

// RemoveTrust removes backsight's [hooks.state] tables, each whole, and
// nothing else: other hooks' trust and the [hooks.state] table itself
// (Codex's own) stay.
func RemoveTrust(doc []byte) ([]byte, []claudecfg.Change, error) {
	return RemoveTables(doc, IsTrustTable)
}

// MemoriesKey turns Codex's own memories on ([features] memories = true).
var MemoriesKey = []string{"features", "memories"}

// MemoriesOn reports that config.toml turns Codex's own memories on (a
// profile or a command line flag may still turn them on otherwise: codex
// features list tells the effective state).
func MemoriesOn(doc []byte) (bool, error) {
	v, ok, err := Get(doc, MemoriesKey)
	return ok && v == "true", err
}

// MemoriesOff removes `memories = true`, as `codex features disable
// memories` does (an empty [features] table stays); any other value is
// left as it is.
func MemoriesOff(doc []byte) ([]byte, []claudecfg.Change, error) {
	if on, err := MemoriesOn(doc); err != nil || !on {
		return doc, nil, err
	}
	out, ch, err := Remove(doc, MemoriesKey)
	return out, []claudecfg.Change{ch}, err
}

// MemoriesBack puts `memories = true` back where pre (config.toml before
// MemoriesOff) had it, unless memories is set again since.
func MemoriesBack(pre []byte) func(doc []byte) ([]byte, []claudecfg.Change, error) {
	return func(doc []byte) ([]byte, []claudecfg.Change, error) {
		out, ch, err := Restore(doc, pre, MemoriesKey)
		return out, []claudecfg.Change{ch}, err
	}
}

// MemoriesTrue sets `memories = true` in [features], whatever was there
// (`client codex-memories on`: the user asks for it).
func MemoriesTrue(doc []byte) ([]byte, []claudecfg.Change, error) {
	out, ch, err := Set(doc, MemoriesKey, "true")
	return out, []claudecfg.Change{ch}, err
}

// MemoriesBackOn sets `memories = true` only while the key is absent, as
// backsight's MemoriesOff left it: uninstall's way back. A value the user
// set since (memories = false included) is theirs and stays.
func MemoriesBackOn(doc []byte) ([]byte, []claudecfg.Change, error) {
	if _, there, err := Get(doc, MemoriesKey); err != nil || there {
		return doc, nil, err
	}
	return MemoriesTrue(doc)
}

// TrustKeys are backsight's [hooks.state."<key>"] keys in config.toml, read
// with a full TOML decoder: what to name when the narrow editor cannot
// remove them.
func TrustKeys(doc []byte) ([]string, error) {
	v, ok, err := Lookup(doc, []string{"hooks", "state"})
	if err != nil || !ok {
		return nil, err
	}
	m, _ := v.(map[string]any)
	var out []string
	for k := range m {
		if IsTrustTable([]string{"hooks", "state", k}) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ApprovalFallback approves tools of backsight's memory server without a
// prompt: [plugins."backsight@backsight".mcp_servers.memory.tools.<tool>]
// approval_mode = "approve", the key Codex's own "Always allow" writes. It
// is the fallback of 0085 D6, for a Codex that stops honouring the tools'
// hints: nothing applies it by default.
func ApprovalFallback(tools ...string) func(doc []byte) ([]byte, []claudecfg.Change, error) {
	return func(doc []byte) ([]byte, []claudecfg.Change, error) {
		var changes []claudecfg.Change
		for _, t := range tools {
			out, ch, err := Set(doc, []string{"plugins", "backsight@backsight", "mcp_servers", "backsight", "tools", t, "approval_mode"}, `"approve"`)
			if err != nil {
				return nil, nil, err
			}
			doc, changes = out, append(changes, ch)
		}
		return doc, changes, nil
	}
}

// MCPServerTable reports that config.toml configures an MCP server named
// name ([mcp_servers.<name>]), the user's own. It reads the whole file
// with a full TOML decoder: a check never fails on what only the narrow
// editor does not handle.
func MCPServerTable(doc []byte, name string) (bool, error) {
	_, ok, err := Lookup(doc, []string{"mcp_servers", name})
	return ok, err
}

// Lookup is the value at path in config.toml, decoded in full (for the
// reads; the edits are the narrow editor's).
func Lookup(doc []byte, path []string) (any, bool, error) {
	tree, err := decode(doc)
	if err != nil {
		return nil, false, fmt.Errorf("config.toml is not valid TOML: %w", err)
	}
	var v any = tree
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		if v, ok = m[p]; !ok {
			return nil, false, nil
		}
	}
	return v, true, nil
}
