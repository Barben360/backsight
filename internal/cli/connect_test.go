package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/token"
	"github.com/Barben360/backsight/internal/transcript"
)

// hostKey is the API key of the host the tests join.
const hostKey = "hostkey-0123456789abcdefghijkl"

// joinableHost starts the fake server as a host other PCs join: its bank
// and its id written, then the API key required. It returns its token,
// with the host's LLM prices.
func joinableHost(t *testing.T) token.Token {
	t.Helper()
	url := "http://127.0.0.1:" + strconv.Itoa(fakeHindsight(t))
	cl, _ := hindsight.New(url, "", 5*time.Second)
	if err := cl.CreateBank(context.Background(), config.DefaultBank, "backsight", ""); err != nil {
		t.Fatal(err)
	}
	id, err := retain.EnsureServerID(context.Background(), cl, config.DefaultBank)
	if err != nil {
		t.Fatal(err)
	}
	requireKey.Store(true)
	acceptedKey.Store(hostKey)
	bankCalls = nil
	return token.Token{URL: url, Bank: config.DefaultBank, APIKey: hostKey, HindsightVersion: "1.1.0", ServerID: id,
		LLM: token.LLM{Provider: "anthropic", Model: "test-model", Currency: "EUR", PriceInput: 2, PriceOutput: 10}}
}

