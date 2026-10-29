package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

type searchArgs struct {
	Queries  []string `json:"queries" jsonschema:"1-4 short keyword queries, one per subject"`
	Projects []string `json:"projects,omitempty" jsonschema:"other projects to search too"`
	Scope    string   `json:"scope,omitempty" jsonschema:"auto (current project, transverse, related projects and the whole memory; default) or all (the whole memory only)"`
	Limit    int      `json:"limit,omitempty" jsonschema:"most memories returned (default 8)"`
	MinScore *float64 `json:"min_score,omitempty" jsonschema:"relevance floor (default 0.4); lower it to widen"`
}

type readArgs struct {
	ID string `json:"id" jsonschema:"the memory id shown by memory_search, or the short id (first 8 characters) of a memory shown in this session"`
}

type storeArgs struct {
	Kind    string `json:"kind" jsonschema:"decision, correction, convention, pitfall, state or lesson"`
	Text    string `json:"text" jsonschema:"1-3 sentences: the fact, why, absolute dates"`
	Project string `json:"project,omitempty" jsonschema:"default: the current project; \"transverse\" only for a user-wide rule"`
}

type creditArgs struct {
	IDs        []string `json:"ids,omitempty" jsonschema:"1-10 ids of memories shown in this session, full or their first 8 characters; none with verdict none"`
	Verdict    string   `json:"verdict,omitempty" jsonschema:"used (default): these memories informed your answer; none: no memory shown informed it; wrong: this session shows them false; outdated: they were true but no longer are"`
	Correction string   `json:"correction,omitempty" jsonschema:"with wrong or outdated: the right fact, 1-3 sentences, as for memory_store"`
	Evidence   string   `json:"evidence,omitempty" jsonschema:"with wrong or outdated: what in this session shows it (the user's words, a file, a command output), at most 300 characters"`
}

// live is the session and project of the Claude Code session this server
// belongs to, from the record its hooks keep under its parent pid
// (decision 0056), and how it was found: "record", "stale" (written before
// the parent started: a reused pid) or "none". Without a current record,
// the start directory and no session.
func (d mcpDeps) live() (session.Live, string) {
	found := "none"
	if d.parentPID > 1 {
		if l, ok := session.ReadLive(d.dirs.SessionsDir(), d.parentPID); ok {
			if d.parentStart.IsZero() || !l.At.Before(d.parentStart) {
				return l, "record"
			}
			found = "stale"
		}
	}
	cwd, _ := os.Getwd()
	return session.Live{Cwd: cwd, Project: d.current}, found
}

// freshPrivacy is the server's config with the privacy and components
// sections read again: a pause --project or an opt-out taken during the
// session applies to the next store, not to the next session. An
// unreadable config keeps the one the server started with.
func (d mcpDeps) freshPrivacy() config.Config {
	cfg := d.cfg
	if fresh, _, err := loadServerConfig(); err == nil {
		cfg.Privacy, cfg.Components = fresh.Privacy, fresh.Components
	}
	return cfg
}

// mcpKey holds what carries the API key for the server's whole life. A
// `server token --rotate` on this host gives secrets.toml a new key (hooks
// and jobs read it on each run): a tool call the server refuses reads it
// again and, when it changed, is tried once more with it.
type mcpKey struct {
	mu     sync.Mutex
	file   string // secrets.toml
	cfg    config.Config
	sec    config.Secrets
	engine *recall.Engine
}

func (k *mcpKey) get() (config.Secrets, *recall.Engine) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.sec, k.engine
}

// refresh follows secrets.toml after the server refused used, the key of
// the refused call, and says whether to try the call again: the key
// changed, now or in a concurrent call. An unchanged key: no retry.
func (k *mcpKey) refresh(used string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.sec.APIKey != used {
		return true
	}
	sec, err := config.LoadSecrets(k.file)
	if err != nil || sec.APIKey == "" || sec.APIKey == used {
		return false
	}
	eng, err := newEngine(k.cfg, sec)
	if err != nil {
		return false
	}
	k.sec, k.engine = sec, eng
	return true
}

func unauthorized(err error) bool {
	var apiErr *hindsight.APIError
	return errors.As(err, &apiErr) && apiErr.Kind == hindsight.Unauthorized
}

// refused: every scope of r that failed was refused the key, and one did.
func refused(r recall.Result) bool {
	seen := false
	for _, m := range r.Scopes {
		if !m.OK && !m.Unauthorized {
			return false
		}
		seen = seen || m.Unauthorized
	}
	return seen
}

