package feedback

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// Turn is a session's current turn, kept by the prompt hook so that later
// hooks of the turn journal under its id. File and Offset are where the
// journal stood when the turn began.
type Turn struct {
	ID      string    `json:"id"`
	N       int       `json:"n"`
	At      time.Time `json:"at"`
	Machine bool      `json:"machine,omitempty"` // a message Claude Code sent, not a prompt the user typed
	File    string    `json:"file,omitempty"`
	Offset  int64     `json:"offset,omitempty"`
}

// sessionRE is session.ValidID's pattern, repeated here: package session
// imports config, which may come to import this package.
var sessionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func turnFile(dir, sess string) string { return filepath.Join(dir, "turns", sess+".json") }

// SetTurn makes t the current turn of session sess.
func SetTurn(dir, sess string, t Turn) error {
	if !sessionRE.MatchString(sess) {
		return fmt.Errorf("invalid session id %q", sess)
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(turnFile(dir, sess), raw, 0o600)
}

// CurrentTurn is the turn SetTurn last recorded for sess; false when there
// is none or it cannot be read.
func CurrentTurn(dir, sess string) (Turn, bool) {
	var t Turn
	if !sessionRE.MatchString(sess) {
		return t, false
	}
	raw, err := os.ReadFile(turnFile(dir, sess))
	if err != nil || json.Unmarshal(raw, &t) != nil {
		return Turn{}, false
	}
	return t, true
}
