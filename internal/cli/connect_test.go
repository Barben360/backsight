package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/token"
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

// asClientOf makes home a client of the server at url, bank backsight, on
// API key key.
func asClientOf(t *testing.T, home, url, key string) {
	t.Helper()
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	if err := os.WriteFile(d.ConfigFile(), []byte("# my client\nschema_version = 1\n\n[server]\nmode = \"remote\"\nurl = \""+url+"\"\nbank = \"backsight\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("api_key", key) }); err != nil {
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
	asClientOf(t, home, "http://100.101.1.2:8888", "the-old-key-0123456789abcdef")
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
	asClientOf(t, home, oldURL, "the-old-key-0123456789abcdef")
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

	tok := joinableHost(t) // the new server: holds nothing of s1
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--answer", "backfill=no", "--non-interactive", "--json")
	if env := parseConnect(t, out); code != output.OK || env.Data.Situation != joinOther || !hasConnectStep(env.Data.Steps, "history", "done", "start over") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	runs, _ := filepath.Glob(filepath.Join(d.ConnectDir(), "*"))
	if len(runs) != 1 {
		t.Fatalf("connect runs kept: %v", runs)
	}
	for _, p := range []string{"cursors", "sent-index"} {
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
	asClientOf(t, home, tok.URL, "the-old-key-0123456789abcdef")
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
// there, and on a PC with its own server (the next release step).
func TestConnectRefusesAServerOnlyHostAndAnOwnServer(t *testing.T) {
	home := isolate(t)
	tok := joinableHost(t)
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	for cfg, want := range map[string]string{
		"[server]\nrole = \"server\"\n":               "server-only host",
		"[server]\nurl = \"http://127.0.0.1:8888\"\n": "this PC runs its own server",
	} {
		_ = os.WriteFile(d.ConfigFile(), []byte(cfg), 0o600)
		before := treeOf(t, home)
		if code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes"); code != output.Usage || !strings.Contains(errOut, want) {
			t.Errorf("%s: exit %d\n%s%s", want, code, out, errOut)
		}
		sameTree(t, before, treeOf(t, home))
	}
}