// mcpDeps is what the tools need, resolved once per server; sec and
// engine are the start's, which mcpKey follows.
type mcpDeps struct {
	dirs        paths.Dirs
	home        string
	cfg         config.Config
	sec         config.Secrets
	engine      *recall.Engine
	current     string
	tw          *telemetry.Writer
	parentPID   int       // the Claude Code process that started this server
	parentStart time.Time // when it started; zero when the system cannot tell
	generation  int       // the sends generation, read before cfg (sendsGeneration)
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errorResult(s string) *mcp.CallToolResult {
	r := textResult(s)
	r.IsError = true
	return r
}

// newMCPServer builds the memory server (SPEC §7).
func newMCPServer(deps mcpDeps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "memory", Version: buildinfo.Version}, &mcp.ServerOptions{Instructions: instructions.For(deps.cfg.Feedback.Enabled).ShortGuide})
	key := &mcpKey{file: deps.dirs.SecretsFile(), cfg: deps.cfg, sec: deps.sec, engine: deps.engine}
	mcp.AddTool(s, &mcp.Tool{Name: "memory_search", Description: instructions.SearchDescription, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, a searchArgs) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			if !deps.cfg.Components.Read {
				return errorResult("components.read is off in config.toml: search with the other integration"), nil, nil
			}
			if len(a.Queries) == 0 || len(a.Queries) > maxQueries {
				return errorResult("queries: give 1 to 4 short keyword queries"), nil, nil
			}
			if a.Scope != "" && a.Scope != "auto" && a.Scope != "all" {
				return errorResult(`scope: "auto" or "all"`), nil, nil
			}
			limit, minScore := a.Limit, searchMinScore
			if limit <= 0 {
				limit = searchLimit
			}
			if a.MinScore != nil {
				minScore = *a.MinScore
			}
			live, found := deps.live()
			sec, eng := key.get()
			res := searchMemory(ctx, deps.dirs, deps.home, deps.cfg, eng, live.Project, live.Cwd, a.Queries, a.Projects, a.Scope == "all", limit, minScore)
			if refused(res) && key.refresh(sec.APIKey) {
				_, eng = key.get()
				res = searchMemory(ctx, deps.dirs, deps.home, deps.cfg, eng, live.Project, live.Cwd, a.Queries, a.Projects, a.Scope == "all", limit, minScore)
			}
			deps.tw.Emit(telemetry.Tool{Name: "memory_search", MS: int(time.Since(start).Milliseconds()), Hits: len(res.Hits), OK: scopesOK(res), Live: found})
			if len(res.Hits) == 0 && !scopesOK(res) {
				return errorResult("memory unavailable: " + scopeErrors(res)), nil, nil
			}
			return textResult(mcpSearchText(res, deps.cfg.Feedback.Enabled)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "memory_read", Description: instructions.ReadDescription, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, a readArgs) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			if !deps.cfg.Components.Read {
				return errorResult("components.read is off in config.toml"), nil, nil
			}
			id := strings.TrimSpace(a.ID)
			if !memoryIDRE.MatchString(id) || strings.Contains(id, "..") {
				return errorResult("id: the memory id shown by memory_search"), nil, nil
			}
			id, err := deps.readID(id)
			if err != nil {
				return errorResult(err.Error()), nil, nil
			}
			sec, eng := key.get()
			m, err := eng.Client.GetMemory(ctx, deps.cfg.Server.Bank, id)
			if unauthorized(err) && key.refresh(sec.APIKey) {
				_, eng = key.get()
				m, err = eng.Client.GetMemory(ctx, deps.cfg.Server.Bank, id)
			}
			deps.tw.Emit(telemetry.Tool{Name: "memory_read", MS: int(time.Since(start).Milliseconds()), Hits: map[bool]int{true: 1}[err == nil], OK: err == nil})
			var apiErr *hindsight.APIError
			if errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound {
				return errorResult("no memory " + a.ID), nil, nil
			}
			if err != nil {
				return errorResult("memory unavailable: " + err.Error()), nil, nil
			}
			return textResult(readText(m)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "memory_store", Description: instructions.StoreDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, a storeArgs) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			// Refusals first: nothing leaves the machine for a refused note.
			live, found := deps.live()
			cfg := deps.freshPrivacy()
			err := checkNote(deps.dirs, deps.home, cfg, live.SessionID, live.Cwd, a.Kind, a.Project, a.Text)
			if err == nil && found != "record" && session.AnyPaused(deps.dirs.SessionsDir(), 48*time.Hour) {
				// Without its session this server cannot honour a pause:
				// while one is on, it fails closed.
				err = errors.New("not stored: a session is paused and this server cannot tell which session it belongs to (no current hook record); resume it, or store from the shell with backsight store")
			}
			var proj string
			if err == nil {
				proj, err = noteTarget(deps.cfg, a.Project, live.Project, `project "transverse"`)
			}
			if err != nil {
				deps.tw.Emit(telemetry.Tool{Name: "memory_store", MS: int(time.Since(start).Milliseconds()), Live: found})
				return errorResult(err.Error()), nil, nil
			}
			// What this fact updates or repeats, before it is stored (SPEC §7).
			// A refused key is followed here, before the note is queued: the
			// store itself is never sent twice.
			sec, eng := key.get()
			q := recall.Query{Queries: []string{a.Text}, Current: proj, MinScore: searchMinScore, Limit: 3}
			similar := eng.Search(ctx, q)
			if refused(similar) && key.refresh(sec.APIKey) {
				sec, eng = key.get()
				similar = eng.Search(ctx, q)
			}
			res, err := storeNote(ctx, deps.dirs, deps.home, cfg, sec, deps.generation, live.SessionID, live.Cwd, a.Kind, proj, a.Text)
			deps.tw.Emit(telemetry.Tool{Name: "memory_store", MS: int(time.Since(start).Milliseconds()), Hits: len(similar.Hits), OK: err == nil, Live: found})
			if res.Document == "" {
				return errorResult(err.Error()), nil, nil
			}
			var b strings.Builder
			b.WriteString(storeText(res))
			if err != nil {
				fmt.Fprintf(&b, "%s\n", err)
			}
			if len(similar.Hits) > 0 {
				b.WriteString("Most similar memories (does this fact update or repeat one?):\n")
				for _, h := range similar.Hits {
					fmt.Fprintf(&b, "- [%s] %s (%s ago, %s)\n", h.Project, h.Text, h.Age, h.ID)
				}
			}
			return textResult(b.String()), nil, nil
		})
	if deps.cfg.Feedback.Enabled {
		mcp.AddTool(s, &mcp.Tool{Name: "memory_credit", Description: instructions.CreditDescription},
			func(ctx context.Context, _ *mcp.CallToolRequest, a creditArgs) (*mcp.CallToolResult, any, error) {
				verdict := a.Verdict
				if verdict == "" {
					verdict = "used"
				}
				text, err := deps.credit(ctx, key, verdict, a)
				if !slices.Contains([]string{"used", "none", "wrong", "outdated"}, verdict) {
					verdict = "invalid" // telemetry holds enums, never what the agent typed
				}
				deps.tw.Emit(telemetry.Credit{Verdict: verdict, IDs: len(a.IDs), OK: err == nil})
				if err != nil {
					return errorResult(err.Error()), nil, nil
				}
				return textResult(text), nil, nil
			})
	}
	return s
}

