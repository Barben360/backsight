package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/livecheck"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// Seams replaced by tests: the command verify runs as Claude Code would
// (the shim), extra environment for it, and the round trip's patience.
var (
	verifyShim = func(d paths.Dirs) string { return filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS)) }
	verifyEnv  []string
	verifyPoll = 2 * time.Second
	verifyWait = 3 * time.Minute
)

type verifyCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // ok | fail | skipped
	Summary string `json:"summary"`
	MS      int    `json:"ms,omitempty"`
}

type verifyResult struct {
	Checks []verifyCheck `json:"checks"`
	Failed int           `json:"failed"`
}

func newVerifyCmd(e *env) *cobra.Command {
	var configDir string
	var noRoundTrip, live bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Run every hook and the MCP server as Claude Code would, then a retain/recall round trip in a scratch bank",
		Long: "Run every hook and the MCP server as Claude Code would, then a retain/recall round trip in a scratch bank.\n\n" +
			"With --live, check the real pipeline instead, read-only: this machine's latest settled session reached the\n" +
			"configured server and an observation was consolidated from it. Exit 0 pass, 40 pending (rerun after wait_s),\n" +
			"20 fail.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if live {
				if noRoundTrip {
					return &output.Error{Code: output.Usage, Message: "--live reads the real bank and makes no round trip: drop --no-round-trip"}
				}
				res, err := runVerifyLive(cmd.Context(), configDir)
				return e.report("verify", res, func() string { return liveText(res, liveResume(configDir)) }, err)
			}
			res, err := runVerify(cmd.Context(), configDir, noRoundTrip)
			if err == nil && res.Failed > 0 {
				err = shownError{&output.Error{Code: output.Blocked, Message: fmt.Sprintf("%d check(s) failed", res.Failed)}}
			}
			return e.report("verify", res, func() string { return verifyText(res) }, err)
		},
	}
	cmd.Flags().StringVar(&configDir, "config-dir", "", "Claude Code's directory the hooks run for (default: $CLAUDE_CONFIG_DIR, else ~/.claude)")
	cmd.Flags().BoolVar(&noRoundTrip, "no-round-trip", false, "skip the retain/recall round trip (no LLM call)")
	cmd.Flags().BoolVar(&live, "live", false, "check, read-only, that this machine's latest settled session was sent and consolidated")
	return cmd
}

func runVerify(ctx context.Context, configDir string, noRoundTrip bool) (verifyResult, error) {
	res := verifyResult{Checks: []verifyCheck{}}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return res, err
	}
	d, _, err := machine()
	if err != nil {
		return res, err
	}
	add := func(c verifyCheck) {
		res.Checks = append(res.Checks, c)
		if c.Status == "fail" {
			res.Failed++
		}
	}
	shim := verifyShim(d)
	if _, err := os.Stat(shim); err != nil {
		return res, &output.Error{Code: output.NeedsUser, Message: "no backsight shim at " + shim + ": Claude Code has nothing to run",
			Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight client install", Why: "installs the shim the plugin's hooks and MCP server run"}}}
	}
	sid := "backsight-verify-" + randomHex(6)
	scratch, err := os.MkdirTemp("", "backsight-verify-")
	if err != nil {
		return res, blocked(err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	// The hooks and the MCP server read the real config and secrets but
	// write a scratch state: verify must not use up the first-session
	// notice, add telemetry or leave a live session (decision 0064).
	work := filepath.Join(scratch, "cwd")
	sd := d
	sd.State = filepath.Join(scratch, "state")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return res, blocked(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLAUDE_ENV_FILE=") && !strings.HasPrefix(kv, "BACKSIGHT_STATE_DIR=") {
			env = append(env, kv)
		}
	}
	env = append(append(env, verifyEnv...), "BACKSIGHT_STATE_DIR="+sd.State)
	if configDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+configDir)
	}
	for _, h := range plugin.Hooks() {
		add(verifyHook(ctx, sd, shim, env, h, sid, work))
	}
	add(verifyMCP(ctx, shim, env, work))
	if noRoundTrip {
		add(verifyCheck{ID: "round_trip", Status: "skipped", Summary: "--no-round-trip"})
		return res, nil
	}
	add(roundTrip(ctx, cfg, sec))
	return res, nil
}

