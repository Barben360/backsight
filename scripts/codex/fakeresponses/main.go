// Command fakeresponses is a deterministic stand-in for the OpenAI
// Responses API, for running a real Codex CLI with no account and no cost
// (Codex epic, issue 285; CI mode of issue 284). Codex is pointed at it
// through a custom model provider:
//
//	model_provider = "fake"
//	[model_providers.fake]
//	name = "fake"
//	base_url = "http://<host>:<port>/v1"
//	wire_api = "responses"
//
// Every request is logged whole to -log (one JSON file per request: path,
// headers with credentials redacted, body), so a run shows exactly what
// Codex sends the model: the tools list, the instructions, the injected
// hook context.
//
// The scripted model reads its moves from the conversation itself: the
// last input message holding a <<fake [...]>> block, a JSON array of
// steps. The step to play is the number of tool calls already answered
// after that message, so the server keeps no state and a resumed or
// forked thread replays the same way.
//
//	{"call": "memory_search", "args": {...}}  a tool call; the name is
//	    matched against the request's tools (exact, then by suffix, inside
//	    namespaces too), so a script does not depend on how Codex names
//	    a tool
//	{"say": "text"}                            an assistant message; ends the turn
//
// With no script the model answers "ok" (title, compaction and subagent
// requests included). When the steps run out it answers "done".
// Compaction requests get a summary. Codex's own memories are driven too:
// a structured-output request (the per-thread extraction) gets a JSON
// object filling its schema, and the consolidation agent writes the two
// files Codex checks.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	dir := flag.String("log", "", "directory for the request log (required)")
	addrFile := flag.String("addr-file", "", "write the address it listens on (port 0 resolved) to this file")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "fakeresponses: -log is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatal(err)
	}
	s := &server{dir: *dir}
	l, err := listen(*addr, *addrFile)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fakeresponses listening on %s, logging to %s", l.Addr(), *dir)
	log.Fatal(http.Serve(l, s))
}

// listen listens on addr and, when file is set, writes there the address
// it got (a port 0 resolved), so a caller can wait for the file.
func listen(addr, file string) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if file != "" {
		if err := os.WriteFile(file, []byte(l.Addr().String()+"\n"), 0o644); err != nil {
			_ = l.Close()
			return nil, err
		}
	}
	return l, nil
}

type server struct {
	dir string
	mu  sync.Mutex
	n   int
}

// redacted are the headers whose values never reach the log.
var redacted = map[string]bool{"Authorization": true, "Cookie": true, "Openai-Api-Key": true, "X-Api-Key": true}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := readAll(r)
	s.record(r, body)
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses"):
		events, err := reply(body, compaction(r.Header.Get("X-Codex-Turn-Metadata")))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(events)))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	default:
		http.NotFound(w, r)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	return b.Bytes(), err
}

// record writes one request to the log as NNNN.json.
func (s *server) record(r *http.Request, body []byte) {
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	headers := map[string]string{}
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := strings.Join(r.Header[k], ", ")
		if redacted[k] {
			v = "[redacted]"
		}
		headers[k] = v
	}
	entry := map[string]any{"method": r.Method, "path": r.URL.Path, "headers": headers}
	var parsed any
	if json.Unmarshal(body, &parsed) == nil {
		entry["body"] = parsed
	} else {
		entry["body_raw"] = string(body)
	}
	// Text as Codex sent it: no < for "<", so the log reads and greps
	// like the conversation.
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(entry)
	_ = os.WriteFile(filepath.Join(s.dir, fmt.Sprintf("%04d.json", n)), raw.Bytes(), 0o644)
}

// request is the part of a Responses request the scripted model reads.
type request struct {
	Input []map[string]any `json:"input"`
	Tools []map[string]any `json:"tools"`
	// Text.Format.Schema: a structured-output request (Codex's memory
	// extraction asks for one).
	Text struct {
		Format struct {
			Schema map[string]any `json:"schema"`
		} `json:"format"`
	} `json:"text"`
}