// memory_credit's accepted answers start with these: the PostToolUse hook
// journals a credit only behind one of them.
const (
	creditUsedPrefix = "Credited."
	creditNonePrefix = "Noted:"
	maxCreditIDs     = 10
	maxEvidence      = 300 // characters
)

// credit is memory_credit's answer: for used, the line to print, once
// every id resolves to one shown in this session (all or nothing); for
// none, a note; for wrong and outdated, the correction stored and the
// memories retired (correct). It journals no credit: the PostToolUse hook
// does, with the turn and the agent only hooks receive.
func (d mcpDeps) credit(ctx context.Context, key *mcpKey, verdict string, a creditArgs) (string, error) {
	refs := a.IDs
	switch verdict {
	case "used":
		if len(refs) == 0 || len(refs) > maxCreditIDs {
			return "", errors.New("ids: give 1 to 10 ids of memories shown in this session, or verdict none")
		}
	case "none":
		if len(refs) > 0 {
			return "", errors.New(`verdict none takes no ids: drop them, or credit them with verdict "used"`)
		}
	case "wrong", "outdated":
		if len(refs) == 0 || len(refs) > maxCreditIDs {
			return "", fmt.Errorf("ids: give 1 to 10 ids of memories shown in this session that are %s", verdict)
		}
		if strings.TrimSpace(a.Correction) == "" {
			return "", fmt.Errorf("correction: give the right fact, 1-3 sentences, with verdict %s", verdict)
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(a.Evidence)); n == 0 || n > maxEvidence {
			return "", fmt.Errorf("evidence: give what in this session shows it, 1 to %d characters (the user's words, a file, a command output)", maxEvidence)
		}
	default:
		return "", errors.New(`verdict: "used", "none", "wrong" or "outdated"`)
	}
	live, found := d.live()
	if found != "record" {
		return "", errors.New("memory session not ready: call memory_credit again")
	}
	if verdict == "none" {
		return creditNonePrefix + " no memory used.", nil
	}
	shown := shownInSession(d.dirs, live.SessionID)
	var ids, problems []string
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		id, err := feedback.Resolve(ref, shown)
		switch {
		case errors.Is(err, feedback.ErrUnknown):
			problems = append(problems, ref+": not shown in this session, or its search result has not arrived yet: call again after the result")
		case err != nil:
			problems = append(problems, ref+": "+err.Error()+": give more of its id")
		case !slices.Contains(ids, id):
			ids = append(ids, id)
		}
	}
	if len(problems) > 0 {
		return "", errors.New(strings.Join(problems, "\n"))
	}
	owner := shownProjects(shown, live.Project)
	if verdict == "wrong" || verdict == "outdated" {
		return d.correct(ctx, key, live, verdict, a, ids, owner)
	}
	// One line per project, in the order the ids were given.
	var order []string
	groups := map[string][]string{}
	for _, id := range ids {
		p := safeProject(owner(id))
		if groups[p] == nil {
			order = append(order, p)
		}
		groups[p] = append(groups[p], shortID(id))
	}
	lines := make([]string, 0, len(order))
	for _, p := range order {
		lines = append(lines, instructions.CreditLine(p, groups[p]))
	}
	what := "this line"
	if len(lines) > 1 {
		what = "these lines"
	}
	return creditUsedPrefix + " Print " + what + " in your final message:\n" + strings.Join(lines, "\n"), nil
}

