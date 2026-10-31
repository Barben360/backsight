package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/testtool"
	"github.com/Barben360/backsight/internal/token"
)

// exposeDocker plays the server's compose project for server expose: each
// `up` notes the compose.yaml it runs, and the fake server then runs on the
// key its .env gives, unless that compose.yaml holds failOn: then the
// server never answers on it (health fails).
type exposeDocker struct {
	mu     sync.Mutex
	ups    []string // the compose.yaml of each compose up
	failOn string
}

func (f *exposeDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(args, " ")
	switch {
	case strings.HasSuffix(line, "up -d --wait"):
		var compose string
		for i, a := range args {
			if a == "--project-directory" && i+1 < len(args) {
				raw, _ := os.ReadFile(filepath.Join(args[i+1], "compose.yaml"))
				compose = string(raw)
			}
		}
		f.ups = append(f.ups, compose)
		if f.failOn != "" && strings.Contains(compose, f.failOn) {
			acceptedKey.Store("no key works")
		} else {
			acceptedKey.Store(upKey(args))
		}
	case strings.Contains(line, " ps"):
		return []byte(`{"Service":"db","State":"running","Health":"healthy"}` + "\n" + `{"Service":"hindsight","State":"running","Health":""}`), nil
	}
	return nil, nil
}

// exposeHost is a dev install on loopback with a fake server that checks
// the API key and a fake Docker; the machine's addresses are 127.0.0.1,
// 192.168.1.10 (a LAN), 100.101.1.2 and fd7a:115c:a1e0::5 (a tailnet),
// and 203.0.113.7 (public).
// dialed lists the private addresses the command checked; a dial fails
// while dialFails is set. stdout is not a terminal.
type exposeFixture struct {
	home, cfgPath string
	port          int
	dk            *exposeDocker
	dialed        []string
	dialFails     bool
}

func exposeHost(t *testing.T) *exposeFixture {
	t.Helper()
	fx := &exposeFixture{home: devInstall(t, true), port: fakeHindsight(t), dk: &exposeDocker{}}
	fx.cfgPath = filepath.Join(fx.home, "backsight", "config", "config.toml")
	fx.writeConfig(t, "")
	sec, _ := config.LoadSecrets(secretsFile(fx.home))
	requireKey.Store(true)
	acceptedKey.Store(sec.APIKey)
	old, oldPoll, oldTimeout, oldTerm, oldAddrs, oldDial := composeRunner, healthPoll, healthTimeout, stdoutIsTerminal, interfaceAddrs, dialPrivate
	composeRunner, healthPoll, healthTimeout = fx.dk, 5*time.Millisecond, 300*time.Millisecond
	stdoutIsTerminal = func() bool { return false }
	interfaceAddrs = func() ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range []string{"127.0.0.1/8", "192.168.1.10/24", "100.101.1.2/32", "fd7a:115c:a1e0::5/128", "203.0.113.7/24"} {
			ip, n, _ := net.ParseCIDR(c)
			out = append(out, &net.IPNet{IP: ip, Mask: n.Mask})
		}
		return out, nil
	}
	dialPrivate = func(_ context.Context, addr string) error {
		fx.dialed = append(fx.dialed, addr)
		if fx.dialFails {
			return errors.New("connection refused")
		}
		return nil
	}
	t.Cleanup(func() {
		composeRunner, healthPoll, healthTimeout, stdoutIsTerminal, interfaceAddrs, dialPrivate = old, oldPoll, oldTimeout, oldTerm, oldAddrs, oldDial
	})
	return fx
}

