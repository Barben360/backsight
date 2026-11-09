// Package cli wires the backsight commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/termstyle"
	"github.com/Barben360/backsight/internal/token"
)

// env is what every command needs: where to write and whether to emit JSON.
// stdout and stderr are termstyle writers knowing their color level
// (decision 0084); color is the --color flag, noSplash --no-splash.
type env struct {
	stdout, stderr io.Writer
	json           bool
	color          termstyle.Mode
	noSplash       bool
}

// emit prints a command result: an envelope with --json, else text(data).
func (e *env) emit(command string, data any, text func() string) error {
	if e.json {
		return output.WriteJSON(e.stdout, command, data, nil)
	}
	_, err := io.WriteString(e.stdout, maskTokenText(text()))
	return err
}

// machine resolves backsight's directories and the user's home, the
// starting point of every command that reads the machine.
func machine() (paths.Dirs, string, error) {
	d, err := paths.Default()
	if err != nil {
		return d, "", &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	if err := d.EnsurePrivate(); err != nil {
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
// cmdErr without a second message. A connection token in the text or the
// error is masked: an error can quote a file given by mistake (the token
// file as --answers-file), and the token holds the API key.
func (e *env) report(command string, data any, text func() string, cmdErr error) error {
	cmdErr = maskReported(cmdErr)
	if e.json {
		if err := output.WriteJSON(e.stdout, command, data, cmdErr); err != nil {
			return err
		}
	} else {
		if _, err := io.WriteString(e.stdout, maskTokenText(text())); err != nil {
			return err
		}
		var shown shownError
		if cmdErr != nil && !errors.As(cmdErr, &shown) { // the reason, which the text may not carry
			_, _ = fmt.Fprintf(e.stderr, "backsight: %v\n", cmdErr)
		}
	}
	if cmdErr != nil {
		return reported{output.CodeOf(cmdErr)}
	}
	return nil
}

// shownError is a failure the command's text output already explains.
type shownError struct{ err *output.Error }

func (s shownError) Error() string { return s.err.Error() }
func (s shownError) Unwrap() error { return s.err }

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
	root.PersistentFlags().Var(colorFlag{&e.color}, "color", "color the text at a terminal: auto, always or never")
	root.PersistentFlags().BoolVar(&e.noSplash, "no-splash", false, "print no splash before the output")
	// What a scheduled task passes instead of the variables it cannot carry.
	root.PersistentFlags().Var(dirFlag("BACKSIGHT_HOME"), "backsight-home", "backsight's directory (BACKSIGHT_HOME), which a scheduled task cannot set")
	root.PersistentFlags().Var(dirFlag("BACKSIGHT_STATE_DIR"), "backsight-state-dir", "backsight's state directory (BACKSIGHT_STATE_DIR), which a scheduled task cannot set")
	_ = root.PersistentFlags().MarkHidden("backsight-home")
	_ = root.PersistentFlags().MarkHidden("backsight-state-dir")
	root.AddCommand(newVersionCmd(e), newPathsCmd(e), newDoctorCmd(e), newSecretCmd(e), newServerCmd(e), newBankCmd(e), newRetainCmd(e), newStoreCmd(e), newJobsCmd(e), newClientCmd(e), newSearchCmd(e), newReadCmd(e), newMCPCmd(), newWhyCmd(e), newRecallPromptCmd(e), newPauseCmd(e, true), newPauseCmd(e, false), newForgetCmd(e), newMemoryCmd(e), newAskCmd(e), newProjectsCmd(e), newStatsCmd(e), newFeedbackCmd(e), newDetectReplayCmd(), newBackupCmd(e), newRestoreCmd(e), newSetupCmd(e), newVerifyCmd(e), newUpdateCmd(e), newChangelogCmd(e), newReportCmd(e), newUninstallCmd(e), newGuideCmd(e), newConnectCmd(e), newInstallShimCmd(e), newDashboardCmd(e), newImportCmd(e))
	requireSubcommands(root)
	return root
}

// dirFlag is a hidden directory flag that sets its variable: what a
// scheduled task passes instead of the BACKSIGHT_HOME or
// BACKSIGHT_STATE_DIR it cannot carry (decision 0080). It sets the
// variable as the flag is parsed, before any command resolves a path; a
// relative directory is a usage error.
type dirFlag string

func (dirFlag) String() string { return "" }
func (dirFlag) Type() string   { return "dir" }

func (f dirFlag) Set(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("must be absolute, got %q", dir)
	}
	return os.Setenv(string(f), dir)
}

// requireSubcommands makes every group a usage error (exit 2) when called
// bare or with an unknown subcommand; cobra would print help and exit 0.
func requireSubcommands(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		requireSubcommands(c)
	}
	if !cmd.HasSubCommands() || cmd.Runnable() || !cmd.HasParent() {
		return
	}
	cmd.Args = usageArgs(cobra.NoArgs)
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		var names []string
		for _, sub := range c.Commands() {
			if sub.IsAvailableCommand() {
				names = append(names, sub.Name())
			}
		}
		return usageError{fmt.Errorf("%s needs a subcommand: %s", c.CommandPath(), strings.Join(names, ", "))}
	}
}

// hookStdin is where hook events are read; tests replace it.
var hookStdin io.Reader = os.Stdin

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

// colorFlag is --color: auto, always or never.
type colorFlag struct{ m *termstyle.Mode }

func (f colorFlag) String() string { return f.m.String() }
func (colorFlag) Type() string     { return "mode" }

func (f colorFlag) Set(s string) error {
	m, err := termstyle.ParseMode(s)
	if err != nil {
		return err
	}
	*f.m = m
	return nil
}

