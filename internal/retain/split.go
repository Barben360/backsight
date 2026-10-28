package retain

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// SplitSession is a session continued on two PCs (decision 0073): the
// server held one of its documents up to a record this PC's transcript
// lacks, so this PC sent what its sent index lacked as the next segment
// instead of quarantining it. The part both PCs share may be stored twice.
type SplitSession struct {
	Documents []string  `json:"documents"`
	FirstAt   time.Time `json:"first_at"`
}

// splitWait: how long RecordSplit waits for another flush recording one.
const splitWait = 2 * time.Second

func splitPath(stateDir string) string { return filepath.Join(stateDir, "split-sessions.json") }

// SplitSessionsFile is where the split sessions of the server this PC
// sends to are recorded: connect moves it aside with the cursors.
func SplitSessionsFile(stateDir string) string { return splitPath(stateDir) }

// SplitSessions reads <state>/split-sessions.json, by session id; absent:
// none.
func SplitSessions(stateDir string) (map[string]SplitSession, error) {
	out := map[string]SplitSession{}
	raw, err := os.ReadFile(splitPath(stateDir))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return out, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RecordSplit records that session sid's document was found split at at.
// It lives outside the cursor, which a resync rebuilds; the first time a
// session split is kept, a document already named is not added again.
func RecordSplit(stateDir, sid, document string, at time.Time) error {
	lock := filepath.Join(paths.Dirs{State: stateDir}.LocksDir(), "split-sessions.lock")
	deadline := time.Now().Add(splitWait)
	l, _, err := state.Acquire(lock, "retain flush")
	for held := (*state.HeldError)(nil); errors.As(err, &held) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		l, _, err = state.Acquire(lock, "retain flush")
	}
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()
	all, err := SplitSessions(stateDir)
	if err != nil {
		return err
	}
	s, ok := all[sid]
	if !ok {
		s.FirstAt = at.UTC()
	}
	if slices.Contains(s.Documents, document) {
		return nil
	}
	s.Documents = append(s.Documents, document)
	all[sid] = s
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(splitPath(stateDir), raw, 0o600)
}