// tokenFile writes tok as server token --out does, outside home.
func tokenFile(t *testing.T, tok token.Token) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte(token.Encode(tok)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// leaked reports whether text carries tok's key or its encoded body.
func leaked(text string, tok token.Token) bool {
	return strings.Contains(text, tok.APIKey) || strings.Contains(text, strings.TrimPrefix(token.Encode(tok), token.Prefix))
}

// oldKey is the API key a client had before connect.
const oldKey = "the-old-key-0123456789abcdef"

// asClientOf makes home a client of the server at url, bank backsight, on
// API key oldKey.
func asClientOf(t *testing.T, home, url string) {
	t.Helper()
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	if err := os.WriteFile(d.ConfigFile(), []byte("# my client\nschema_version = 1\n\n[server]\nmode = \"remote\"\nurl = \""+url+"\"\nbank = \"backsight\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("api_key", oldKey) }); err != nil {
		t.Fatal(err)
	}
}

type connectEnvelope struct {
	OK    bool          `json:"ok"`
	Data  connectResult `json:"data"`
	Error *output.Error `json:"error"`
}

func parseConnect(t *testing.T, out string) connectEnvelope {
	t.Helper()
	var env connectEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not one envelope: %v\n%s", err, out)
	}
	return env
}

func hasConnectStep(steps []step, name, status, detail string) bool {
	for _, s := range steps {
		if s.Name == name && s.Status == status && strings.Contains(s.Detail, detail) {
			return true
		}
	}
	return false
}

// A token pasted on the command line (by the user or an agent) is refused
// without being echoed, says to rotate the key it exposed, and changes
// nothing: no server call, no file.
func TestConnectRefusesATokenArgument(t *testing.T) {
	home := isolate(t)
	tok := joinableHost(t)
	enc := token.Encode(tok)
	before := treeOf(t, home)
	for _, args := range [][]string{
		{"connect", "--token", enc},
		{"connect", "--token=" + enc, "--json"},
		{"connect", enc, "--plan"},
		{"connect", "--token-file", enc},
		{"connect", "--token-file", tokenFile(t, tok), "--answer", "retention=" + enc},
	} {
		code, out, errOut := run(t, args...)
		if code != output.Usage || leaked(out+errOut, tok) || !strings.Contains(out+errOut, "--token-file F") || !strings.Contains(out+errOut, "backsight server token --rotate") {
			t.Errorf("%s: exit %d\n%s%s", args[1][:7], code, out, errOut)
		}
	}
	if len(bankCalls) != 0 || refusedCalls.Load() != 0 {
		t.Errorf("the server was called: %v", bankCalls)
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err == nil {
		t.Error("config.toml written")
	}
	sameTree(t, before, treeOf(t, home))
}

// --token - reads the token from stdin; with nothing there and no
// terminal, connect asks for a file instead.
func TestConnectReadsTheTokenFromStdin(t *testing.T) {
	isolate(t)
	tok := joinableHost(t)
	withStdin(t, token.Encode(tok)+"\n")
	code, out, errOut := run(t, "connect", "--token", "-", "--plan", "--json")
	env := parseConnect(t, out)
	if code != output.OK || env.Data.ServerID != tok.ServerID || env.Data.URL != tok.URL || env.Data.Situation != joinNew || !env.Data.Plan || leaked(out+errOut, tok) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	withStdin(t, "")
	if code, out, errOut := run(t, "connect", "--token", "-"); code != output.NeedsUser || !strings.Contains(errOut, "backsight connect --token-file F") {
		t.Errorf("empty stdin: exit %d\n%s%s", code, out, errOut)
	}
}

// --token-file reads it from a file, spaces and newlines around it
// ignored; a file that holds no token is refused without quoting it.
func TestConnectReadsTheTokenFromAFile(t *testing.T) {
	isolate(t)
	tok := joinableHost(t)
	f := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(f, []byte("  "+token.Encode(tok)+"  \r\n"), 0o600)
	code, out, errOut := run(t, "connect", "--token-file", f, "--plan", "--json")
	if env := parseConnect(t, out); code != output.OK || env.Data.ServerID != tok.ServerID || env.Data.Bank != config.DefaultBank || leaked(out+errOut, tok) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	_ = os.WriteFile(f, []byte(token.Prefix+"not-base64!"+hostKey+"\n"), 0o600)
	if code, out, errOut := run(t, "connect", "--token-file", f, "--plan"); code != output.Usage || !strings.Contains(errOut, "damaged") || strings.Contains(out+errOut, hostKey) {
		t.Errorf("damaged token: exit %d\n%s%s", code, out, errOut)
	}
}

// After a rotation on the host, the old token's key is refused by the
// server: connect stops before writing anything, never quoting the key.
func TestConnectRefusesARotatedKey(t *testing.T) {
	home := isolate(t)
	tok := joinableHost(t)
	tok.APIKey = "the-old-key-before-rotation-0123"
	before := treeOf(t, home)
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--non-interactive")
	if code != output.Blocked || !strings.Contains(errOut, "the server rejected this token's key (rotated?)") || leaked(out+errOut, tok) {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	sameTree(t, before, treeOf(t, home))
}

// A token made for another server than the one at its address (the host
// reinstalled without a restore) is refused before anything is written.
func TestConnectRefusesAnotherServerID(t *testing.T) {
	home := isolate(t)
	tok := joinableHost(t)
	tok.ServerID = "another-server-id"
	before := treeOf(t, home)
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--non-interactive", "--json")
	if env := parseConnect(t, out); code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "is not the one this token was made for: get a fresh token there") || leaked(out+errOut, tok) {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	sameTree(t, before, treeOf(t, home))
}

// A server on a public IP literal is refused (the key would cross the
// internet); a host name passes to the server checks.
func TestConnectRefusesAPublicAddress(t *testing.T) {
	home := isolate(t)
	tok := joinableHost(t)
	before := treeOf(t, home)
	tok.URL = "http://8.8.8.8:8888"
	if code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--plan"); code != output.Usage || !strings.Contains(errOut, "8.8.8.8:8888 is a public address") {
		t.Errorf("public: exit %d\n%s%s", code, out, errOut)
	}
	tok.URL = "http://localhost:" + strconv.Itoa(closedPort(t))
	if code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--plan"); code != output.Blocked || !strings.Contains(errOut, "does not answer") {
		t.Errorf("host name: exit %d\n%s%s", code, out, errOut)
	}
	sameTree(t, before, treeOf(t, home))
}

// --plan checks the server and shows the writes and the backfill offer,
// priced with the host's LLM from the token; it changes nothing.
func TestConnectPlanChangesNothing(t *testing.T) {
	home := isolate(t)
	tok := joinableHost(t)
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	f := tokenFile(t, tok)
	before := treeOf(t, home)
	code, out, errOut := run(t, "connect", "--token-file", f, "--plan", "--json")
	steps := parseConnect(t, out).Data.Steps
	if code != output.OK || !hasConnectStep(steps, "config", "planned", "anthropic/test-model") || !hasConnectStep(steps, "secrets", "planned", "api_key") ||
		!hasConnectStep(steps, "binding", "planned", tok.ServerID) || !hasConnectStep(steps, "setup", "planned", "1 past session(s) (≈ ") || !hasConnectStep(steps, "setup", "planned", " EUR, capped") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if code, out, _ := run(t, "connect", "--token-file", f, "--plan"); code != output.OK || !strings.Contains(out, "connect will:") || !strings.Contains(out, "EUR") {
		t.Errorf("text plan: exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, home))
}

// Without a terminal, connect needs the consent as an answer: exit 10,
// the resume naming a token file, nothing written.
func TestConnectAsksForConsent(t *testing.T) {
	home := isolate(t)
	tok := joinableHost(t)
	before := treeOf(t, home)
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--json")
	if env := parseConnect(t, out); code != output.NeedsUser || env.Error == nil || env.Error.Resume != "backsight connect --token-file F --answer consent=yes" || len(env.Data.Steps) == 0 {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if code, out, _ := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=no"); code != output.OK || !strings.Contains(out, "nothing changed") {
		t.Errorf("consent=no: exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, home))
}

// A new PC joins: config.toml points at the host (its LLM for estimates,
// never a key), the key is in secrets.toml (0600), the server's id is
// recorded, and setup runs its client steps with the consent given.
func TestConnectNewPC(t *testing.T) {
	home, f := setupMachine(t)
	tok := joinableHost(t)
	file := tokenFile(t, tok)
	code, out, errOut := run(t, "connect", "--token-file", file, "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseConnect(t, out)
	if code != output.OK || env.Data.Setup == nil {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	d := pathsOf(home)
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil || cfg.Server.Mode != "remote" || cfg.Server.URL != tok.URL || cfg.Server.Bank != tok.Bank || cfg.LLM.Provider != "anthropic" ||
		cfg.LLM.Model != "test-model" || cfg.LLM.PriceInput != 2 || cfg.LLM.Currency != "EUR" {
		t.Errorf("config: %+v %+v %v", cfg.Server, cfg.LLM, err)
	}
	if raw, _ := os.ReadFile(d.ConfigFile()); strings.Contains(string(raw), hostKey) || !strings.HasPrefix(string(raw), "# Written by backsight connect.") {
		t.Errorf("config.toml:\n%s", raw)
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil || sec.APIKey != hostKey {
		t.Errorf("secrets: %v", err)
	}
	if fi, err := os.Stat(d.SecretsFile()); runtime.GOOS != "windows" && (err != nil || fi.Mode().Perm() != 0o600) {
		t.Errorf("secrets.toml mode: %v %v", fi.Mode(), err)
	}
	if st, _ := state.Load(d.StateFile()); st.RemoteServer == nil || st.RemoteServer.ID != tok.ServerID || st.RemoteServer.URL != tok.URL {
		t.Errorf("binding: %+v", st.RemoteServer)
	}
	steps := env.Data.Setup.Steps
	if !hasStep(steps, "server", "skipped", tok.URL) || !hasStep(steps, "docs", "skipped", "the server's host indexes its docs") || !hasStep(steps, "client", "done", "") {
		t.Errorf("setup steps: %+v", steps)
	}
	if cmds := strings.Join(f.commands(), ","); !strings.Contains(cmds, "client install") || strings.Contains(cmds, "server install") || strings.Contains(cmds, "guide index") {
		t.Errorf("setup ran: %s", cmds)
	}
	if next := strings.Join(env.Data.Setup.NextSteps, "\n"); strings.Contains(next, "backup key") {
		t.Errorf("a client is told to save a backup key it has none of: %s", next)
	}
	abs, _ := filepath.Abs(file)
	if !env.Data.RestartRequired || !strings.Contains(strings.Join(env.Data.NextSteps, "\n"), "delete "+abs+": it holds the API key") || env.Data.Situation != joinNew {
		t.Errorf("result: %+v", env.Data)
	}
	if _, err := os.Stat(file); err != nil {
		t.Error("connect deleted the token file: it only says to")
	}
}

// Neither the key nor the token reaches any output: plan, run, text,
// JSON, and a refusal.
func TestConnectJSONNeverCarriesKey(t *testing.T) {
	setupMachine(t)
	tok := joinableHost(t)
	f := tokenFile(t, tok)
	for _, args := range [][]string{
		{"--plan", "--json"}, {"--plan"},
		{"--answer", "consent=yes", "--non-interactive", "--json"},
		{"--answer", "consent=yes", "--non-interactive"},
	} {
		code, out, errOut := run(t, append([]string{"connect", "--token-file", f}, args...)...)
		if code != output.OK || leaked(out+errOut, tok) || !strings.Contains(out, tok.ServerID) && args[0] == "--plan" {
			t.Errorf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
	acceptedKey.Store("rotated-0123456789abcdefghij")
	if code, out, errOut := run(t, "connect", "--token-file", f, "--plan", "--json"); code != output.Blocked || leaked(out+errOut, tok) {
		t.Errorf("refused: exit %d\n%s%s", code, out, errOut)
	}
}

// Joining again the server this PC is bound to (its address or key
// changed) edits the config and the key only: the sends go on.
func TestConnectSameServerOnlyUpdatesURLAndKey(t *testing.T) {
	home, _ := setupMachine(t)
	tok := joinableHost(t)
	d := pathsOf(home)
	asClientOf(t, home, "http://100.101.1.2:8888")
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_ = state.Update(d, "test", func(s *state.State) error {
		s.RetainSince, s.RemoteServer = &since, &state.RemoteServer{ID: tok.ServerID, URL: "http://100.101.1.2:8888"}
		return nil
	})
	_ = os.MkdirAll(d.CursorsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.CursorsDir(), "s1.json"), []byte(`{"offset":42}`), 0o600)
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--non-interactive", "--json")
	if env := parseConnect(t, out); code != output.OK || env.Data.Situation != joinSame || hasConnectStep(env.Data.Steps, "history", "done", "") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	cfg, _ := config.Load(d.ConfigFile(), d, home)
	sec, _ := config.LoadSecrets(d.SecretsFile())
	if raw, _ := os.ReadFile(d.ConfigFile()); cfg.Server.URL != tok.URL || sec.APIKey != hostKey || !strings.HasPrefix(string(raw), "# my client\n") {
		t.Errorf("config %s, key changed %v\n%s", cfg.Server.URL, sec.APIKey == hostKey, raw)
	}
	if raw, err := os.ReadFile(filepath.Join(d.CursorsDir(), "s1.json")); err != nil || string(raw) != `{"offset":42}` {
		t.Errorf("cursor: %q %v", raw, err)
	}
	if _, err := os.Stat(d.ConnectDir()); err == nil {
		t.Error("retain state moved aside")
	}
	if st, _ := state.Load(d.StateFile()); st.RetainSince == nil || !st.RetainSince.Equal(since) || st.RemoteServer.URL != tok.URL {
		t.Errorf("state: %v %+v", st.RetainSince, st.RemoteServer)
	}
}

// The old and the new server use the same bank id: the old cursors would
// match and history would never reach the new one. connect moves them
// aside, and the next flush sends the session to the new server.
func TestConnectToAnotherServerResendsHistory(t *testing.T) {
	home, _ := setupMachine(t)
	d := pathsOf(home)
	oldURL := "http://127.0.0.1:" + strconv.Itoa(fakeHindsight(t))
	cl, _ := hindsight.New(oldURL, "", 5*time.Second)
	oldID, err := retain.EnsureServerID(context.Background(), cl, config.DefaultBank)
	if err != nil {
		t.Fatal(err)
	}
	asClientOf(t, home, oldURL)
	since := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	_ = state.Update(d, "test", func(s *state.State) error {
		s.RetainSince, s.RemoteServer = &since, &state.RemoteServer{ID: oldID, URL: oldURL}
		return nil
	})
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	writeTranscript(t, home, "s1", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	if code, out, errOut := run(t, "retain", "flush", "--session", "s1"); code != output.OK || !strings.Contains(out, "1 sent") {
		t.Fatalf("to the old server: exit %d\n%s%s", code, out, errOut)
	}

	split := retain.SplitSessionsFile(d.State) // the old server's split sessions
	_ = os.WriteFile(split, []byte(`{"s9":{"document":"conversation:s9"}}`), 0o600)
	tok := joinableHost(t) // the new server: holds nothing of s1
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--answer", "backfill=no", "--non-interactive", "--json")
	if env := parseConnect(t, out); code != output.OK || env.Data.Situation != joinOther || !hasConnectStep(env.Data.Steps, "history", "done", "start over") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	runs, _ := filepath.Glob(filepath.Join(d.ConnectDir(), "*"))
	if len(runs) != 1 {
		t.Fatalf("connect runs kept: %v", runs)
	}
	if _, err := os.Stat(split); err == nil {
		t.Error("split-sessions.json left in place")
	}
	for _, p := range []string{"cursors", "sent-index", "split-sessions.json"} {
		if _, err := os.Stat(filepath.Join(runs[0], p)); err != nil {
			t.Errorf("%s not kept: %v", p, err)
		}
	}
	var meta connectMeta
	raw, _ := os.ReadFile(filepath.Join(runs[0], "meta.json"))
	if err := json.Unmarshal(raw, &meta); err != nil || meta.URL != oldURL || meta.Mode != "remote" || meta.RemoteServer == nil || meta.RemoteServer.ID != oldID ||
		meta.RetainSince == nil || !meta.RetainSince.Equal(since) {
		t.Errorf("meta.json %v:\n%s", err, raw)
	}
	st, _ := state.Load(d.StateFile())
	if st.RetainSince == nil || !st.RetainSince.Equal(time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)) || st.Backfill != nil ||
		st.RestoreEpoch == nil || st.RestoreEpoch.Bank != config.DefaultBank || st.RestoreEpoch.Epoch != 0 || st.RemoteServer.ID != tok.ServerID {
		t.Errorf("state: since %v, epoch %+v, binding %+v", st.RetainSince, st.RestoreEpoch, st.RemoteServer)
	}
	before := retained.Load()
	if code, out, errOut := run(t, "retain", "flush", "--session", "s1"); code != output.OK || !strings.Contains(out, "1 sent") || retained.Load() == before {
		t.Errorf("to the new server: exit %d, %d retained\n%s%s", code, retained.Load()-before, out, errOut)
	}
}

// A client that found another server at its address joins it with its
// token: the binding moves to the new id and the mismatch is cleared.
func TestConnectAgainClearsMismatch(t *testing.T) {
	home, _ := setupMachine(t)
	tok := joinableHost(t)
	asClientOf(t, home, tok.URL)
	setMismatch(t, home, "b")
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--non-interactive", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.RemoteServer == nil || st.RemoteServer.ID != tok.ServerID || st.RemoteServer.Mismatch != "" || !st.RemoteServer.MismatchAt.IsZero() {
		t.Errorf("binding: %+v", st.RemoteServer)
	}
}

// A server-only host is what the others connect to: connect refuses
// there.
func TestConnectRefusesAServerOnlyHost(t *testing.T) {
	home := isolate(t)
	tok := joinableHost(t)
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	_ = os.WriteFile(d.ConfigFile(), []byte("[server]\nrole = \"server\"\n"), 0o600)
	before := treeOf(t, home)
	if code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes"); code != output.Usage || !strings.Contains(errOut, "server-only host") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	sameTree(t, before, treeOf(t, home))
}

// withConnectFault makes the named steps of applyConnect fail.
func withConnectFault(t *testing.T, steps ...string) {
	t.Helper()
	old := connectFault
	connectFault = func(step string) error {
		if slices.Contains(steps, step) {
			return errors.New("injected " + step + " failure")
		}
		return nil
	}
	t.Cleanup(func() { connectFault = old })
}

// switchingClient is a client of another server than the fake's, with
// cursors, a sent-index, split sessions and the state fields a switch
// resets.
func switchingClient(t *testing.T, home string) {
	t.Helper()
	d := pathsOf(home)
	asClientOf(t, home, "http://100.101.1.2:8888")
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := state.Update(d, "test", func(s *state.State) error {
		s.RetainSince, s.RemoteServer = &since, &state.RemoteServer{ID: "a", URL: "http://100.101.1.2:8888"}
		s.Backfill = &state.Backfill{Done: 1, Total: 2, StartedAt: since}
		s.RestoreEpoch = &state.RestoreEpoch{Bank: config.DefaultBank, Epoch: 3, ReconciledAt: since}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(d.CursorsDir(), 0o700)
	_ = os.MkdirAll(d.SentIndexDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.CursorsDir(), "s1.json"), []byte(`{"offset":42}`), 0o600)
	_ = os.WriteFile(filepath.Join(d.SentIndexDir(), "index.bin"), []byte("12345678"), 0o600)
	_ = os.WriteFile(retain.SplitSessionsFile(d.State), []byte(`{}`), 0o600)
}

// A write that fails before the key is saved puts back what connect had
// changed (moved retain state, state fields, config.toml) and exits 30,
// naming the cause.
func TestConnectRollsBackAFailedWrite(t *testing.T) {
	for _, step := range []string{"config", "secrets"} {
		t.Run(step, func(t *testing.T) {
			home, f := setupMachine(t)
			tok := joinableHost(t)
			switchingClient(t, home)
			before := treeOf(t, home)
			withConnectFault(t, step)
			code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--non-interactive", "--json")
			if env := parseConnect(t, out); code != output.RolledBack || env.Error == nil || !strings.Contains(env.Error.Message, "injected "+step+" failure") ||
				!strings.Contains(env.Error.Message, "this PC is as it was") || leaked(out+errOut, tok) {
				t.Errorf("exit %d\n%s%s", code, out, errOut)
			}
			sameTree(t, before, treeOf(t, home))
			if len(f.calls) != 0 {
				t.Errorf("setup ran: %v", f.commands())
			}
		})
	}
}

// When putting back fails too, connect exits 20 and says what is left and
// where meta.json records the state as it was.
func TestConnectSaysWhatIsLeftWhenTheRollbackFails(t *testing.T) {
	home, _ := setupMachine(t)
	tok := joinableHost(t)
	switchingClient(t, home)
	withConnectFault(t, "secrets", "undo")
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseConnect(t, out)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "putting this PC back failed") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	runs, _ := filepath.Glob(filepath.Join(pathsOf(home).ConnectDir(), "*", "meta.json"))
	if len(runs) != 1 || !strings.Contains(env.Error.Message, runs[0]) {
		t.Errorf("meta.json %v not named:\n%s", runs, env.Error.Message)
	}
}

// A failure recording the server's id, once config.toml and the key are
// written, exits 40 with the command that finishes it, which does.
func TestConnectResumesAFailedBinding(t *testing.T) {
	home, _ := setupMachine(t)
	tok := joinableHost(t)
	switchingClient(t, home)
	file := tokenFile(t, tok)
	withConnectFault(t, "binding")
	code, out, errOut := run(t, "connect", "--token-file", file, "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseConnect(t, out)
	if code != output.Partial || env.Error == nil || env.Error.Resume != "backsight connect --token-file "+shellWord(file)+" --answer consent=yes" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	d := pathsOf(home)
	cfg, _ := config.Load(d.ConfigFile(), d, home)
	sec, _ := config.LoadSecrets(d.SecretsFile())
	if st, _ := state.Load(d.StateFile()); cfg.Server.URL != tok.URL || sec.APIKey != hostKey || st.RemoteServer.ID != "a" {
		t.Errorf("config %s, key written %v, binding %+v", cfg.Server.URL, sec.APIKey == hostKey, st.RemoteServer)
	}
	withConnectFault(t)
	if code, out, errOut := run(t, "connect", "--token-file", file, "--answer", "consent=yes", "--non-interactive"); code != output.OK {
		t.Fatalf("resume: exit %d\n%s%s", code, out, errOut)
	}
	if st, _ := state.Load(d.StateFile()); st.RemoteServer.ID != tok.ServerID {
		t.Errorf("binding: %+v", st.RemoteServer)
	}
}

// A token typed as a flag's value by mistake is quoted back by the flag
// parser: every command's error masks it, text and JSON.
func TestATokenInAFlagValueIsMasked(t *testing.T) {
	isolate(t)
	tok := joinableHost(t)
	enc := token.Encode(tok)
	for _, args := range [][]string{
		{"connect", "--plan=" + enc},
		{"connect", "--plan=" + enc, "--json"},
		{"connect", "--non-interactive=" + enc, "--json"},
		{"setup", "--plan=" + enc},
		{"setup", enc, "--json"},
	} {
		code, out, errOut := run(t, args...)
		if code != output.Usage || leaked(out+errOut, tok) || !strings.Contains(out+errOut, token.Prefix+"[redacted]") {
			t.Errorf("%s: exit %d\n%s%s", args[0], code, out, errOut)
		}
	}
}

// A server on another Hindsight than this backsight's pin is joined with
// a warning (minor: update, patch: nothing to do); an unreadable version
// is the token's.
func TestConnectWarnsOnAVersionSkew(t *testing.T) {
	setupMachine(t)
	tok := joinableHost(t)
	f := tokenFile(t, tok)
	pin := strings.Split(server.PinnedVersion(), ".")
	minor, _ := strconv.Atoi(pin[1])
	patch, _ := strconv.Atoi(pin[2])
	fakeAPIVersion = pin[0] + "." + pin[1] + "." + strconv.Itoa(patch+1)
	code, out, errOut := run(t, "connect", "--token-file", f, "--plan", "--json")
	if env := parseConnect(t, out); code != output.OK || env.Data.Skew != doctor.Info || !strings.Contains(strings.Join(env.Data.Warnings, "\n"), "only the patch differs") {
		t.Errorf("patch: exit %d\n%s%s", code, out, errOut)
	}
	fakeAPIVersion = ""
	code, out, errOut = run(t, "connect", "--token-file", f, "--plan", "--json")
	if env := parseConnect(t, out); code != output.OK || env.Data.HindsightVersion != tok.HindsightVersion {
		t.Errorf("unreadable: exit %d\n%s%s", code, out, errOut)
	}
	other := pin[0] + "." + strconv.Itoa(minor+1) + ".0"
	fakeAPIVersion = other
	code, out, errOut = run(t, "connect", "--token-file", f, "--answer", "consent=yes", "--non-interactive", "--json")
	if env := parseConnect(t, out); code != output.OK || env.Data.Skew != doctor.Warn || env.Data.HindsightVersion != other ||
		!strings.Contains(strings.Join(env.Data.Warnings, "\n"), "backsight update apply") {
		t.Errorf("minor: exit %d\n%s%s", code, out, errOut)
	}
}

// ownServerPC makes home a PC with its own server, deployed, on a fake of
// its own (its id written; the backup identity its daily backups made),
// with the cursors and sent-index of its sends to it. It returns that
// fake's client, to fill its bank. A host started after it shares the
// fakes' key switch: call noKeyRequired then.
func ownServerPC(t *testing.T, home string) *hindsight.Client {
	t.Helper()
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error {
		config.GenerateMissing(s)
		return s.Set("llm_api_key", "fakellmkey")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureBackupIdentity(d); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(composeDir(home), 0o700)
	if err := os.WriteFile(filepath.Join(composeDir(home), "compose.yaml"), []byte("services:\n  hindsight:\n    image: ghcr.io/vectorize-io/hindsight:1.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "", 5*time.Second)
	if _, err := retain.EnsureServerID(context.Background(), cl, ownBank); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := state.Update(d, "test", func(s *state.State) error {
		s.RetainSince = &since
		s.RestoreEpoch = &state.RestoreEpoch{Bank: ownBank, Epoch: 2, ReconciledAt: since}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(d.CursorsDir(), 0o700)
	_ = os.MkdirAll(d.SentIndexDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.CursorsDir(), "s1.json"), []byte(`{"offset":42}`), 0o600)
	_ = os.WriteFile(filepath.Join(d.SentIndexDir(), "index.bin"), []byte("12345678"), 0o600)
	return cl
}

// ownBank is the bank of ownServerPC's server (an install's default).
const ownBank = config.LegacyBank

// noKeyRequired: the fakes share one key switch, and this PC's server is
// not on the host's key.
func noKeyRequired() { requireKey.Store(false) }

// ownFiles are what this PC's own server uses that connect must keep or
// put back byte for byte.
func ownFiles(home string) []string {
	d := pathsOf(home)
	return []string{d.ConfigFile(), d.SecretsFile(), filepath.Join(d.CursorsDir(), "s1.json"), filepath.Join(d.SentIndexDir(), "index.bin")}
}

func contentsOf(t *testing.T, files []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[f] = string(raw)
	}
	return out
}

func sameContents(t *testing.T, before map[string]string) {
	t.Helper()
	for f, want := range before {
		if raw, err := os.ReadFile(f); err != nil || string(raw) != want {
			t.Errorf("%s changed (%v)", f, err)
		}
	}
}

// composeStopped: the server was stopped (Compose's stop, the whole
// project).
func composeStopped(calls []string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "compose") && strings.HasSuffix(c, " stop") })
}

// A token of this machine's own server (its id answers there) is refused
// before anything else: no Docker, nothing written.
func TestConnectRefusesThisMachinesOwnServer(t *testing.T) {
	home, _ := setupMachine(t)
	tok := joinableHost(t)
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	_ = os.WriteFile(d.ConfigFile(), []byte("[server]\nurl = \""+tok.URL+"\"\n"), 0o600)
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("api_key", hostKey) }); err != nil {
		t.Fatal(err)
	}
	dk := withBackupDocker(t)
	before := treeOf(t, home)
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--non-interactive", "--json")
	if env := parseConnect(t, out); code != output.Usage || env.Error == nil || env.Error.Message != "this token is for this machine's own server: it is already connected" || leaked(out+errOut, tok) {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if len(dk.calls) != 0 {
		t.Errorf("Docker was called: %q", dk.calls)
	}
	sameTree(t, before, treeOf(t, home))
}

// A backup that fails, or one that does not verify, stops connect before
// the server is stopped or anything is written: exit 20.
func TestConnectOwnServerBackupFailureChangesNothing(t *testing.T) {
	home, f := setupMachine(t)
	ownServerPC(t, home)
	tok := joinableHost(t)
	noKeyRequired()
	dk := withBackupDocker(t)
	file := tokenFile(t, tok)
	before := contentsOf(t, ownFiles(home))
	for _, c := range []struct {
		fail func()
		want string
	}{
		{func() { dk.failDump = true }, "backing up this PC's server failed"},
		{func() { dk.failDump, dk.restored = false, "41" }, "did not verify"},
	} {
		c.fail()
		code, out, errOut := run(t, "connect", "--token-file", file, "--answer", "consent=yes", "--non-interactive", "--json")
		if env := parseConnect(t, out); code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, c.want) ||
			!strings.Contains(env.Error.Message, "nothing was changed") || leaked(out+errOut, tok) {
			t.Errorf("%s: exit %d\n%s%s", c.want, code, out, errOut)
		}
		if i := composeStopped(dk.calls); i >= 0 {
			t.Errorf("%s: the server was stopped: %q", c.want, dk.calls)
		}
		sameContents(t, before)
		if _, err := os.Stat(pathsOf(home).ConnectDir()); err == nil {
			t.Errorf("%s: retain state moved aside", c.want)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("setup ran: %v", f.commands())
	}
}

// A PC with its own server joins a shared one: the plan counts the
// sessions sent again and the saved notes no transcript here holds; then
// the server is stopped once its backup verified (its data kept), and
// what it used is kept for connect --undo.
func TestConnectOwnServer(t *testing.T) {
	home, f := setupMachine(t)
	cl := ownServerPC(t, home)
	d := pathsOf(home)
	localCfg, _ := config.Load(d.ConfigFile(), d, home)
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), true) // one save, sent again as its replay
	fh, _ := os.Open(filepath.Join(home, ".claude", "projects", "-home-u-billing", "s1.jsonl"))
	recs, _, _ := transcript.Read(fh, 0)
	_ = fh.Close()
	saves := retain.NotesFromRecords(ownBank, "s1", recs, nil, func(transcript.Record) (string, bool) { return "", true })
	if len(saves) != 1 {
		t.Fatalf("saves %+v", saves)
	}
	if err := cl.RetainNow(context.Background(), ownBank, []hindsight.RetainItem{
		{Content: "replayed", DocumentID: saves[0].DocumentID, Metadata: map[string]string{"source": "note"}},
		{Content: "live in s1", DocumentID: "note:live-s1", Metadata: map[string]string{"source": "note", "session": "s1"}},
		{Content: "live in s9", DocumentID: "note:live-s9", Metadata: map[string]string{"source": "note", "session": "s9"}},
		{Content: "no session", DocumentID: "note:none", Metadata: map[string]string{"source": "note"}},
		{Content: "s1's text", DocumentID: "conversation:s1", Metadata: map[string]string{"session_id": "s1"}},
	}); err != nil {
		t.Fatal(err)
	}
	tok := joinableHost(t)
	noKeyRequired()
	dk := withBackupDocker(t)
	file := tokenFile(t, tok)
	before := contentsOf(t, ownFiles(home))

	code, out, errOut := run(t, "connect", "--token-file", file, "--plan", "--json")
	env := parseConnect(t, out)
	own := env.Data.OwnServer
	if code != output.OK || env.Data.Situation != joinOwn || own == nil || own.OrphanNotes == nil || *own.OrphanNotes != 2 || *own.SavedNotes != 4 || own.Sessions != 1 ||
		!strings.Contains(strings.Join(env.Data.Warnings, "\n"), "2 of 4 saved notes have no transcript left on this PC: they stay in this PC's old bank only") ||
		!hasConnectStep(env.Data.Steps, "server", "planned", "back up and verify this PC's server, then stop it (data and backups kept; connect --undo starts it again)") ||
		!hasConnectStep(env.Data.Steps, "setup", "planned", "at most 1 past session(s)") {
		t.Fatalf("plan: exit %d\n%s%s", code, out, errOut)
	}
	if len(dk.calls) != 0 {
		t.Errorf("the plan called Docker: %q", dk.calls)
	}
	sameContents(t, before)

	code, out, errOut = run(t, "connect", "--token-file", file, "--answer", "consent=yes", "--answer", "backfill=no", "--non-interactive", "--json")
	env = parseConnect(t, out)
	if code != output.OK || env.Data.OwnServer == nil || env.Data.OwnServer.Backup == "" || leaked(out+errOut, tok) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	verified := slices.IndexFunc(dk.calls, func(c string) bool {
		return strings.HasPrefix(c, "exec backsight-verify-") && strings.Contains(c, "count(*)")
	})
	if stop := composeStopped(dk.calls); verified < 0 || stop < verified {
		t.Errorf("stopped at %d, verified at %d: %q", stop, verified, dk.calls)
	}
	for _, c := range dk.calls {
		if strings.Contains(c, "compose.yaml down") || strings.Contains(c, "compose.yaml rm") || strings.Contains(c, "volume rm") {
			t.Errorf("the server's data was touched: %s", c)
		}
	}
	if _, err := os.Stat(env.Data.OwnServer.Backup); err != nil {
		t.Errorf("backup %s: %v", env.Data.OwnServer.Backup, err)
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil || cfg.Server.Mode != "remote" || cfg.Server.URL != tok.URL {
		t.Errorf("config: %+v %v", cfg.Server, err)
	}
	runs, _ := filepath.Glob(filepath.Join(d.ConnectDir(), "*"))
	if len(runs) != 1 {
		t.Fatalf("connect runs kept: %v", runs)
	}
	var meta connectMeta
	raw, _ := os.ReadFile(filepath.Join(runs[0], "meta.json"))
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Mode != "local" || meta.URL != localCfg.Server.URL {
		t.Errorf("meta.json %v:\n%s", err, raw)
	}
	for name, p := range map[string]string{"config.toml": d.ConfigFile(), "secrets.toml": d.SecretsFile(), "cursors/s1.json": filepath.Join(d.CursorsDir(), "s1.json"), "sent-index/index.bin": filepath.Join(d.SentIndexDir(), "index.bin")} {
		kept := filepath.Join(runs[0], filepath.FromSlash(name))
		if raw, err := os.ReadFile(kept); err != nil || string(raw) != before[p] {
			t.Errorf("%s not kept as it was: %v", name, err)
		}
		if fi, err := os.Stat(kept); runtime.GOOS != "windows" && (err != nil || fi.Mode().Perm() != 0o600) {
			t.Errorf("%s mode %v", name, fi.Mode())
		}
	}
	if !slices.Contains(f.commands(), "client install") {
		t.Errorf("setup ran: %v", f.commands())
	}
}

// A write that fails once the server is stopped puts it back too: the
// server is started again, exit 30.
func TestConnectOwnServerRollbackStartsTheServerAgain(t *testing.T) {
	home, _ := setupMachine(t)
	ownServerPC(t, home)
	tok := joinableHost(t)
	noKeyRequired()
	dk := withBackupDocker(t)
	before := contentsOf(t, ownFiles(home))
	withConnectFault(t, "secrets")
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--non-interactive", "--json")
	if env := parseConnect(t, out); code != output.RolledBack || env.Error == nil || !strings.Contains(env.Error.Message, "this PC is as it was") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	stop := composeStopped(dk.calls)
	started := slices.IndexFunc(dk.calls, func(c string) bool { return strings.HasPrefix(c, "compose") && strings.Contains(c, " up -d") })
	if stop < 0 || started < stop {
		t.Errorf("stopped at %d, started at %d: %q", stop, started, dk.calls)
	}
	sameContents(t, before)
	if _, err := os.Stat(pathsOf(home).ConnectDir()); err == nil {
		t.Error("retain state left aside")
	}
}

// connectedFromOwnServer: home had its own server and joined the host's.
// It returns ownFiles' contents and the state before connect.
func connectedFromOwnServer(t *testing.T, home string) (map[string]string, state.State) {
	t.Helper()
	ownServerPC(t, home)
	before := contentsOf(t, ownFiles(home))
	st, _ := state.Load(pathsOf(home).StateFile())
	tok := joinableHost(t)
	noKeyRequired()
	withBackupDocker(t)
	if code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--answer", "backfill=no", "--non-interactive"); code != output.OK {
		t.Fatalf("connect: exit %d\n%s%s", code, out, errOut)
	}
	return before, st
}

// connect --undo points the PC back at its own server: config, key and
// sends as they were before connect, the binding gone, the server
// started; the sends to the shared server are kept aside.
func TestConnectUndo(t *testing.T) {
	home, f := setupMachine(t)
	d := pathsOf(home)
	before, stBefore := connectedFromOwnServer(t, home)
	_ = os.MkdirAll(d.CursorsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.CursorsDir(), "s2.json"), []byte(`{"offset":7}`), 0o600) // sent to the shared server
	f.calls = nil
	code, out, errOut := run(t, "connect", "--undo", "--json")
	env := parseConnect(t, out)
	if code != output.OK || env.Data.Plan || !env.Data.RestartRequired {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	sameContents(t, before)
	if _, err := os.Stat(filepath.Join(d.CursorsDir(), "s2.json")); err == nil {
		t.Error("the shared server's cursor stayed")
	}
	st, _ := state.Load(d.StateFile())
	if st.RemoteServer != nil || st.RetainSince == nil || !st.RetainSince.Equal(*stBefore.RetainSince) || st.RestoreEpoch == nil || st.RestoreEpoch.Epoch != 2 {
		t.Errorf("state: binding %+v, since %v, epoch %+v", st.RemoteServer, st.RetainSince, st.RestoreEpoch)
	}
	if cmds := f.commands(); !slices.Contains(cmds, "server start") {
		t.Errorf("ran %v", cmds)
	}
	shared, _ := filepath.Glob(filepath.Join(d.ConnectDir(), "*-shared", "cursors", "s2.json"))
	if len(shared) != 1 {
		t.Errorf("the shared server's sends not kept: %v", shared)
	}
	// Done once: the pre-image is used up.
	if code, out, errOut := run(t, "connect", "--undo"); code != output.Usage {
		t.Errorf("again: exit %d\n%s%s", code, out, errOut)
	}
}

// A server start that fails after the switch exits 40 with the command
// that finishes it; the switch stays done.
func TestConnectUndoSaysToStartTheServer(t *testing.T) {
	home, f := setupMachine(t)
	connectedFromOwnServer(t, home)
	f.fail["server start"] = &output.Error{Code: output.Blocked, Message: "port taken"}
	code, out, errOut := run(t, "connect", "--undo", "--json")
	if env := parseConnect(t, out); code != output.Partial || env.Error == nil || env.Error.Resume != "backsight server start" || !strings.Contains(env.Error.Message, "port taken") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	d := pathsOf(home)
	if cfg, _ := config.Load(d.ConfigFile(), d, home); cfg.Server.Mode != "local" {
		t.Errorf("config mode %q", cfg.Server.Mode)
	}
}

// Without a server of its own before connect, there is nothing to go back
// to: a new client, and one that switched from another shared server.
func TestConnectUndoWithoutOwnServerRefused(t *testing.T) {
	home, _ := setupMachine(t)
	tok := joinableHost(t)
	switchingClient(t, home)
	if code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--answer", "backfill=no", "--non-interactive"); code != output.OK {
		t.Fatalf("connect: exit %d\n%s%s", code, out, errOut)
	}
	before := treeOf(t, home)
	code, out, errOut := run(t, "connect", "--undo")
	if code != output.Usage || !strings.Contains(errOut, "this PC had no server of its own before connect: backsight uninstall removes the client") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	sameTree(t, before, treeOf(t, home))
	if code, _, errOut := run(t, "connect", "--undo", "--token-file", tokenFile(t, tok)); code != output.Usage || !strings.Contains(errOut, "--undo") {
		t.Errorf("--undo with a token: exit %d\n%s", code, errOut)
	}
}

// connect --undo --plan says what it would put back and changes nothing.
func TestConnectUndoPlanChangesNothing(t *testing.T) {
	home, f := setupMachine(t)
	connectedFromOwnServer(t, home)
	f.calls = nil
	before := treeOf(t, home)
	code, out, errOut := run(t, "connect", "--undo", "--plan", "--json")
	env := parseConnect(t, out)
	if code != output.OK || !env.Data.Plan || !hasConnectStep(env.Data.Steps, "config", "planned", "as they were before connect") || !hasConnectStep(env.Data.Steps, "server", "planned", "backsight server start") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if code, out, _ := run(t, "connect", "--undo", "--plan"); code != output.OK || !strings.Contains(out, "connect --undo will:") {
		t.Errorf("text: exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, home))
	if len(f.calls) != 0 {
		t.Errorf("ran %v", f.commands())
	}
}