// structured is a JSON object filling every required string property of
// schema with a recognisable value, so Codex accepts the answer.
func structured(schema map[string]any) string {
	out := map[string]any{}
	props, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]any)
	for _, r := range required {
		name, _ := r.(string)
		v := "fake " + name
		if name == "rollout_slug" {
			v = "fake-slug"
		}
		if p, ok := props[name].(map[string]any); ok && p["type"] == "boolean" {
			out[name] = false
			continue
		}
		out[name] = v
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

// The consolidation agent of Codex's memories is recognised by its prompt:
// v1's asks for MEMORY.md and memory_summary.md under a folder it names,
// v2's for memory_summary.md alone.
var (
	consolidationV1 = regexp.MustCompile(`Memory Writing Agent: Phase 2 \(Consolidation\)[\s\S]*?Folder structure \(under (\S+?)/?\):`)
	consolidationV2 = regexp.MustCompile("Consolidate the supplied rollout summaries into `memory_summary.md`[\\s\\S]*?Read `phase2_workspace_diff.md` in `(\\S+?)/?` first")
)

type consolidation struct {
	root     string
	handbook bool // v1: MEMORY.md too
}

func consolidationRoot(input []map[string]any) (consolidation, bool) {
	for _, it := range input {
		text := messageText(it)
		if m := consolidationV1.FindStringSubmatch(text); m != nil {
			return consolidation{root: m[1], handbook: true}, true
		}
		if m := consolidationV2.FindStringSubmatch(text); m != nil {
			return consolidation{root: m[1]}, true
		}
	}
	return consolidation{}, false
}

// consolidationSteps writes the files Codex checks after consolidation
// (memory_summary.md starts with "v1" and carries four headings; v1 also
// needs MEMORY.md). The agent runs its tools through Codex's code mode: one
// JavaScript `exec` call running exec_command.
func consolidationSteps(c consolidation) []step {
	cmd := fmt.Sprintf(`cd %q && printf 'v1\n## User Profile\nfake\n## User preferences\nfake\n## General Tips\nfake\n## What%ss in Memory\nfake\n' > memory_summary.md`, c.root, `'"'"'`)
	if c.handbook {
		cmd += ` && printf '# Memory\n\nfake handbook entry\n' > MEMORY.md`
	}
	quoted, _ := json.Marshal(cmd)
	js, _ := json.Marshal(`await tools.exec_command({cmd: ` + string(quoted) + `}); text("written");`)
	done := "consolidated"
	return []step{{Call: "exec", Args: js}, {Say: &done}}
}

// allTools is the request's tools and those an additional_tools input item
// carries (Codex's code mode passes its tools that way).
func allTools(req request) []map[string]any {
	list := append([]map[string]any{}, req.Tools...)
	for _, it := range req.Input {
		if it["type"] != "additional_tools" {
			continue
		}
		extra, _ := it["tools"].([]any)
		for _, e := range extra {
			if m, ok := e.(map[string]any); ok {
				list = append(list, m)
			}
		}
	}
	return list
}

// answeredAfter counts the tool outputs in input after index from.
func answeredAfter(input []map[string]any, from int) int {
	n := 0
	for _, it := range input[from:] {
		switch it["type"] {
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
			n++
		}
	}
	return n
}

type step struct {
	Call string          `json:"call"`
	Args json.RawMessage `json:"args"`
	Say  *string         `json:"say"`
}

// compaction says whether Codex's turn metadata header marks a compaction
// request, answered with a summary whatever script the history holds.
func compaction(header string) bool {
	var meta struct {
		Kind string `json:"request_kind"`
	}
	return json.Unmarshal([]byte(header), &meta) == nil && meta.Kind == "compaction"
}

// reply is the SSE events answering one request.
func reply(body []byte, compacting bool) ([]map[string]any, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("not a Responses request: %w", err)
	}
	usage := len(body) / 4 // a stable, rough token count
	var steps []step
	answered := 0
	cons, consolidating := consolidationRoot(req.Input)
	var err error
	if !compacting && req.Text.Format.Schema == nil {
		if consolidating {
			steps, answered = consolidationSteps(cons), answeredAfter(req.Input, 0)
		} else if steps, answered, err = script(req.Input); err != nil {
			return nil, err
		}
	}
	var item map[string]any
	switch {
	case compacting:
		item = message("summary: the conversation so far")
	case req.Text.Format.Schema != nil:
		item = message(structured(req.Text.Format.Schema))
	case steps == nil:
		item = message("ok")
	case answered >= len(steps):
		item = message("done")
	case steps[answered].Say != nil:
		item = message(*steps[answered].Say)
	case steps[answered].Call != "":
		item, err = call(allTools(req), steps[answered], fmt.Sprintf("call_fake_%d_%d", len(req.Input), answered), lastOutput(req.Input))
		if err != nil {
			item = message("fake model: " + err.Error())
		}
	default:
		return nil, fmt.Errorf("step %d has neither call nor say", answered)
	}
	id := fmt.Sprintf("resp_fake_%d", answered)
	return []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": id}},
		{"type": "response.output_item.done", "item": item},
		{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{
			"input_tokens": usage, "input_tokens_details": nil, "output_tokens": 1,
			"output_tokens_details": nil, "total_tokens": usage + 1,
		}}},
	}, nil
}

const open, closing = "<<fake ", ">>"

// script finds the last input message carrying a script and counts the tool
// calls answered after it.
func script(input []map[string]any) ([]step, int, error) {
	at := -1
	var steps []step
	for i, it := range input {
		if it["type"] != nil && it["type"] != "message" {
			continue
		}
		text := messageText(it)
		// The first block: a step may carry another script (a subagent's
		// task), which must not be taken for this one.
		start := strings.Index(text, open)
		if start < 0 {
			continue
		}
		rest := text[start+len(open):]
		dec := json.NewDecoder(strings.NewReader(rest))
		var s []step
		if err := dec.Decode(&s); err != nil {
			return nil, 0, fmt.Errorf("the <<fake block is not a JSON array of steps: %w", err)
		}
		if !strings.HasPrefix(strings.TrimLeft(rest[dec.InputOffset():], " \n"), closing) {
			return nil, 0, errors.New("a <<fake block is not closed by >>")
		}
		at, steps = i, s
	}
	if at < 0 {
		return nil, 0, nil
	}
	answered := 0
	for _, it := range input[at+1:] {
		switch it["type"] {
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
			answered++
		}
	}
	return steps, answered, nil
}