// isTerminalFd says whether a descriptor is a terminal; tests replace it.
var isTerminalFd = func(fd uintptr) bool { return term.IsTerminal(int(fd)) }

// isTerminal: w itself is a file on a terminal. A buffer, a pipe writer
// or a tee never is, whatever this process's own stdout is.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminalFd(f.Fd())
}

// vtRestores are the console modes to put back when the command ends.
type vtRestores struct {
	mu  sync.Mutex
	fns []func()
}

func (r *vtRestores) run() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, fn := range r.fns {
		fn()
	}
	r.fns = nil
}

// styled wraps w in a termstyle writer whose level is resolved on first
// use, once cobra has parsed --json and --color (decision 0084 D1).
func (e *env) styled(w io.Writer, plain bool, restores *vtRestores) *termstyle.Writer {
	return termstyle.NewWriter(w, func() termstyle.Level {
		lvl := termstyle.Resolve(termstyle.Inputs{JSON: e.json, Plain: plain, Flag: e.color, Terminal: isTerminal(w)})
		if lvl == termstyle.None {
			return lvl
		}
		restore, ok := termstyle.EnableVT(w)
		if !ok {
			return termstyle.None
		}
		restores.mu.Lock()
		restores.fns = append(restores.fns, restore)
		restores.mu.Unlock()
		return lvl
	})
}

// Execute runs backsight with args and returns the process exit code.
func Execute(args []string, stdout, stderr io.Writer) int {
	return ExecuteContext(context.Background(), args, stdout, stderr)
}

// ExecuteContext is Execute with a context commands can watch (Ctrl-C).
func ExecuteContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	code, _ := executeContext(ctx, args, stdout, stderr)
	return code
}

// executeContext is ExecuteContext, also returning the command's env (nil
// for hooks) so tests can read what it resolved.
func executeContext(ctx context.Context, args []string, stdout, stderr io.Writer) (int, *env) {
	// Redaction also replaces the exact values of the secret stores, read
	// once at the first redaction of this command (H1, security review).
	redact.SetLocalSecrets(localSecretValues)
	// Hooks bypass the command parser: no usage error (exit 2 blocks in
	// Claude Code) can come out of a hook call, whatever its arguments.
	if len(args) > 0 && args[0] == "hook" {
		return runHook(ctx, args[1:], hookStdin, stdout), nil
	}
	if len(args) > 0 && args[0] == "hook-child" {
		return runHookChild(ctx, args[1:], hookStdin, stdout), nil
	}
	e := &env{}
	root := newRoot(e)
	plain := classForArgs(root, args) == lookPlain
	var restores vtRestores
	defer restores.run()
	e.stdout, e.stderr = e.styled(stdout, plain, &restores), e.styled(stderr, plain, &restores)
	root.SetArgs(args)
	root.SetOut(e.stdout)
	root.SetErr(e.stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	// --json and --color must be honoured even when parsing fails before
	// flags are bound.
	for i, a := range args {
		switch {
		case a == "--json":
			e.json = true
		case strings.HasPrefix(a, "--json="):
			if v, err := strconv.ParseBool(strings.TrimPrefix(a, "--json=")); err == nil && v {
				e.json = true
			}
		case strings.HasPrefix(a, "--color="):
			if m, err := termstyle.ParseMode(strings.TrimPrefix(a, "--color=")); err == nil {
				e.color = m
			}
		case a == "--color" && i+1 < len(args):
			if m, err := termstyle.ParseMode(args[i+1]); err == nil {
				e.color = m
			}
		}
	}
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return output.OK, e
	}
	var rep reported
	if errors.As(err, &rep) {
		return rep.code, e
	}
	code := output.CodeOf(err)
	var ue usageError
	// cobra reports an unknown subcommand as a plain error with this prefix.
	if errors.As(err, &ue) || strings.HasPrefix(err.Error(), "unknown command") {
		code = output.Usage
		err = &output.Error{Code: code, Message: err.Error()}
	}
	err = maskTokens(err)
	if e.json {
		_ = output.WriteJSON(e.stdout, strings.TrimPrefix(cmd.CommandPath(), "backsight "), nil, err)
	} else {
		_, _ = fmt.Fprintf(e.stderr, "backsight: %v\n", err)
	}
	return code, e
}

// tokenInText is a connection token, or its start, inside a message.
var tokenInText = regexp.MustCompile(regexp.QuoteMeta(token.Prefix) + `[A-Za-z0-9_=-]*`)

// maskTokens hides any connection token in err's message: a token typed
// as a flag's value by mistake (--plan=bst1.…) is quoted back by the flag
// parser, and it holds the API key.
func maskTokens(err error) error {
	if !strings.Contains(err.Error(), token.Prefix) {
		return err
	}
	var oe *output.Error
	if errors.As(err, &oe) {
		out := *oe
		out.Message = maskTokenText(oe.Message)
		return &out
	}
	return &output.Error{Code: output.CodeOf(err), Message: maskTokenText(err.Error())}
}

// maskTokenText hides any connection token in s.
func maskTokenText(s string) string {
	if !strings.Contains(s, token.Prefix) {
		return s
	}
	return tokenInText.ReplaceAllString(s, token.Prefix+"[redacted]")
}

// maskReported is maskTokens for report's error (nil passes), keeping a
// failure the text already explains as one.
func maskReported(err error) error {
	if err == nil || !strings.Contains(err.Error(), token.Prefix) {
		return err
	}
	var shown shownError
	if errors.As(err, &shown) {
		out := *shown.err
		out.Message = maskTokenText(out.Message)
		return shownError{&out}
	}
	return maskTokens(err)
}
