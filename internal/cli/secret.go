package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
)

// stdin and stdinIsTerminal are replaced by tests.
var (
	stdin           io.Reader = os.Stdin
	stdinIsTerminal           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
)

func newSecretCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "List and set secrets (values are never shown)", Args: cobra.NoArgs}
	cmd.AddCommand(newSecretListCmd(e), newSecretSetCmd(e))
	return cmd
}

func newSecretListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show which secrets are set",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d, _, err := machine()
			if err != nil {
				return err
			}
			s, err := config.LoadSecrets(d.SecretsFile())
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return &output.Error{Code: output.Blocked, Message: err.Error()}
			}
			names := s.Names()
			return e.emit("secret list", names, func() string {
				keys := make([]string, 0, len(names))
				for k := range names {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				var b strings.Builder
				for _, k := range keys {
					state := "not set"
					if names[k] {
						state = "set"
					}
					fmt.Fprintf(&b, "%-16s %s\n", k, state)
				}
				return b.String()
			})
		},
	}
}

func newSecretSetCmd(e *env) *cobra.Command {
	var fromEnv, fromFile string
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Set a secret from a terminal prompt, a pipe, --from-env or --from-file",
		Long: "The value is never an argument: type it at the prompt (no echo), pipe it on stdin " +
			"(first line), or name an environment variable or a file.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if fromEnv != "" && fromFile != "" {
				return &output.Error{Code: output.Usage, Message: "use --from-env or --from-file, not both"}
			}
			value, err := readSecret(e, name, fromEnv, fromFile)
			if err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set(name, value) }); err != nil {
				code := output.Blocked
				if strings.HasPrefix(err.Error(), "unknown secret") {
					code = output.Usage
				}
				return &output.Error{Code: code, Message: err.Error()}
			}
			return e.emit("secret set", map[string]string{"name": name, "file": d.SecretsFile()}, func() string {
				return name + " saved in " + d.SecretsFile() + "\n"
			})
		},
	}
	cmd.Flags().StringVar(&fromEnv, "from-env", "", "read the value from this environment variable")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read the value from the first line of this file")
	return cmd
}

func readSecret(e *env, name, fromEnv, fromFile string) (string, error) {
	switch {
	case fromEnv != "":
		v, ok := os.LookupEnv(fromEnv)
		v = strings.TrimSpace(v)
		if !ok || v == "" {
			return "", &output.Error{Code: output.Usage, Message: fromEnv + " is not set"}
		}
		return v, nil
	case fromFile != "":
		f, err := os.Open(fromFile)
		if err != nil {
			return "", &output.Error{Code: output.Blocked, Message: err.Error()}
		}
		defer func() { _ = f.Close() }()
		return firstLine(f)
	case stdinIsTerminal():
		fmt.Fprintf(e.stderr, "%s (input hidden): ", name)
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(e.stderr)
		if err != nil {
			return "", &output.Error{Code: output.Blocked, Message: err.Error()}
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return firstLine(stdin)
}

func firstLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 64<<10)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return strings.TrimSpace(line), nil
}
