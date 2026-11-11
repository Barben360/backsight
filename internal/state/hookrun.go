package state

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
)

// HookRun is <state>/hook-runs/session-start-<client>.json: the last
// SessionStart hook a coding tool ran on this machine, and its session.
// Doctor reads it as the hooks' heartbeat: a conversation of that tool
// started after it means the tool ran no hook of backsight's (decision
// 0085 D5, Codex).
type HookRun struct {
	At      time.Time `json:"at"`
	Session string    `json:"session"`
}

func hookRunPath(d paths.Dirs, client string) string {
	return filepath.Join(d.State, "hook-runs", "session-start-"+client+".json")
}

// RecordHookRun records a SessionStart run of client at at, for session.
// Best effort: a lost write is redone by the next session start.
func RecordHookRun(d paths.Dirs, client, session string, at time.Time) {
	raw, err := json.Marshal(HookRun{At: at.UTC(), Session: session})
	if err != nil {
		return
	}
	p := hookRunPath(d, client)
	if fsutil.MkdirPrivate(filepath.Dir(p)) != nil {
		return
	}
	_ = fsutil.WriteAtomic(p, raw, 0o600)
}

// LastHookRun is client's last recorded SessionStart run; ok false when
// none was recorded or it cannot be read.
func LastHookRun(d paths.Dirs, client string) (HookRun, bool) {
	raw, err := fsutil.ReadFileShared(hookRunPath(d, client))
	if err != nil {
		return HookRun{}, false
	}
	var r HookRun
	if json.Unmarshal(raw, &r) != nil || r.At.IsZero() {
		return HookRun{}, false
	}
	return r, true
}
