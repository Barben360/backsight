package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"unicode/utf16"

	"github.com/Barben360/backsight/internal/supervise"
)

// What doctor reads of the native server on Windows (decision 0080): the
// scheduled task's state, the supervisor's answer and the VC++ runtime
// DLLs' copies. Read-only.

// The task's states as TaskState reads them (NotInstalled when Task
// Scheduler does not know it).
const (
	TaskEnabled  = "enabled"
	TaskDisabled = "disabled"
)

// taskDisabled finds the task's own <Enabled>false</Enabled>, in its
// Settings: the triggers carry their own Enabled.
var taskDisabled = regexp.MustCompile(`(?s)<Settings>.*?<Enabled>\s*false\s*</Enabled>.*?</Settings>`)

// TaskState is the server's scheduled task: TaskEnabled, TaskDisabled
// (server stop disables it), or NotInstalled when schtasks cannot query
// it. It reads the task's XML, never schtasks' localized text.
func (n Native) TaskState(ctx context.Context) (string, error) {
	if n.Err != nil {
		return "", n.Err
	}
	out, err := n.Sys.Run(ctx, "schtasks", "/Query", "/TN", n.TaskName(), "/XML")
	if err != nil {
		return NotInstalled, nil
	}
	if taskDisabled.Match(utf8XML(out)) {
		return TaskDisabled, nil
	}
	return TaskEnabled, nil
}

// utf8XML is schtasks' XML as UTF-8: it writes UTF-16 when its output is
// a file, the console's code page otherwise.
func utf8XML(raw []byte) []byte {
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xFE {
		return raw
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 2; i+1 < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return []byte(string(utf16.Decode(units)))
}

// SupervisorStatus is the supervisor's answer to status: nil when none
// runs.
func (n Native) SupervisorStatus(ctx context.Context) (*supervise.Status, error) {
	if n.Err != nil {
		return nil, n.Err
	}
	st, err := taskHost{n: n}.ask(ctx, "status")
	if errors.Is(err, supervise.ErrNotRunning) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// DLLManifest is dlls.json: each VC++ runtime DLL → its sha256.
func (n Native) DLLManifest() string { return n.join(n.Root(), dllManifest) }

// DLLProblems checks every copy of the runtime DLLs the deployed venv v
// uses (pg0's bin\, onnxruntime\capi\, the copied installation's bin\)
// against dlls.json: each one missing or other than its sum, as
// "<path>: missing" or "<path>: differs", and how many copies it read.
func (n Native) DLLProblems(v string) (problems []string, copies int, err error) {
	raw, err := os.ReadFile(n.DLLManifest())
	if err != nil {
		return nil, 0, err
	}
	sums := map[string]string{}
	if err := json.Unmarshal(raw, &sums); err != nil {
		return nil, 0, err
	}
	if len(sums) == 0 {
		return nil, 0, errors.New(n.DLLManifest() + " names no DLL")
	}
	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, dir := range n.venvDLLDirs(v) {
		for _, name := range names {
			p := n.join(dir, name)
			copies++
			got, err := os.ReadFile(p)
			switch {
			case errors.Is(err, os.ErrNotExist):
				problems = append(problems, p+": missing")
			case err != nil:
				problems = append(problems, p+": "+err.Error())
			default:
				if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != sums[name] {
					problems = append(problems, p+": differs")
				}
			}
		}
	}
	return problems, copies, nil
}
