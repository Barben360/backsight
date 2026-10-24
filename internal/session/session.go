// Package session keeps what the hooks know about one Claude Code session
// (A.3 sessions/<id>.json): prompts, what was injected and why, the seen
// set, stores and the gate. Hooks update it with a try-lock and give up
// rather than wait (SPEC §6.1).
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/state"
)

// State of one session.
type State struct {
	Project          string      `json:"project"`
	Prompts          int         `json:"prompts"`     // human prompts seen
	LastInject       int         `json:"last_inject"` // prompt number of the last recall injection
	LastWords        []string    `json:"last_words"`  // vocabulary of the last injected prompt (topic shift)
	Seen             []string    `json:"seen"`        // memory ids already shown this session
	Injected         []Injection `json:"injected"`
	Stored           []string    `json:"stored"` // documents stored this session
	GatedPrompt      int         `json:"gated_prompt"`
	SearchedPromptID int         `json:"searched_prompt_id"` // last prompt during which a memory tool ran
	CharsInjected    int         `json:"chars_injected"`     // hook-injected text after the briefing
	Paused           bool        `json:"paused"`
}

// Injection is one memory a hook put in the context, and the rule that let
// it through (what `why` shows).
type Injection struct {
	ID      string    `json:"id"`
	Text    string    `json:"text"`
	Project string    `json:"project"`
	Score   float64   `json:"score"`
	Scope   string    `json:"scope"`
	Prompt  int       `json:"prompt"` // 0: the startup briefing
	Rule    string    `json:"rule"`   // briefing | first | gap | topic_shift | gate
	At      time.Time `json:"at"`
}

// Store is the sessions directory and its locks.
type Store struct {
	Dir   string
	Locks string
}

// ErrBusy: another hook holds this session's lock.
var ErrBusy = errors.New("session state busy")

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func checkID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

func (s Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

// Load reads a session; an unknown one is empty.
func (s Store) Load(id string) (State, error) {
	var st State
	if err := checkID(id); err != nil {
		return st, err
	}
	raw, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

// Update applies fn under the session's lock, or returns ErrBusy at once.
func (s Store) Update(id string, fn func(*State) error) error {
	if err := checkID(id); err != nil {
		return err
	}
	lockPath := filepath.Join(s.Locks, "session-"+id+".lock")
	l, err := state.TryAcquire(lockPath, "hook")
	var held *state.HeldError
	if errors.As(err, &held) && held.Stale && state.BreakStale(held) == nil {
		l, err = state.TryAcquire(lockPath, "hook")
	}
	if errors.As(err, &held) {
		return ErrBusy
	}
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()
	st, err := s.Load(id)
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax) || errors.As(err, &typeErr):
		// A corrupt state starts over, paused: a pause must never be lost
		// (flushes treat an unreadable state as paused too).
		st = State{Paused: true}
	case err != nil:
		return err // unreadable for now (permissions, I/O): never overwrite it
	}
	if err := fn(&st); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.path(id), raw, 0o600)
}

// Paused says whether session id is paused; an unreadable state counts as
// paused (fail closed). An unknown session is not.
func Paused(dir, id string) (bool, string) {
	if checkID(id) != nil {
		return false, ""
	}
	st, err := Store{Dir: dir}.Load(id)
	if err != nil {
		return true, "session state unreadable: treated as paused"
	}
	return st.Paused, "session paused"
}

// Latest is the most recently written session id, "" when there is none.
func (s Store) Latest() (string, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var best string
	var bestAt time.Time
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || checkID(name) != nil {
			continue
		}
		fi, err := e.Info()
		if err == nil && fi.ModTime().After(bestAt) {
			best, bestAt = name, fi.ModTime()
		}
	}
	return best, nil
}
