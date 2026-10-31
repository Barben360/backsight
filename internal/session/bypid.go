package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// Live is what the hooks of one Claude Code process last saw: its session
// and working directory. Claude Code starts hooks and MCP servers as
// children of the same process, so the MCP server finds its session by its
// parent pid (decision 0056).
type Live struct {
	SessionID string    `json:"session_id"`
	Cwd       string    `json:"cwd"`
	Project   string    `json:"project"`
	At        time.Time `json:"at"`
}

func byPIDPath(dir string, pid int) string {
	return filepath.Join(dir, "by-pid", strconv.Itoa(pid)+".json")
}

// WriteLive records what the hooks of Claude Code process pid see now.
func WriteLive(dir string, pid int, l Live) error {
	if pid <= 1 || !ValidID(l.SessionID) {
		return nil
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(byPIDPath(dir, pid), raw, 0o600)
}

// ReadLive returns the record of Claude Code process pid, if any.
func ReadLive(dir string, pid int) (Live, bool) {
	var l Live
	raw, err := fsutil.ReadFileShared(byPIDPath(dir, pid))
	if err != nil || json.Unmarshal(raw, &l) != nil || !ValidID(l.SessionID) {
		return Live{}, false
	}
	return l, true
}

// PruneLive removes records older than maxAge (Claude Code processes that
// ended long ago).
func PruneLive(dir string, maxAge time.Duration, now time.Time) {
	ents, err := os.ReadDir(filepath.Join(dir, "by-pid"))
	if err != nil {
		return
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > maxAge {
			_ = os.Remove(filepath.Join(dir, "by-pid", e.Name()))
		}
	}
}
