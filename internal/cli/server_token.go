package cli

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/token"
)

// tokenResult is what --json says: never the token, which holds the key.
type tokenResult struct {
	URL      string `json:"url"`
	Bank     string `json:"bank"`
	ServerID string `json:"server_id"`
	Written  string `json:"written"`
}

// clientURL is the API address other PCs use: the private address the
// server is exposed on, at its API port. A loopback server has none.
func clientURL(cfg config.Config) (string, error) {
	if cfg.Network.Exposure != "private" {
		return "", &output.Error{Code: output.Usage, Message: "the server listens on 127.0.0.1 only, so no other PC can reach it: run backsight server expose --private <ip> first"}
	}
	port, err := cfg.APIPort()
	if err != nil {
		return "", blocked(err)
	}
	return "http://" + net.JoinHostPort(cfg.Network.BindIP, strconv.Itoa(port)), nil
}

// tokenOut refuses a token output that is not a person's: the token holds
// the API key, so it goes to a file or to a terminal, never to a pipe or
// an agent's transcript.
func tokenOut(e *env, out string) error {
	if out == "" && (e.json || !stdoutIsTerminal()) {
		return &output.Error{Code: output.Usage, Message: "the token holds the API key: write it with --out FILE"}
	}
	return nil
}

// onTerminal is writeToken's where for a token printed to the terminal.
const onTerminal = "the terminal"

// writeToken writes the encoded token to out (atomically, mode 0600, an
// existing file replaced) or, with no out, prints it to a terminal; where
// says which.
func writeToken(e *env, tok token.Token, out string) (where string, err error) {
	if err := tokenOut(e, out); err != nil {
		return "", err
	}
	if out == "" {
		if _, err := fmt.Fprintln(e.stdout, token.Encode(tok)); err != nil {
			return "", blocked(err)
		}
		return onTerminal, nil
	}
	if abs, err := filepath.Abs(out); err == nil {
		out = abs
	}
	if err := fsutil.WriteAtomic(out, []byte(token.Encode(tok)+"\n"), 0o600); err != nil {
		return "", blocked(err)
	}
	return out, nil
}

func newServerTokenCmd(e *env) *cobra.Command {
	var out string
	var rotate bool
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Write the connection token other PCs join this server with (it holds the API key)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			if err := requireLocal(cfg); err != nil {
				return err
			}
			url, err := clientURL(cfg)
			if err != nil {
				return err
			}
			if err := tokenOut(e, out); err != nil {
				return err
			}
			var d paths.Dirs
			var rt server.Runtime
			if rotate {
				rt = runtimeFor(cfg)
				if err := pinGuard(cfg, rt); err != nil {
					return err
				}
				if d, _, err = machine(); err != nil {
					return err
				}
				unlock, err := serverLock(d, "server token --rotate")
				if err != nil {
					return err
				}
				defer unlock()
				// Again under the lock: another command may have changed them.
				if _, sec, err = loadServerConfig(); err != nil {
					return err
				}
			}
			if sec.APIKey == "" {
				return needSecrets([]string{"api_key"}, "backsight server token")
			}
			// The server id first, on the current key: a server that does not
			// answer leaves the key as it is. server install's warning says
			// this command writes a missing id.
			ctx := cmd.Context()
			id, v, err := serverIdentity(ctx, cfg, sec)
			if err != nil {
				return err
			}
			if rotate {
				if sec, v, err = rotateKey(ctx, d, cfg, sec, rt); err != nil {
					return err
				}
			}
			tok := token.Token{URL: url, Bank: cfg.Server.Bank, APIKey: sec.APIKey, UIURL: uiURL(cfg), HindsightVersion: v.APIVersion, ServerID: id,
				LLM: token.LLM{Provider: cfg.LLM.Provider, Model: cfg.LLM.Model, Currency: cfg.LLM.Currency,
					PriceInput: cfg.LLM.PriceInput, PriceCached: cfg.LLM.PriceCached, PriceOutput: cfg.LLM.PriceOutput}}
			where, err := writeToken(e, tok, out)
			if err != nil {
				if rotate {
					return redacted(fmt.Errorf("the API key was rotated, but the token could not be written (%s): run backsight server token --out FILE to write it", err.Error()), sec)
				}
				return redacted(err, sec)
			}
			res := tokenResult{URL: url, Bank: cfg.Server.Bank, ServerID: id, Written: where}
			return e.emit("server token", res, func() string { return tokenText(res, rotate) })
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write the token to this file (mode 0600) instead of the terminal")
	cmd.Flags().BoolVar(&rotate, "rotate", false, "give the server a new API key first: every token handed out before stops working")
	return cmd
}

