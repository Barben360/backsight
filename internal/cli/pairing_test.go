package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/session"
)

// When Claude Code's process tree does not pair a hook with the MCP
// server by parent pid (on Windows a hook may run under a shell of its
// own), the server finds its session by the session id Claude Code gives
// its children (CLAUDE_CODE_SESSION_ID), in the record the hooks keep
// under the id they carry (decision 0080).
func TestPairingFallsBackToTheSessionID(t *testing.T) {
	mcpParentPID, mcpParentStart, mcpSessionID = 4747, time.Now().Add(-time.Minute), "s-tree"
	t.Cleanup(func() { mcpParentPID, mcpParentStart, mcpSessionID = 0, time.Time{}, "" })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	dir := pathsOf(home).SessionsDir()

	// No record of either kind: the start project, unpaired.
	if out, _ := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps PostgreSQL 16."}); !strings.Contains(out, "project billing)") {
		t.Errorf("unpaired: %s", out)
	}
	// Another session's record pairs nothing.
	_ = session.WriteLiveSession(dir, session.Live{SessionID: "s-other", Cwd: filepath.Join(home, "infra"), Project: "infra", At: time.Now()})
	if out, _ := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps its cron."}); !strings.Contains(out, "project billing)") {
		t.Errorf("another session's record was taken: %s", out)
	}
	// The session's own record, written by its hooks, pairs.
	_ = session.WriteLiveSession(dir, session.Live{SessionID: "s-tree", Cwd: filepath.Join(home, "infra"), Project: "infra", At: time.Now()})
	if out, _ := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Infra keeps Traefik."}); !strings.Contains(out, "project infra)") {
		t.Errorf("not paired by the session id: %s", out)
	}
	if !strings.Contains(telemetryText(t, home), `"live":"session"`) {
		t.Errorf("the pairing is not reported as by session:\n%s", telemetryText(t, home))
	}
	// The parent pid's record, when there is one, comes first.
	_ = session.WriteLive(dir, 4747, session.Live{SessionID: "s-tree", Cwd: filepath.Join(home, "web"), Project: "web", At: time.Now()})
	if out, _ := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Web keeps Vite."}); !strings.Contains(out, "project web)") {
		t.Errorf("the pid record is not first: %s", out)
	}
}

// A session record older than the server's parent is a previous run's:
// ignored, as a pid record is.
func TestPairingIgnoresASessionRecordOlderThanItsParent(t *testing.T) {
	mcpParentPID, mcpParentStart, mcpSessionID = 4848, time.Now(), "s-old"
	t.Cleanup(func() { mcpParentPID, mcpParentStart, mcpSessionID = 0, time.Time{}, "" })
	cs := mcpSession(t)
	home := os.Getenv("HOME")
	_ = session.WriteLiveSession(pathsOf(home).SessionsDir(), session.Live{SessionID: "s-old", Cwd: filepath.Join(home, "infra"), Project: "infra", At: time.Now().Add(-time.Hour)})
	if out, _ := call(t, cs, "memory_store", map[string]any{"kind": "decision", "text": "Billing keeps PostgreSQL 16."}); !strings.Contains(out, "project billing)") {
		t.Errorf("a stale session record was taken: %s", out)
	}
}

// Every hook keeps the record under its session id, whatever its parent
// pid (none known here): the fallback always has something to read.
func TestHooksRecordTheirSessionByID(t *testing.T) {
	home := hookHome(t)
	t.Setenv(claudePIDEnv, "")
	fire(t, "session-start", map[string]any{"session_id": "s-hook", "source": "startup", "cwd": filepath.Join(home, "billing")})
	l, ok := session.ReadLiveSession(pathsOf(home).SessionsDir(), "s-hook")
	if !ok || l.Project != "billing" || l.Cwd != filepath.Join(home, "billing") || l.At.IsZero() {
		t.Errorf("record %+v %v", l, ok)
	}
}
