// Package cli wires the backsight commands.
package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/output"
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

func newRoot(e *env) *cobra.Command {
	root := &cobra.Command{
		Use:           "backsight",
		Short:         "A self-hosted Hindsight memory for Claude Code",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&e.json, "json", false, "print one JSON envelope (schema_version 1)")
	root.AddCommand(newVersionCmd(e))
	return root
}

// usageError marks cobra's own argument and flag errors.
type usageError struct{ error }

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
	code := output.CodeOf(err)
	var ue usageError
	if errors.As(err, &ue) || strings.HasPrefix(err.Error(), "unknown command") {
		code = output.Usage
		err = &output.Error{Code: code, Message: err.Error()}
	}
	if e.json {
		_ = output.WriteJSON(stdout, cmd.Name(), nil, err)
	} else {
		_, _ = fmt.Fprintf(stderr, "backsight: %v\n", err)
	}
	return code
}
