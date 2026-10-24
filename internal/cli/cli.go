// Package cli wires the backsight commands.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
)

// env is what every command needs: where to write and whether to emit JSON.
type env struct {
	stdout, stderr io.Writer
	json           bool
}

// emit prints a command result: an envelope with --json, else text(data).
func (e *env) emit(command string, data any, text func() string) error {
	if e.json {
		return output.WriteJSON(e.stdout, command, data, nil)
	}
	_, err := io.WriteString(e.stdout, text())
	return err
}

// machine resolves backsight's directories and the user's home, the
// starting point of every command that reads the machine.
func machine() (paths.Dirs, string, error) {
	d, err := paths.Default()
	if err != nil {
		return d, "", &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return d, "", &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return d, home, nil
}

// report prints a result that may carry a failure: the data is always shown
// (inside the error envelope with --json), and the exit code still follows
// cmdErr without a second message.
func (e *env) report(command string, data any, text func() string, cmdErr error) error {
	if e.json {
		if err := output.WriteJSON(e.stdout, command, data, cmdErr); err != nil {
			return err
		}
	} else if _, err := io.WriteString(e.stdout, text()); err != nil {
		return err
	}
	if cmdErr != nil {
		return reported{output.CodeOf(cmdErr)}
	}
	return nil
}

// reported is an exit code whose message was already printed.
type reported struct{ code int }

func (r reported) Error() string { return fmt.Sprintf("exit %d", r.code) }

func newRoot(e *env) *cobra.Command {
	root := &cobra.Command{
		Use:           "backsight",
		Short:         "A self-hosted Hindsight memory for Claude Code",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&e.json, "json", false, "print one JSON envelope (schema_version 1)")
	root.AddCommand(newVersionCmd(e), newPathsCmd(e), newDoctorCmd(e), newSecretCmd(e), newServerCmd(e), newBankCmd(e))
	return root
}

// usageError marks cobra's own argument and flag errors.
type usageError struct{ error }

// usageArgs makes a positional-argument check a usage error (exit 2).
func usageArgs(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// Execute runs backsight with args and returns the process exit code.
func Execute(args []string, stdout, stderr io.Writer) int {
	e := &env{stdout: stdout, stderr: stderr}
	root := newRoot(e)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	// --json must be honoured even when parsing fails before flags are bound.
	for _, a := range args {
		if a == "--json" {
			e.json = true
		}
	}
	cmd, err := root.ExecuteC()
	if err == nil {
		return output.OK
	}
	var rep reported
	if errors.As(err, &rep) {
		return rep.code
	}
	code := output.CodeOf(err)
	var ue usageError
	// cobra reports an unknown subcommand as a plain error with this prefix.
	if errors.As(err, &ue) || strings.HasPrefix(err.Error(), "unknown command") {
		code = output.Usage
		err = &output.Error{Code: code, Message: err.Error()}
	}
	if e.json {
		_ = output.WriteJSON(stdout, strings.TrimPrefix(cmd.CommandPath(), "backsight "), nil, err)
	} else {
		_, _ = fmt.Fprintf(stderr, "backsight: %v\n", err)
	}
	return code
}