// shownProjects is the project of each memory shown: the one its latest
// show names; a read names none (its show carries the session's), so it
// only fills in for an id that no other show named; else current.
func shownProjects(shown []feedback.Show, current string) func(id string) string {
	of := map[string]string{}
	for _, s := range shown {
		if s.Path != "read" {
			of[s.ID] = s.Project
		}
	}
	for _, s := range shown {
		if _, ok := of[s.ID]; !ok {
			of[s.ID] = s.Project
		}
	}
	return func(id string) string {
		if p := of[id]; p != "" {
			return p
		}
		return current
	}
}

// correct answers a wrong or outdated verdict on ids, each shown in this
// session: every guard first (each memory's project allowed, the
// correction and the evidence through the note checks, the session's
// retirements under feedback.max_retirements, counting what will be
// retired or hidden), then the correction stored as a note naming the
// memories it replaces, then the retirement. An observation none of whose
// facts is named stops the call before anything is stored: its facts are
// listed for a second call. The correction and the evidence go into the
// note and the invalidation reason only, never the journal or telemetry.
func (d mcpDeps) correct(ctx context.Context, key *mcpKey, live session.Live, verdict string, a creditArgs, ids []string, owner func(string) string) (string, error) {
	cfg := d.freshPrivacy()
	// Allowed: the session's project, the transverse pool and the projects
	// it links (as memory_search scopes them); another project's memory is
	// the user's to retire.
	linked := linkedProjects(d.dirs, cfg, project.New(d.home, cfg.Projects.Transverse), live.Cwd, live.Project)
	var problems []string
	pool := true
	for _, id := range ids {
		p := owner(id)
		pool = pool && isPool(cfg, p)
		if p != live.Project && !isPool(cfg, p) && !slices.Contains(linked, p) {
			problems = append(problems, fmt.Sprintf("%s: a memory of project %s, outside this session's project and the projects it links: nothing stored; the user can retire it from a terminal: backsight forget %s",
				shortID(id), safeProject(p), id))
		}
	}
	if len(problems) > 0 {
		return "", errors.New(strings.Join(problems, "\n"))
	}
	// The note goes to the session's project, or to the pool when every
	// memory it replaces is transverse.
	proj := live.Project
	if pool {
		proj = owner(ids[0])
	}
	correction, evidence := strings.TrimSpace(a.Correction), singleLine(a.Evidence)
	if err := checkNote(d.dirs, d.home, cfg, live.SessionID, live.Cwd, "correction", proj, correction); err != nil {
		return "", err
	}
	if _, kinds := redact.Text(evidence); len(kinds) > 0 {
		return "", errors.New("the evidence holds a secret (" + strings.Join(kinds, ", ") + "): remove it, secrets never go to memory")
	}
	// Retiring writes to the server: not during maintenance (a restore
	// would undo it silently), never to another server than the joined one.
	if held := state.Inspect(d.dirs.MaintenanceLock()); held != nil {
		return "", heldRefusal(held)
	}
	if err := otherServerRefusal(d.dirs, cfg); err != nil {
		return "", err
	}
	// Every call retires at least one memory: a session at its limit is
	// refused before the server is asked anything.
	done, limit := retirementsInSession(d.dirs, live.SessionID), cfg.Feedback.MaxRetirements
	if done >= limit {
		return "", capRefusal(limit, done, ids)
	}
	plan := d.classify(ctx, key, ids, owner)
	if len(plan.listed) > 0 {
		d.tw.Emit(telemetry.Retire{Action: "sources", IDs: len(plan.listed)})
		return sourcesText(plan), nil
	}
	if acted := plan.acted(); done+len(acted) > limit {
		return "", capRefusal(limit, done, acted)
	}
	sec, _ := key.get()
	res, err := storeNote(ctx, d.dirs, d.home, cfg, sec, d.generation, live.SessionID, live.Cwd, "correction", proj, correction, noteOptions{Replaces: ids, Evidence: evidence})
	if err != nil {
		return "", err
	}
	return d.retire(ctx, key, live, verdict, res.Document, invalidationReason(verdict, evidence, correction, res.Document), ids, plan)
}

