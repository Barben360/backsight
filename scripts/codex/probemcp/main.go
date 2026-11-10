// Command probemcp is a tiny stdio MCP server for the Codex harness (issue
// 285): one tool per annotation set, to see which ones Codex runs without
// asking, and a log of every call's _meta and of its own environment, so a
// run shows what Codex sends a server. It uses the same go-sdk version as
// backsight's own server.
//
//	probemcp -log <dir>
//
// Tools: readonly (readOnlyHint), closed (destructiveHint false,
// openWorldHint false), plain (no annotations), destructive
// (destructiveHint true). Each answers "ok <tool>".
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	dir := flag.String("log", "", "directory for the call log (required)")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "probemcp: -log is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatal(err)
	}
	p := &probe{dir: *dir, pid: os.Getpid()}
	p.write("start", map[string]any{"env": environ()})
	if err := server(p).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Print(err)
	}
}

type probe struct {
	dir string
	pid int
	n   atomic.Int64
}

// write saves one record as <pid>-<n>-<what>.json.
func (p *probe) write(what string, v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	name := fmt.Sprintf("%d-%03d-%s.json", p.pid, p.n.Add(1), what)
	_ = os.WriteFile(filepath.Join(p.dir, name), append(raw, '\n'), 0o644)
}

func environ() []string {
	env := os.Environ()
	sort.Strings(env)
	return env
}

func ptr(b bool) *bool { return &b }

// tools are the probe's tools and their annotations.
var tools = []struct {
	name string
	ann  *mcp.ToolAnnotations
}{
	{"readonly", &mcp.ToolAnnotations{ReadOnlyHint: true}},
	{"closed", &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
	{"plain", nil},
	{"destructive", &mcp.ToolAnnotations{DestructiveHint: ptr(true)}},
}

func server(p *probe) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "1"}, nil)
	for _, t := range tools {
		name := t.name
		mcp.AddTool(s, &mcp.Tool{Name: name, Description: "Probe tool " + name + ": answers ok.", Annotations: t.ann},
			func(_ context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
				p.write("call-"+name, map[string]any{"meta": req.Params.Meta, "session": req.Session.ID()})
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok " + name}}}, nil, nil
			})
	}
	return s
}