func messageText(it map[string]any) string {
	switch c := it["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			if p, ok := part.(map[string]any); ok {
				if t, ok := p["text"].(string); ok {
					b.WriteString(t)
					b.WriteByte('\n')
				}
			}
		}
		return b.String()
	}
	return ""
}

func message(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "assistant", "id": "msg_fake",
		"content": []map[string]any{{"type": "output_text", "text": text}},
	}
}

// tool is one callable tool of the request: its name, its namespace ("" at
// top level) and whether it takes free-form input (a custom tool).
type tool struct {
	name, namespace string
	custom          bool
}

func tools(list []map[string]any, namespace string) []tool {
	var out []tool
	for _, t := range list {
		typ, _ := t["type"].(string)
		name, _ := t["name"].(string)
		if nested, ok := t["tools"].([]any); ok {
			var inner []map[string]any
			for _, n := range nested {
				if m, ok := n.(map[string]any); ok {
					inner = append(inner, m)
				}
			}
			out = append(out, tools(inner, name)...)
			continue
		}
		if name != "" {
			out = append(out, tool{name: name, namespace: namespace, custom: typ == "custom"})
		}
	}
	return out
}

// resolve finds the tool a step names: an exact name first, then the only
// tool whose name ends with "_"+want or "__"+want.
func resolve(list []map[string]any, want string) (tool, error) {
	all := tools(list, "")
	for _, t := range all {
		if t.name == want {
			return t, nil
		}
	}
	var found []tool
	for _, t := range all {
		if strings.HasSuffix(t.name, "_"+want) || strings.HasSuffix(t.name, "__"+want) {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return tool{}, fmt.Errorf("no tool named %s in the request", want)
	}
	return tool{}, fmt.Errorf("%d tools match %s", len(found), want)
}

func call(list []map[string]any, s step, id, last string) (map[string]any, error) {
	t, err := resolve(list, s.Call)
	if err != nil {
		return nil, err
	}
	s.Args = fill(s.Args, last)
	item := map[string]any{"call_id": id, "name": t.name}
	if t.namespace != "" {
		item["namespace"] = t.namespace
	}
	if t.custom {
		var in string
		if err := json.Unmarshal(s.Args, &in); err != nil {
			return nil, fmt.Errorf("%s is a free-form tool: its args must be a JSON string", t.name)
		}
		item["type"], item["input"] = "custom_tool_call", in
		return item, nil
	}
	args := string(s.Args)
	if args == "" {
		args = "{}"
	}
	item["type"], item["arguments"] = "function_call", args
	return item, nil
}

var (
	placeholder = regexp.MustCompile(`\{\{last:([A-Za-z0-9_]+)\}\}`)
	matcher     = regexp.MustCompile(`\{\{match:(.+?)\}\}`)
)

// fill replaces each {{last:key}} in args by the value of "key" in the last
// tool output (a spawned agent's id, say), and each {{match:<regexp>}} by
// the regexp's first group in it (a memory id in a search's text; the
// regexp is JSON-escaped like the rest of args, and holds no "}}"), so a
// step can use what an earlier one returned. One that finds nothing stays
// as it is.
func fill(args json.RawMessage, last string) json.RawMessage {
	args = placeholder.ReplaceAllFunc(args, func(m []byte) []byte {
		key := placeholder.FindSubmatch(m)[1]
		v := regexp.MustCompile(`"` + string(key) + `"\s*:\s*"([^"]*)"`).FindStringSubmatch(last)
		if v == nil {
			return m
		}
		return []byte(v[1])
	})
	return matcher.ReplaceAllFunc(args, func(m []byte) []byte {
		var expr string
		if json.Unmarshal([]byte(`"`+string(matcher.FindSubmatch(m)[1])+`"`), &expr) != nil {
			return m
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return m
		}
		v := re.FindStringSubmatch(last)
		if len(v) < 2 {
			return m
		}
		quoted, _ := json.Marshal(v[1])
		return quoted[1 : len(quoted)-1]
	})
}

// lastOutput is the text of the last tool output in the input.
func lastOutput(input []map[string]any) string {
	for i := len(input) - 1; i >= 0; i-- {
		switch input[i]["type"] {
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
			if text, ok := input[i]["output"].(string); ok {
				return text
			}
			raw, _ := json.Marshal(input[i]["output"])
			return string(raw)
		}
	}
	return ""
}

// sse encodes events as a Server-Sent Events stream.
func sse(events []map[string]any) string {
	var b strings.Builder
	for _, ev := range events {
		raw, _ := json.Marshal(ev)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", ev["type"], raw)
	}
	return b.String()
}