// retirementsInSession is how many memories session sess retired, from
// the journal's last 62 days; an unreadable journal counts what it could
// read (a journal failure never fails a tool call).
func retirementsInSession(d paths.Dirs, sess string) int {
	events, _, _ := feedback.Read(d.FeedbackDir(), now().AddDate(0, 0, -62))
	return feedback.Retirements(events, sess)
}

// readID is the memory id memory_read's ref names: the id of a memory
// shown in this session that the ref starts with (8 characters or more),
// else the ref itself (a full id needs no show).
func (d mcpDeps) readID(ref string) (string, error) {
	live, found := d.live()
	if found != "record" || fullUUIDRE.MatchString(ref) {
		return ref, nil
	}
	id, err := feedback.Resolve(ref, shownInSession(d.dirs, live.SessionID))
	switch {
	case errors.Is(err, feedback.ErrAmbiguous):
		return "", errors.New(ref + ": " + err.Error() + ": give more of its id")
	case err != nil:
		return ref, nil
	}
	return id, nil
}

// fullUUIDRE: a whole id, which needs no lookup among the shows.
var fullUUIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// memoryIDRE: what Hindsight ids look like (UUIDs); nothing that could
// turn into another path.
var memoryIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

func scopesOK(r recall.Result) bool {
	for _, m := range r.Scopes {
		if m.OK {
			return true
		}
	}
	return len(r.Scopes) == 0
}

func scopeErrors(r recall.Result) string {
	var errs []string
	for name, m := range r.Scopes {
		if !m.OK {
			errs = append(errs, name+": "+m.Err)
		}
	}
	return strings.Join(errs, "; ")
}

// mcpSearchText is searchText with the ask to declare what the agent uses
// (to cite it, when memory_credit is not offered).
func mcpSearchText(r recall.Result, feedback bool) string {
	if len(r.Hits) == 0 {
		return "nothing in memory\n"
	}
	return searchText(r) + "\n" + instructions.For(feedback).SearchEnd + "\n"
}

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp",
		Short:  "Serve the memory tools to Claude Code over stdio (started by the plugin)",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			generation := sendsGeneration(d) // before the config it goes with
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg, sec)
			if err != nil {
				return err
			}
			current := currentProject(home, cfg)
			tw := telemetry.New(d.State, telemetry.Common{V: instructions.Version, Project: current, BinaryVersion: buildinfo.Version})
			ppid := os.Getppid()
			pstart, _ := session.ProcessStart(ppid)
			s := newMCPServer(mcpDeps{dirs: d, home: home, cfg: cfg, sec: sec, engine: eng, current: current, tw: tw, parentPID: ppid, parentStart: pstart, generation: generation})
			// Claude Code ends a session by closing stdin: a clean exit.
			if err := s.Run(cmd.Context(), &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
				return &output.Error{Code: output.Blocked, Message: err.Error()}
			}
			return nil
		},
	}
}