// syntheticInput is a synthetic event: a session of its own in an empty
// directory, a prompt too short to recall, a transcript that does not
// exist, so that no hook sends or stores anything.
func syntheticInput(h plugin.Hook, sid, dir string) []byte {
	in := map[string]any{"session_id": sid, "cwd": dir, "hook_event_name": h.Event,
		"transcript_path": filepath.Join(dir, sid+".jsonl"), "permission_mode": "default"}
	switch h.Event {
	case "SessionStart":
		in["source"] = "startup"
	case "UserPromptSubmit":
		in["prompt"] = "backsight verify"
	case "PreToolUse":
		in["tool_name"], in["tool_input"] = "AskUserQuestion", map[string]any{"questions": []any{}}
	case "PostToolUse":
		in["tool_name"], in["tool_input"], in["tool_response"] = plugin.ToolNames[0], map[string]any{"query": "verify"}, map[string]any{}
	case "PostToolUseFailure":
		in["tool_name"], in["tool_input"], in["error"] = "Bash", map[string]any{"command": "backsight verify"}, "Exit code 1\nverify"
	case "SubagentStart", "SubagentStop":
		in["agent_id"], in["agent_type"] = "verify", "general-purpose"
	case "SessionEnd":
		in["reason"] = "other"
	}
	b, _ := json.Marshal(in)
	return b
}

func verifyHook(ctx context.Context, d paths.Dirs, shim string, env []string, h plugin.Hook, sid, dir string) verifyCheck {
	c := verifyCheck{ID: "hook " + h.Arg}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(h.Timeout)*time.Second+2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shim, "hook", h.Arg)
	cmd.Env, cmd.Dir, cmd.Stdin = env, dir, bytes.NewReader(syntheticInput(h, sid, dir))
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	start := time.Now()
	err := cmd.Run()
	c.MS = int(time.Since(start).Milliseconds())
	trimmed := bytes.TrimSpace(out.Bytes())
	switch {
	case err != nil:
		c.Status, c.Summary = "fail", fmt.Sprintf("exit: %v %s", err, strings.TrimSpace(errOut.String()))
	case len(trimmed) > 0 && !json.Valid(trimmed):
		c.Status, c.Summary = "fail", "output is not JSON"
	case hookFailed(d, sid, h.Arg):
		// The supervisor always exits 0; a crashed or late child shows in telemetry.
		c.Status, c.Summary = "fail", "the hook failed behind the supervisor (see backsight doctor)"
	default:
		c.Status, c.Summary = "ok", fmt.Sprintf("%s answered in %d ms", h.Event, c.MS)
	}
	return c
}

// hookFailed looks for the supervisor's hook_failure event of this run.
func hookFailed(d paths.Dirs, sid, arg string) bool {
	files, _ := filepath.Glob(filepath.Join(d.TelemetryDir(), "events-*.jsonl"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if bytes.Contains(line, []byte(`"hook_failure"`)) && bytes.Contains(line, []byte(`"`+sid+`"`)) && bytes.Contains(line, []byte(`"`+arg+`"`)) {
				return true
			}
		}
	}
	return false
}

func verifyMCP(ctx context.Context, shim string, env []string, dir string) verifyCheck {
	c := verifyCheck{ID: "mcp"}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shim, "mcp")
	cmd.Env, cmd.Dir = env, dir
	start := time.Now()
	client := mcp.NewClient(&mcp.Implementation{Name: "backsight-verify", Version: buildinfo.Version}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		c.Status, c.Summary = "fail", "no MCP handshake: "+err.Error()
		return c
	}
	defer func() { _ = sess.Close() }()
	list, err := sess.ListTools(ctx, nil)
	c.MS = int(time.Since(start).Milliseconds())
	if err != nil {
		c.Status, c.Summary = "fail", "tools/list: "+err.Error()
		return c
	}
	var got []string
	for _, t := range list.Tools {
		got = append(got, t.Name)
	}
	for _, want := range plugin.ToolNames {
		short := want[strings.LastIndex(want, "__")+2:]
		if !slices.Contains(got, short) {
			c.Status, c.Summary = "fail", fmt.Sprintf("tool %s missing (server lists %v)", short, got)
			return c
		}
	}
	c.Status, c.Summary = "ok", fmt.Sprintf("handshake and %d tools in %d ms", len(got), c.MS)
	return c
}

// verifyDoc is the round trip's document id inside the scratch bank.
const verifyDoc = "backsight-verify"