func tokenText(r tokenResult, rotated bool) string {
	text := ""
	if rotated {
		text = "API key rotated: the server and this PC use the new one; every other PC joins again with the new token.\n"
	}
	where := "written to " + r.Written
	if r.Written == onTerminal {
		where = "printed above"
	}
	return text + fmt.Sprintf("connection token for %s (bank %s, server %s) %s.\n"+
		"It holds the API key: carry it to the other PC yourself, never through a chat or a shared folder.\n", r.URL, r.Bank, r.ServerID, where)
}

// serverIdentity is the server's id (written now if it has none) and its
// version, read on sec's API key.
func serverIdentity(ctx context.Context, cfg config.Config, sec config.Secrets) (string, hindsight.Version, error) {
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err != nil {
		return "", hindsight.Version{}, blocked(err)
	}
	id, err := retain.EnsureServerID(ctx, cl, cfg.Server.Bank)
	if err != nil {
		return "", hindsight.Version{}, redacted(fmt.Errorf("reading the server id at %s: %s: check backsight server status", cfg.Server.URL, err.Error()), sec)
	}
	v, err := cl.Version(ctx)
	if err != nil {
		return "", hindsight.Version{}, redacted(fmt.Errorf("reading the server version at %s: %s", cfg.Server.URL, err.Error()), sec)
	}
	return id, v, nil
}

// rotateKey gives the server a new API key: secrets.toml, the server's
// files, a restart, and health and a bank read on the new key. Any failure
// puts the old key back and starts the server on it again; the error says
// whether that worked. Errors mask both keys.
func rotateKey(ctx context.Context, d paths.Dirs, cfg config.Config, old config.Secrets, rt server.Runtime) (config.Secrets, hindsight.Version, error) {
	var next config.Secrets
	err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error {
		var fresh config.Secrets
		config.GenerateMissing(&fresh) // the generator every server key comes from
		s.APIKey = fresh.APIKey
		next = *s
		return nil
	})
	if err != nil {
		return old, hindsight.Version{}, redacted(fmt.Errorf("writing the new API key to %s: %s: nothing was changed, the old token still works", d.SecretsFile(), err.Error()), old)
	}
	// Messages quote causes as text: redacted keeps only the message of a
	// wrapped *output.Error.
	mask := func(err error) error { return redacted(redacted(err, old), next) }
	deploy := func(ctx context.Context, sec config.Secrets) (hindsight.Version, error) {
		files, err := renderOn(rt, sec, "backsight server token --rotate")
		if err != nil {
			return hindsight.Version{}, err
		}
		if _, err := rt.Write(files); err != nil {
			return hindsight.Version{}, err
		}
		if err := rt.Start(ctx); err != nil {
			return hindsight.Version{}, err
		}
		v, err := waitHealthy(ctx, rt, cfg, sec)
		if err != nil {
			return v, err
		}
		// /health may answer without a key: a bank read proves this one.
		cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
		if err == nil {
			_, err = retain.ReadServerID(ctx, cl, cfg.Server.Bank)
		}
		return v, err
	}
	v, err := deploy(ctx, next)
	if err == nil {
		return next, v, nil
	}
	cause := mask(err).Error()
	// Put the old key back even when the command is interrupted.
	ctx = context.WithoutCancel(ctx)
	if rerr := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error {
		s.APIKey = old.APIKey
		return nil
	}); rerr != nil {
		return old, hindsight.Version{}, mask(fmt.Errorf("the server did not run on the new API key (%s), and putting the old key back in %s failed (%s): "+
			"secrets.toml holds the new key, which no token carries, and the old token no longer works: run backsight server start, then backsight server status; "+
			"once it is healthy, backsight server token hands out a token with that key", cause, d.SecretsFile(), rerr.Error()))
	}
	if _, rerr := deploy(ctx, old); rerr != nil {
		return old, hindsight.Version{}, mask(fmt.Errorf("the server did not run on the new API key (%s); the old key is back in secrets.toml, but the server did not start healthy on it (%s): "+
			"the old token works again once it does: run backsight server start, then backsight server status", cause, rerr.Error()))
	}
	return old, hindsight.Version{}, mask(fmt.Errorf("the server did not run on the new API key (%s): the old key is back and the server runs on it, so the old token still works", cause))
}
