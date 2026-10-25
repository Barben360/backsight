// Package output holds the command contract shared by every backsight
// command: exit codes and the JSON envelope (SPEC §11.2).
package output

import (
	"encoding/json"
	"errors"
	"io"
)

// SchemaVersion of the JSON envelope; bumped on any breaking change.
const SchemaVersion = 1

// Exit codes. Agents branch on them, so the values never change.
const (
	OK         = 0  // done
	Usage      = 2  // invalid invocation (unknown command or flag)
	NeedsUser  = 10 // the user must answer, type a secret or run something
	Blocked    = 20 // the environment prevents the operation
	RolledBack = 30 // failed and rolled back
	Partial    = 40 // partially done, resumable
)

// Error is a command failure with its exit code. With NeedsUser it carries
// what the user must provide or do (SPEC §11.2 needs_user).
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Questions: the full question schema (setup), when answers are missing.
	Questions any      `json:"questions,omitempty"`
	Secrets   []string `json:"secrets,omitempty"`
	Actions   []Action `json:"actions,omitempty"`
	Resume    string   `json:"resume,omitempty"`
}

// Action is something the user runs: run_as_user | relogin | install.
type Action struct {
	Kind string `json:"kind"`
	Cmd  string `json:"cmd"`
	Why  string `json:"why"`
}

func (e *Error) Error() string { return e.Message }

// CodeOf returns the exit code for err: OK for nil, the code of the first
// *Error in its chain, else Blocked.
func CodeOf(err error) int {
	if err == nil {
		return OK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Blocked
}

type envelope struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	OK            bool   `json:"ok"`
	Data          any    `json:"data,omitempty"`
	Error         *Error `json:"error,omitempty"`
}

// WriteJSON writes one envelope for command: data on success, cmdErr on failure.
func WriteJSON(w io.Writer, command string, data any, cmdErr error) error {
	env := envelope{SchemaVersion: SchemaVersion, Command: command, OK: cmdErr == nil, Data: data}
	if cmdErr != nil {
		var e *Error
		if !errors.As(cmdErr, &e) {
			e = &Error{Code: CodeOf(cmdErr), Message: cmdErr.Error()}
		}
		env.Error = e
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}