// roundTrip retains one document in a scratch bank, recalls it and
// deletes the bank (decision 0064): the user's bank is never written.
func roundTrip(ctx context.Context, cfg config.Config, sec config.Secrets) verifyCheck {
	c := verifyCheck{ID: "round_trip"}
	fail := func(format string, a ...any) verifyCheck {
		c.Status, c.Summary = "fail", server.Redact(fmt.Sprintf(format, a...), sec) // provider errors may echo a key
		return c
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err != nil {
		return fail("%v", err)
	}
	bank := "backsight-verify-" + randomHex(4)
	start := time.Now()
	if err := cl.CreateBank(ctx, bank, "backsight verify (scratch)", "A scratch bank backsight verify deletes right away."); err != nil {
		return fail("cannot create the scratch bank: %v", err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = cl.DeleteBank(dctx, bank)
	}()
	marker := "lantern-" + randomHex(2)
	now := time.Now().UTC()
	item := hindsight.RetainItem{DocumentID: verifyDoc, Tags: []string{"source:verify"}, Timestamp: &now,
		Content: "During setup the user chose the verification codeword " + marker + " for the backsight memory install on this machine."}
	if err := cl.Retain(ctx, bank, []hindsight.RetainItem{item}, ""); err != nil {
		return fail("retain: %v", err)
	}
	deadline := time.Now().Add(verifyWait)
	for {
		doc, err := cl.GetDocument(ctx, bank, verifyDoc)
		if err == nil && doc.MemoryUnits > 0 {
			break
		}
		if time.Now().After(deadline) {
			return fail("no fact extracted within %s: check backsight server llm check", verifyWait)
		}
		select {
		case <-ctx.Done():
			return fail("%v", ctx.Err())
		case <-time.After(verifyPoll):
		}
	}
	hits, err := cl.Recall(ctx, bank, hindsight.RecallRequest{Query: "Which verification codeword did the user choose during setup?", Budget: "low"})
	if err != nil {
		return fail("recall: %v", err)
	}
	if len(hits) == 0 {
		return fail("recall found nothing in the scratch bank")
	}
	c.MS = int(time.Since(start).Milliseconds())
	c.Status, c.Summary = "ok", fmt.Sprintf("retained, extracted and recalled in %.1f s (scratch bank deleted)", time.Since(start).Seconds())
	return c
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func verifyText(r verifyResult) string {
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-7s %-24s %s\n", c.Status, c.ID, c.Summary)
	}
	return b.String()
}

// liveServer is the configured server as verify --live reads it; tests
// replace it.
var liveServer = func(url, key string) (livecheck.Server, error) { return hindsight.New(url, key, 30*time.Second) }

// runVerifyLive is verify --live (0068): read-only against the configured
// server. Pending exits 40 with the rerun as its resume, fail exits 20.
func runVerifyLive(ctx context.Context, configDir string) (livecheck.Result, error) {
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return livecheck.Result{}, err
	}
	d, home, err := machine()
	if err != nil {
		return livecheck.Result{}, err
	}
	srv, err := liveServer(cfg.Server.URL, sec.APIKey)
	if err != nil {
		return livecheck.Result{}, blocked(err)
	}
	files := transcripts(home)
	if configDir != "" {
		loc := claudecode.Locate(home, func(k string) string {
			if k == "CLAUDE_CONFIG_DIR" {
				return configDir
			}
			return os.Getenv(k)
		})
		files, _ = filepath.Glob(filepath.Join(loc.Projects, "*", "*.jsonl"))
	}
	in := livecheck.Input{Server: srv, Bank: cfg.Server.Bank, Transcripts: files, CursorsDir: d.CursorsDir(), Now: now()}
	if st, err := state.Load(d.StateFile()); err == nil && st.RetainSince != nil {
		in.RetainSince = *st.RetainSince
	}
	res := livecheck.Check(ctx, in)
	switch res.Status {
	case livecheck.Pending:
		return res, shownError{&output.Error{Code: output.Partial, Message: res.Summary, Resume: liveResume(configDir)}}
	case livecheck.Fail:
		return res, shownError{&output.Error{Code: output.Blocked, Message: res.Summary}}
	}
	return res, nil
}

// liveResume is the rerun of verify --live with the flags it was given.
func liveResume(configDir string) string {
	if configDir == "" {
		return "backsight verify --live"
	}
	return "backsight verify --live --config-dir " + shellQuote(configDir)
}

// liveText is the check's verdict; nothing when the check did not run
// (no config: the error on stderr says why).
func liveText(r livecheck.Result, resume string) string {
	if r.Status == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", r.Status, r.Summary)
	if r.Detail != "" {
		fmt.Fprintf(&b, "  %s\n", r.Detail)
	}
	if r.Fix != "" {
		fmt.Fprintf(&b, "  fix: %s\n", r.Fix)
	}
	if r.Status == livecheck.Pending && r.WaitS > 0 {
		fmt.Fprintf(&b, "  rerun in about %d min: %s\n", (r.WaitS+59)/60, resume)
	}
	return b.String()
}
