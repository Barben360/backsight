package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

type codexRequestKey struct{}

type codexCreditArgs struct {
	IDs     []string `json:"ids,omitempty" jsonschema:"1-10 memory ids, full or their first 8 characters; none with verdict none"`
	Verdict string   `json:"verdict,omitempty" jsonschema:"used (default) or none"`
}

type codexRequest struct {
	live       session.Live
	turn, call string
	items      []feedback.Item
}

func (d mcpDeps) withRequest(ctx context.Context) mcpDeps {
	if r, ok := ctx.Value(codexRequestKey{}).(*codexRequest); ok {
		d.request = r
	}
	return d
}

// L'identité vient du transport, jamais des arguments proposés par le modèle.
func (d mcpDeps) codexIdentity(meta map[string]any) (*codexRequest, error) {
	thread, _ := meta["threadId"].(string)
	call, _ := meta["callId"].(string)
	turnMeta, _ := meta["x-codex-turn-metadata"].(map[string]any)
	turn, _ := turnMeta["turn_id"].(string)
	other, otherOK := turnMeta["thread_id"].(string)
	_, otherPresent := turnMeta["thread_id"]
	if !session.ValidID(thread) || len(thread) > 120 || !session.ValidID(turn) || !session.ValidID(call) || (otherPresent && (!otherOK || other != thread)) {
		return nil, errors.New("memory attribution unavailable: missing or inconsistent threadId, callId or turn_id in MCP metadata")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, errors.New("memory attribution unavailable: cannot resolve working directory")
	}
	return &codexRequest{live: session.Live{SessionID: "codex-" + thread, Cwd: cwd, Project: d.current}, turn: turn, call: call}, nil
}

// Chaque appel garde son propre contexte, même sur une connexion partagée.
func (d mcpDeps) codexMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		p, ok := req.GetParams().(*mcp.CallToolParamsRaw)
		if !ok {
			return errorResult("memory attribution unavailable: unsupported MCP request"), nil
		}
		if p.Name != "memory_search" && p.Name != "memory_read" && p.Name != "memory_credit" {
			return errorResult("this client supports memory search, read and credits only"), nil
		}
		var credit creditArgs
		if p.Name == "memory_credit" {
			if err := json.Unmarshal(p.Arguments, &credit); err != nil {
				return errorResult("invalid credit arguments"), nil
			}
			if credit.Verdict != "" && credit.Verdict != "used" && credit.Verdict != "none" {
				return errorResult("this client accepts used or none credits only; corrections are not enabled"), nil
			}
		}
		r, err := d.codexIdentity(p.GetMeta())
		if err != nil {
			return errorResult(err.Error()), nil
		}
		if paused, _ := session.Paused(d.dirs.SessionsDir(), r.live.SessionID); paused {
			return errorResult("memory attribution paused for this session"), nil
		}
		// Le verrou empêche les crédits concurrents de compter deux fois.
		lockPath := filepath.Join(d.dirs.State, "locks", fmt.Sprintf("mcp-credit-%x.lock", sha256.Sum256([]byte(r.live.SessionID))))
		lock, _, err := state.Acquire(lockPath, "memory attribution")
		if err != nil {
			return errorResult("memory attribution busy or unavailable; retry the call"), nil
		}
		defer func() { _ = lock.Release() }()
		events, err := sessionJournal(d.dirs, r.live.SessionID)
		if err != nil {
			return errorResult("memory attribution unavailable: cannot read the journal"), nil
		}
		ctx = context.WithValue(ctx, codexRequestKey{}, r)
		result, err := next(ctx, method, req)
		if err != nil {
			return result, err
		}
		out, ok := result.(*mcp.CallToolResult)
		if !ok || out.IsError || !d.cfg.Feedback.Enabled {
			if p.Name == "memory_credit" && d.cfg.Feedback.Enabled {
				d.codexCreditTelemetry(credit, false)
			}
			return result, nil
		}
		recorded, err := d.recordCodex(r, p.Name, credit, events)
		if p.Name == "memory_credit" && (recorded || err != nil) {
			d.codexCreditTelemetry(credit, err == nil)
		}
		if err != nil {
			return errorResult(err.Error()), nil
		}
		return result, nil
	}
}

func (d mcpDeps) codexCreditTelemetry(a creditArgs, ok bool) {
	verdict := a.Verdict
	if verdict == "" {
		verdict = "used"
	}
	d.tw.Emit(telemetry.Credit{Verdict: verdict, IDs: len(a.IDs), OK: ok})
}

func (d mcpDeps) recordCodex(r *codexRequest, tool string, a creditArgs, events []feedback.Event) (bool, error) {
	e := feedback.Event{Turn: r.turn, Call: r.call, Source: "codex"}
	if tool == "memory_credit" {
		e.Kind, e.Verdict = "credit", a.Verdict
		if e.Verdict == "" {
			e.Verdict = "used"
		}
		shown := feedback.Shows(events, r.live.SessionID)
		for _, ref := range a.IDs {
			id, err := feedback.Resolve(strings.TrimSpace(ref), shown)
			if err != nil {
				return false, errors.New("memory credit not recorded: memory was not shown in this session")
			}
			if !slices.Contains(e.IDs, id) {
				e.IDs = append(e.IDs, id)
			}
		}
		slices.Sort(e.IDs)
	} else {
		e.Kind, e.Items = "shows", r.items
		e.Path = strings.TrimPrefix(tool, "memory_")
	}
	var turnSeen bool
	for _, old := range events {
		if old.Session != r.live.SessionID {
			continue
		}
		turnSeen = turnSeen || (old.Kind == "turn" && old.Turn == r.turn)
		if old.Kind == "credit" && e.Kind == "credit" && old.Turn == e.Turn {
			ids := slices.Clone(old.IDs)
			slices.Sort(ids)
			if old.Verdict == e.Verdict && slices.Equal(ids, e.IDs) {
				return false, nil
			}
			return false, errors.New("memory credit not recorded: this turn already has a different verdict")
		}
		if old.Call == e.Call && old.Call != "" {
			return false, errors.New("memory attribution refused: tool call id already recorded; use a new call id")
		}
	}
	j := feedback.NewJournal(d.dirs.FeedbackDir(), feedback.Common{Session: r.live.SessionID, Project: r.live.Project, Binary: buildinfo.Version, V: instructions.Version})
	if !turnSeen {
		if err := j.Append(feedback.Event{Kind: "turn", Turn: r.turn, Source: "codex"}); err != nil {
			return false, errors.New("memory attribution unavailable: cannot write the journal")
		}
	}
	if e.Kind == "shows" && len(e.Items) == 0 {
		return false, nil
	}
	if err := j.Append(e); err != nil {
		return false, errors.New("memory attribution unavailable: cannot write the journal")
	}
	return true, nil
}
