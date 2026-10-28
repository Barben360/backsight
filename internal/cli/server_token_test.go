package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/token"
)

// keyDocker plays the server's compose project for a key rotation: each
// `up` notes the API key the .env gives the server, which the fake server
// then runs on, unless only is set: then it runs on that key whatever .env
// says (health fails on any other).
type keyDocker struct {
	mu   sync.Mutex
	keys []string // HINDSIGHT_API_TENANT_API_KEY of each compose up
	only string
	echo bool // with only: an up on another key fails, its output quoting that key
}

func (f *keyDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(args, " ")
	switch {
	case strings.HasSuffix(line, "up -d --wait"):
		k := upKey(args)
		f.keys = append(f.keys, k)
		if f.echo && f.only != "" && k != f.only {
			return []byte("Error: HINDSIGHT_API_TENANT_API_KEY=" + k + " refused"), errors.New("exit status 1")
		}
		if f.only != "" {
			k = f.only
		}
		acceptedKey.Store(k)
	case strings.Contains(line, " ps"):
		return []byte(`{"Service":"db","State":"running","Health":"healthy"}` + "\n" + `{"Service":"hindsight","State":"running","Health":""}`), nil
	}
	return nil, nil
}

// tokenHost is a dev install exposed on 192.168.1.10, with a fake server
// that checks the API key and a fake Docker; stdout is not a terminal.
func tokenHost(t *testing.T) (home string, port int, dk *keyDocker) {
	t.Helper()
	home = devInstall(t, true)
	port = fakeHindsight(t)
	cfg := "[server]\nurl = \"http://127.0.0.1:" + strconv.Itoa(port) + "\"\nui_port = 19999\nbank = \"" + config.DefaultBank + "\"\n\n" +
		"[llm]\nmodel = \"test-model\"\nprice_input = 0.25\ncurrency = \"EUR\"\n\n" +
		"[network]\nexposure = \"private\"\nbind_ip = \"192.168.1.10\"\n"
	if err := os.WriteFile(filepath.Join(home, "backsight", "config", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	sec, _ := config.LoadSecrets(secretsFile(home))
	requireKey.Store(true)
	acceptedKey.Store(sec.APIKey)
	dk = &keyDocker{}
	old, oldPoll, oldTimeout, oldTerm := composeRunner, healthPoll, healthTimeout, stdoutIsTerminal
	composeRunner, healthPoll, healthTimeout = dk, 5*time.Millisecond, 300*time.Millisecond
	stdoutIsTerminal = func() bool { return false }
	t.Cleanup(func() { composeRunner, healthPoll, healthTimeout, stdoutIsTerminal = old, oldPoll, oldTimeout, oldTerm })
	return home, port, dk
}

func readToken(t *testing.T, path string) token.Token {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := token.Decode(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func serverIDOf(t *testing.T, port int, key string) string {
	t.Helper()
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), key, 5*time.Second)
	id, err := retain.ReadServerID(context.Background(), cl, config.DefaultBank)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTokenRefusesPipedStdout(t *testing.T) {
	home, _, _ := tokenHost(t)
	sec, _ := config.LoadSecrets(secretsFile(home))
	code, out, errOut := run(t, "server", "token")
	if code != output.Usage || out != "" || !strings.Contains(errOut, "--out FILE") || strings.Contains(errOut, sec.APIKey) {
		t.Errorf("exit %d\nstdout %q\nstderr %s", code, out, errOut)
	}
	code, out, _ = run(t, "server", "token", "--json")
	if code != output.Usage || strings.Contains(out, sec.APIKey) || strings.Contains(out, token.Prefix) {
		t.Errorf("--json without --out: exit %d\n%s", code, out)
	}
}

func TestTokenPrintsToATerminal(t *testing.T) {
	home, _, _ := tokenHost(t)
	stdoutIsTerminal = func() bool { return true }
	code, out, errOut := run(t, "server", "token")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	sec, _ := config.LoadSecrets(secretsFile(home))
	first, _, _ := strings.Cut(out, "\n")
	if tok, err := token.Decode(first); err != nil || tok.APIKey != sec.APIKey {
		t.Errorf("first line is not the token: %v\n%s", err, out)
	}
}

func TestTokenWritesFile0600(t *testing.T) {
	home, port, _ := tokenHost(t)
	f := filepath.Join(t.TempDir(), "join.token")
	if err := os.WriteFile(f, []byte("an older token"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "server", "token", "--out", f)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	sec, _ := config.LoadSecrets(secretsFile(home))
	if strings.Contains(out+errOut, sec.APIKey) || strings.Contains(out+errOut, token.Prefix) {
		t.Errorf("the key reached the output:\n%s%s", out, errOut)
	}
	if fi, err := os.Stat(f); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v", fi.Mode(), err)
	}
	tok := readToken(t, f)
	want := token.Token{URL: "http://192.168.1.10:" + strconv.Itoa(port), Bank: config.DefaultBank, APIKey: sec.APIKey,
		UIURL: "http://192.168.1.10:19999", HindsightVersion: "1.1.0", ServerID: serverIDOf(t, port, sec.APIKey),
		LLM: token.LLM{Provider: "deepseek", Model: "test-model", Currency: "EUR", PriceInput: 0.25}}
	if tok.ServerID == "" || token.Encode(tok) != token.Encode(want) {
		t.Errorf("token url %s bank %s ui %s version %s server %s llm %+v (key equal: %v)",
			tok.URL, tok.Bank, tok.UIURL, tok.HindsightVersion, tok.ServerID, tok.LLM, tok.APIKey == sec.APIKey)
	}
	if !strings.Contains(out, f) {
		t.Errorf("text does not say where:\n%s", out)
	}
}

func TestTokenJSONNeverCarriesKey(t *testing.T) {
	home, port, _ := tokenHost(t)
	f := filepath.Join(t.TempDir(), "join.token")
	code, out, errOut := run(t, "server", "token", "--json", "--out", f)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	sec, _ := config.LoadSecrets(secretsFile(home))
	if strings.Contains(out+errOut, sec.APIKey) || strings.Contains(out+errOut, token.Prefix) {
		t.Errorf("the key reached the output:\n%s", out)
	}
	var env struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"url": "http://192.168.1.10:" + strconv.Itoa(port), "bank": config.DefaultBank,
		"server_id": serverIDOf(t, port, sec.APIKey), "written": f}
	if len(env.Data) != len(want) {
		t.Errorf("data %v, want %v", env.Data, want)
	}
	for k, v := range want {
		if env.Data[k] != v {
			t.Errorf("data.%s = %q, want %q", k, env.Data[k], v)
		}
	}
}

func TestTokenLoopbackNamesExpose(t *testing.T) {
	home, _, _ := tokenHost(t)
	pointAt(t, home, fakeHindsight(t)) // loopback exposure
	f := filepath.Join(t.TempDir(), "join.token")
	code, _, errOut := run(t, "server", "token", "--out", f)
	if code != output.Usage || !strings.Contains(errOut, "backsight server expose --private <ip>") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(f); err == nil {
		t.Error("a token was written")
	}
}

func TestTokenRemoteModeRefused(t *testing.T) {
	home, _, _ := tokenHost(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	_ = os.WriteFile(cfg, []byte("[server]\nmode = \"remote\"\nurl = \"https://mem.example\"\n"), 0o600)
	f := filepath.Join(t.TempDir(), "join.token")
	for _, args := range [][]string{{"server", "token", "--out", f}, {"server", "token", "--rotate", "--out", f}} {
		if code, _, errOut := run(t, args...); code != output.Usage || !strings.Contains(errOut, "local server") {
			t.Errorf("%v: exit %d: %s", args, code, errOut)
		}
	}
	if _, err := os.Stat(f); err == nil {
		t.Error("a token was written")
	}
}

func TestRotateNewKeyWorks(t *testing.T) {
	home, port, dk := tokenHost(t)
	old, _ := config.LoadSecrets(secretsFile(home))
	f := filepath.Join(t.TempDir(), "join.token")
	code, out, errOut := run(t, "server", "token", "--rotate", "--out", f)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	now, _ := config.LoadSecrets(secretsFile(home))
	tok := readToken(t, f)
	if now.APIKey == old.APIKey || tok.APIKey != now.APIKey {
		t.Errorf("secrets.toml kept the old key or the token does not carry the new one")
	}
	if now.DBPassword != old.DBPassword || now.UIKey != old.UIKey || now.LLMAPIKey != old.LLMAPIKey {
		t.Error("the rotation changed another secret")
	}
	if len(dk.keys) != 1 || dk.keys[0] != now.APIKey {
		t.Errorf("the server was not started on the new key (%d starts)", len(dk.keys))
	}
	if tok.ServerID == "" || tok.ServerID != serverIDOf(t, port, now.APIKey) {
		t.Errorf("server id %q", tok.ServerID)
	}
	if strings.Contains(out+errOut, old.APIKey) || strings.Contains(out+errOut, now.APIKey) || !strings.Contains(out, "rotated") {
		t.Errorf("output:\n%s%s", out, errOut)
	}
}

func TestRotateRollsBack(t *testing.T) {
	home, _, dk := tokenHost(t)
	old, _ := config.LoadSecrets(secretsFile(home))
	dk.only = old.APIKey // the server never runs healthy on a new key
	f := filepath.Join(t.TempDir(), "join.token")
	code, out, errOut := run(t, "server", "token", "--rotate", "--out", f)
	if code != output.Blocked || !strings.Contains(errOut, "the old token still works") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	now, _ := config.LoadSecrets(secretsFile(home))
	if now != old {
		t.Error("secrets.toml does not hold the old secrets again")
	}
	if len(dk.keys) != 2 || dk.keys[0] == old.APIKey || dk.keys[1] != old.APIKey {
		t.Errorf("want a start on a new key then one on the old key, got %d starts (last on the old key: %v)",
			len(dk.keys), len(dk.keys) > 0 && dk.keys[len(dk.keys)-1] == old.APIKey)
	}
	if strings.Contains(out+errOut, old.APIKey) || len(dk.keys) > 0 && strings.Contains(out+errOut, dk.keys[0]) {
		t.Error("a key reached the output")
	}
	if _, err := os.Stat(f); err == nil {
		t.Error("a token was written")
	}
}

// The old key cannot be put back on the server either: the message names
// that state, and the exit is blocked.
func TestRotateFailedRollbackSaysSo(t *testing.T) {
	home, _, dk := tokenHost(t)
	old, _ := config.LoadSecrets(secretsFile(home))
	dk.only = "neither-key" // the server never runs healthy again
	code, out, errOut := run(t, "server", "token", "--rotate", "--out", filepath.Join(t.TempDir(), "join.token"))
	if code != output.Blocked || strings.Contains(errOut, "the old token still works") ||
		!strings.Contains(errOut, "did not start healthy on it") || !strings.Contains(errOut, "backsight server start") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if now, _ := config.LoadSecrets(secretsFile(home)); now.APIKey != old.APIKey {
		t.Error("secrets.toml does not hold the old key again")
	}
}

// Docker output quoting a key, on the new key and then on the old one:
// both are masked in the message.
func TestRotateMasksBothKeys(t *testing.T) {
	home, _, dk := tokenHost(t)
	old, _ := config.LoadSecrets(secretsFile(home))
	dk.only, dk.echo = "neither-key", true
	code, out, errOut := run(t, "server", "token", "--rotate", "--json", "--out", filepath.Join(t.TempDir(), "join.token"))
	if code != output.Blocked || len(dk.keys) != 2 || !strings.Contains(out, "[redacted]") {
		t.Fatalf("exit %d, %d starts\n%s%s", code, len(dk.keys), out, errOut)
	}
	for i, k := range []string{dk.keys[0], old.APIKey} {
		if strings.Contains(out+errOut, k) {
			t.Errorf("key %d reached the output:\n%s%s", i, out, errOut)
		}
	}
}