// writeConfig writes the host's config.toml with network as its
// [network] table ("": none), a comment and an unrelated key around it.
func (fx *exposeFixture) writeConfig(t *testing.T, network string) {
	t.Helper()
	raw := "# my server\n[server]\nurl = \"http://127.0.0.1:" + strconv.Itoa(fx.port) + "\"\nui_port = 19999\nbank = \"" + config.DefaultBank + "\"\n\n" +
		"[llm]\nmodel = \"test-model\" # kept\n"
	if network != "" {
		raw += "\n[network]\n" + network
	}
	if err := os.WriteFile(fx.cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fx *exposeFixture) config(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(fx.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (fx *exposeFixture) apiKey(t *testing.T) string {
	t.Helper()
	sec, _ := config.LoadSecrets(secretsFile(fx.home))
	return sec.APIKey
}

func TestExposeRefusesForeignAddress(t *testing.T) {
	fx := exposeHost(t)
	before := fx.config(t)
	code, out, errOut := run(t, "server", "expose", "--private", "192.168.1.99", "--answer", "cleartext_lan=yes")
	if code != output.Usage || !strings.Contains(errOut, "not an address of this machine") ||
		!strings.Contains(errOut, "192.168.1.10") || !strings.Contains(errOut, "100.101.1.2") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	// A public address is refused by the config's own rule.
	if code, _, errOut := run(t, "server", "expose", "--private", "8.8.8.8", "--answer", "cleartext_lan=yes"); code != output.Usage || !strings.Contains(errOut, "private") {
		t.Errorf("public address: exit %d: %s", code, errOut)
	}
	if string(fx.config(t)) != string(before) || len(fx.dk.ups) != 0 || len(fx.dialed) != 0 {
		t.Errorf("something changed: %d starts, %d dials\n%s", len(fx.dk.ups), len(fx.dialed), fx.config(t))
	}
}

// A public address of this machine (a VPS, a bridged NAS) is refused by the
// config's own rule, before anything changes.
func TestExposeRefusesPublicAddress(t *testing.T) {
	fx := exposeHost(t)
	before := fx.config(t)
	code, out, errOut := run(t, "server", "expose", "--private", "203.0.113.7", "--answer", "cleartext_lan=yes")
	if code != output.Usage || !strings.Contains(errOut, "network.bind_ip") || !strings.Contains(errOut, "nothing was changed") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if string(fx.config(t)) != string(before) || len(fx.dk.ups) != 0 || len(fx.dialed) != 0 {
		t.Errorf("something changed: %d starts, %d dials\n%s", len(fx.dk.ups), len(fx.dialed), fx.config(t))
	}
	if _, err := os.Stat(filepath.Join(composeDir(fx.home), "compose.yaml")); err == nil {
		t.Error("the server's files were rendered")
	}
}

func TestExposeAsksCleartext(t *testing.T) {
	fx := exposeHost(t)
	before := fx.config(t)
	f := filepath.Join(t.TempDir(), "join.token")
	code, out, errOut := run(t, "server", "expose", "--private", "192.168.1.10", "--out", f, "--json")
	if code != output.NeedsUser {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var env struct {
		Error struct {
			Message   string `json:"message"`
			Resume    string `json:"resume"`
			Questions []struct {
				ID string `json:"id"`
			} `json:"questions"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(env.Error.Questions) != 1 || env.Error.Questions[0].ID != "cleartext_lan" ||
		!strings.Contains(env.Error.Message, "the API key travels unencrypted on this network; prefer a tailnet address (Tailscale: 100.x)") ||
		env.Error.Resume != "backsight server expose --private 192.168.1.10 --out "+f+" --json --answer cleartext_lan=yes" {
		t.Errorf("needs_user error: %+v", env.Error)
	}
	if string(fx.config(t)) != string(before) || len(fx.dk.ups) != 0 {
		t.Errorf("something changed: %d starts\n%s", len(fx.dk.ups), fx.config(t))
	}
	for _, a := range []string{"cleartext_lan=no", "consent=yes"} {
		if code, _, errOut := run(t, "server", "expose", "--private", "192.168.1.10", "--answer", a); code != output.Usage {
			t.Errorf("--answer %s: exit %d: %s", a, code, errOut)
		}
	}
	// The answer given, the LAN address is taken.
	if code, out, errOut := run(t, "server", "expose", "--private", "192.168.1.10", "--out", f, "--answer", "cleartext_lan=yes"); code != output.OK {
		t.Fatalf("with the answer: exit %d\n%s%s", code, out, errOut)
	}
	if cfg := fx.config(t); !strings.Contains(string(cfg), "bind_ip = \"192.168.1.10\"") {
		t.Errorf("config.toml:\n%s", cfg)
	}
}

func TestExposeTailnetNeedsNoAnswer(t *testing.T) {
	fx := exposeHost(t)
	f := filepath.Join(t.TempDir(), "join.token")
	code, out, errOut := run(t, "server", "expose", "--private", "100.101.1.2", "--out", f)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	cfg := string(fx.config(t))
	if !strings.Contains(cfg, "[network]\nexposure = \"private\"\nbind_ip = \"100.101.1.2\"\n") || !strings.HasPrefix(cfg, "# my server\n") ||
		!strings.Contains(cfg, "model = \"test-model\" # kept\n") {
		t.Errorf("config.toml:\n%s", cfg)
	}
	api := "100.101.1.2:" + strconv.Itoa(fx.port)
	if len(fx.dk.ups) != 1 || !strings.Contains(fx.dk.ups[0], "\""+api+":8888\"") {
		t.Errorf("want one start binding %s, got %d:\n%s", api, len(fx.dk.ups), strings.Join(fx.dk.ups, "\n"))
	}
	if len(fx.dialed) != 1 || fx.dialed[0] != api {
		t.Errorf("dialed %v, want %s", fx.dialed, api)
	}
	key := fx.apiKey(t)
	if tok := tokenFrom(t, f); tok.URL != "http://"+api || tok.APIKey != key || tok.ServerID == "" || tok.UIURL != "http://100.101.1.2:19999" {
		t.Errorf("token url %s ui %s server %q (key equal: %v)", tok.URL, tok.UIURL, tok.ServerID, tok.APIKey == key)
	}
	if strings.Contains(out+errOut, key) || strings.Contains(out+errOut, token.Prefix) || !strings.Contains(out, f) {
		t.Errorf("output:\n%s%s", out, errOut)
	}
}

// IPv6 tailnet addresses are bracketed in the address dialed and handed out.
func TestExposeTailnetIPv6(t *testing.T) {
	fx := exposeHost(t)
	f := filepath.Join(t.TempDir(), "join.token")
	if code, out, errOut := run(t, "server", "expose", "--private", "fd7a:115c:a1e0::5", "--out", f); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	api := "[fd7a:115c:a1e0::5]:" + strconv.Itoa(fx.port)
	if len(fx.dialed) != 1 || fx.dialed[0] != api || tokenFrom(t, f).URL != "http://"+api {
		t.Errorf("dialed %v, token %s", fx.dialed, tokenFrom(t, f).URL)
	}
}

func TestExposePlanChangesNothing(t *testing.T) {
	fx := exposeHost(t)
	before := fx.config(t)
	code, out, errOut := run(t, "server", "expose", "--private", "100.101.1.2", "--plan")
	if code != output.OK || !strings.Contains(out, "network.exposure = \"private\"") || !strings.Contains(out, "network.bind_ip = \"100.101.1.2\"") ||
		!strings.Contains(out, filepath.Join(composeDir(fx.home), "compose.yaml")+": would be written") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if string(fx.config(t)) != string(before) || len(fx.dk.ups) != 0 || len(fx.dialed) != 0 || len(bankCalls) != 0 {
		t.Errorf("the plan changed something: %d starts, %d dials, %v", len(fx.dk.ups), len(fx.dialed), bankCalls)
	}
	if _, err := os.Stat(filepath.Join(composeDir(fx.home), "compose.yaml")); err == nil {
		t.Error("the server's files were rendered")
	}
}

// The server does not run healthy exposed, or does not answer on the
// private address: config.toml is put back byte for byte and the server
// started again as it was.
func TestExposeRollsBack(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(fx *exposeFixture)
	}{
		{"unhealthy", func(fx *exposeFixture) { fx.dk.failOn = "100.101.1.2" }},
		{"private address unreachable", func(fx *exposeFixture) { fx.dialFails = true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := exposeHost(t)
			w := watchUps(fx.home)
			c.fail(fx)
			before := fx.config(t)
			f := filepath.Join(t.TempDir(), "join.token")
			code, out, errOut := run(t, "server", "expose", "--private", "100.101.1.2", "--out", f)
			// Started again on the old config, whatever it was before (a
			// stopped server runs now): never "as before".
			if code != output.RolledBack || !strings.Contains(errOut, "config.toml is back as it was and the server started again on it") || strings.Contains(errOut, "as before") {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			// Hooks queue and jobs skip meanwhile; a Ctrl-C cannot kill the rollback.
			w.guarded(t, 2)
			if after := fx.config(t); string(after) != string(before) {
				t.Errorf("config.toml changed:\n%s", after)
			}
			if len(fx.dk.ups) != 2 || !strings.Contains(fx.dk.ups[0], "100.101.1.2") || strings.Contains(fx.dk.ups[1], "100.101.1.2") {
				t.Errorf("want a start exposed then one on loopback only, got %d:\n%s", len(fx.dk.ups), strings.Join(fx.dk.ups, "\n"))
			}
			if strings.Contains(out+errOut, fx.apiKey(t)) {
				t.Error("the key reached the output")
			}
			if _, err := os.Stat(f); err == nil {
				t.Error("a token was written")
			}
		})
	}
}

func TestExposeLoopbackRemovesBindIP(t *testing.T) {
	fx := exposeHost(t)
	fx.writeConfig(t, "exposure = \"private\"\nbind_ip = \"192.168.1.10\"\nbreaker_failures = 4\n")
	code, out, errOut := run(t, "server", "expose", "--loopback")
	if code != output.OK || !strings.Contains(out, "no longer") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	cfg := string(fx.config(t))
	if !strings.HasSuffix(cfg, "[network]\nexposure = \"loopback\"\nbreaker_failures = 4\n") || !strings.HasPrefix(cfg, "# my server\n") {
		t.Errorf("config.toml:\n%s", cfg)
	}
	if len(fx.dk.ups) != 1 || strings.Contains(fx.dk.ups[0], "192.168.1.10") || !strings.Contains(fx.dk.ups[0], "\"127.0.0.1:"+strconv.Itoa(fx.port)+":8888\"") {
		t.Errorf("want one start on loopback only, got %d:\n%s", len(fx.dk.ups), strings.Join(fx.dk.ups, "\n"))
	}
	if len(fx.dialed) != 0 {
		t.Errorf("dialed %v", fx.dialed)
	}
	if code, _, errOut := run(t, "server", "expose", "--loopback", "--out", filepath.Join(t.TempDir(), "t")); code != output.Usage || !strings.Contains(errOut, "--out") {
		t.Errorf("--loopback --out: exit %d: %s", code, errOut)
	}
}

// The host's own client keeps server.url on 127.0.0.1: a private exposure
// publishes loopback too.
func TestHostStillHealthyOnLoopback(t *testing.T) {
	fx := exposeHost(t)
	d, home, _ := machine()
	was, err := config.Load(fx.cfgPath, d, home)
	if err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run(t, "server", "expose", "--private", "100.101.1.2", "--out", filepath.Join(t.TempDir(), "join.token")); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	compose, _ := os.ReadFile(filepath.Join(composeDir(fx.home), "compose.yaml"))
	if !strings.Contains(string(compose), "\"127.0.0.1:"+strconv.Itoa(fx.port)+":8888\"") || !strings.Contains(string(compose), "\"100.101.1.2:"+strconv.Itoa(fx.port)+":8888\"") {
		t.Errorf("compose.yaml:\n%s", compose)
	}
	now, err := config.Load(fx.cfgPath, d, home)
	if err != nil || now.Server.URL != was.Server.URL {
		t.Errorf("server.url %q, was %q (%v)", now.Server.URL, was.Server.URL, err)
	}
}

// With no --out and no terminal, the server is still exposed: the output
// says how to get the token, and never holds it.
func TestExposePipedSaysHowToGetTheToken(t *testing.T) {
	fx := exposeHost(t)
	code, out, errOut := run(t, "server", "expose", "--private", "100.101.1.2", "--json")
	if code != output.OK || !strings.Contains(out, "backsight server token --out FILE") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if strings.Contains(out+errOut, fx.apiKey(t)) || strings.Contains(out+errOut, token.Prefix) {
		t.Errorf("the token reached the output:\n%s", out)
	}
	if !strings.Contains(string(fx.config(t)), "bind_ip = \"100.101.1.2\"") || len(fx.dk.ups) != 1 {
		t.Errorf("not exposed: %d starts\n%s", len(fx.dk.ups), fx.config(t))
	}
}

func TestExposeFlags(t *testing.T) {
	fx := exposeHost(t)
	for _, args := range [][]string{{}, {"--private", "100.101.1.2", "--loopback"}} {
		if code, _, errOut := run(t, append([]string{"server", "expose"}, args...)...); code != output.Usage || !strings.Contains(errOut, "--private") {
			t.Errorf("%v: exit %d: %s", args, code, errOut)
		}
	}
	_ = os.WriteFile(fx.cfgPath, []byte("[server]\nmode = \"remote\"\nurl = \"https://mem.example\"\n"), 0o600)
	if code, _, errOut := run(t, "server", "expose", "--loopback"); code != output.Usage || !strings.Contains(errOut, "backsight server expose") {
		t.Errorf("remote mode: exit %d: %s", code, errOut)
	}
	if len(fx.dk.ups) != 0 {
		t.Errorf("%d starts", len(fx.dk.ups))
	}
}

// readOnlyAtFirstUp makes dir read-only (no file can be written into it)
// once the command starts the server the first time, then runs inner.
type readOnlyAtFirstUp struct {
	t     *testing.T
	inner server.Runner
	dir   string
	done  bool
}

func (r *readOnlyAtFirstUp) Run(ctx context.Context, args ...string) ([]byte, error) {
	if !r.done && strings.HasSuffix(strings.Join(args, " "), "up -d --wait") {
		r.done = true
		testtool.DenyWrite(r.t, r.dir)
	}
	return r.inner.Run(ctx, args...)
}

// configLockedAtFirstUp: the config directory of home, where config.toml
// and secrets.toml are, cannot be written once the server first starts.
func configLockedAtFirstUp(t *testing.T, home string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := filepath.Join(home, "backsight", "config")
	old := composeRunner
	composeRunner = &readOnlyAtFirstUp{t: t, inner: old, dir: dir}
	t.Cleanup(func() { composeRunner = old })
}

// The new exposure does not run, and config.toml cannot be put back:
// exit 20, saying it still sets the new exposure (which it does), and to
// expose again; the server is not started on a config it does not hold,
// and no key is printed.
func TestExposeSaysWhenConfigCannotBePutBack(t *testing.T) {
	fx := exposeHost(t)
	fx.dk.failOn = "100.101.1.2"
	configLockedAtFirstUp(t, fx.home)
	f := filepath.Join(t.TempDir(), "join.token")
	code, out, errOut := run(t, "server", "expose", "--private", "100.101.1.2", "--out", f)
	if code != output.Blocked || !strings.Contains(errOut, "the server did not run on 100.101.1.2 (") ||
		!strings.Contains(errOut, "and putting "+fx.cfgPath+" back failed (") ||
		!strings.Contains(errOut, `it still sets network.exposure = "private", network.bind_ip = "100.101.1.2"; run backsight server expose again with the exposure you had, then backsight server status`) ||
		strings.Contains(errOut, "is back as it was") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if cfg := string(fx.config(t)); !strings.Contains(cfg, `bind_ip = "100.101.1.2"`) {
		t.Errorf("config.toml does not set what the message says:\n%s", cfg)
	}
	if len(fx.dk.ups) != 1 {
		t.Errorf("%d starts, want the exposed one only", len(fx.dk.ups))
	}
	if strings.Contains(out+errOut, fx.apiKey(t)) {
		t.Error("the key reached the output")
	}
	if _, err := os.Stat(f); err == nil {
		t.Error("a token was written")
	}
}

// The server does not start healthy on the old exposure either: the
// message says config.toml is back but the server is not, and what to run.
func TestExposeFailedRollbackSaysSo(t *testing.T) {
	fx := exposeHost(t)
	fx.dk.failOn = "127.0.0.1" // every compose up
	before := fx.config(t)
	code, out, errOut := run(t, "server", "expose", "--private", "100.101.1.2", "--out", filepath.Join(t.TempDir(), "join.token"))
	if code != output.Blocked || strings.Contains(errOut, "the server runs as before") ||
		!strings.Contains(errOut, "did not start healthy on it") || !strings.Contains(errOut, "backsight server start") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if after := fx.config(t); string(after) != string(before) || len(fx.dk.ups) != 2 {
		t.Errorf("%d starts, config.toml:\n%s", len(fx.dk.ups), after)
	}
}

// --out naming a symlink: refused before anything changes, exit 2; the
// link's target, config.toml and the server as they were.
func TestExposeRefusesASymlinkOut(t *testing.T) {
	fx := exposeHost(t)
	before := fx.config(t)
	dir := t.TempDir()
	victim := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(victim, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "join.token")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	code, out, errOut := run(t, "server", "expose", "--private", "100.101.1.2", "--out", link)
	if code != output.Usage || !strings.Contains(errOut, "nothing was written") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if raw, _ := os.ReadFile(victim); string(raw) != "mine\n" {
		t.Errorf("the link's target was written (%d bytes)", len(raw))
	}
	if !bytes.Equal(fx.config(t), before) || len(fx.dk.ups) != 0 {
		t.Errorf("changed: config %v, %d starts", !bytes.Equal(fx.config(t), before), len(fx.dk.ups))
	}
}

// The native runtime serves this PC only (loopback, 0078's non-goal):
// expose --private refuses before anything is written, naming the way
// to private exposure; config.toml, server.env and both units stay
// byte-identical and the units are left alone.
func TestServerExposeRefusedOnNative(t *testing.T) {
	testtool.SkipWindows(t, "Task 9: native runtime")
	home, n, _, sys, _ := nativeHost(t)
	files := []string{pathsOf(home).ConfigFile(), n.EnvFile(), unitPath(home, n.DBUnit()), unitPath(home, n.ServerUnit())}
	before := map[string]string{}
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = string(raw)
	}
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("100.101.1.2"), Mask: net.CIDRMask(32, 32)}}, nil
	}
	t.Cleanup(func() { interfaceAddrs = old })
	sys.calls = nil
	token := filepath.Join(t.TempDir(), "join.token")
	code, out, errOut := run(t, "server", "expose", "--private", "100.101.1.2", "--out", token)
	want := "the native runtime serves this PC only (loopback) in this release: private exposure needs the docker runtime (backsight server runtime switch docker)"
	if code != output.Blocked || !strings.Contains(errOut, want) {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	for _, p := range files {
		if raw, _ := os.ReadFile(p); string(raw) != before[p] {
			t.Errorf("%s changed:\n%s", p, raw)
		}
	}
	if len(sys.calls) != 0 || onDisk(token) {
		t.Errorf("acted: %q, token written %v", sys.calls, onDisk(token))
	}
	// --loopback stays open on native: it is what native serves.
	if code, out, errOut := run(t, "server", "expose", "--loopback", "--plan"); code != output.OK {
		t.Errorf("--loopback on native: exit %d\n%s%s", code, out, errOut)
	}
}
