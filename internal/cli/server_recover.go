package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// A server exposed on a private address publishes its API there and on
// 127.0.0.1 from one container. When Docker starts it before the address
// exists (Tailscale or DHCP still coming up at boot), the start fails and
// Docker's restart policy does not retry it: the host's own memory stays
// down. jobs tick and the SessionStart hook start it again once the
// address is there.
const (
	// serverCheckEvery: SessionStart looks at most this often, per machine.
	serverCheckEvery = 5 * time.Minute
	// serverCheckStamp, in the state directory, holds when it last looked.
	serverCheckStamp = "server-start-check"
	// serverCheckBudget bounds SessionStart's look (a Docker call), within
	// the hook's deadline; jobs tick has serverCheckTimeout.
	serverCheckBudget  = 1500 * time.Millisecond
	serverCheckTimeout = 30 * time.Second
)

// spawnLogged starts `backsight <args>` detached, its output going to out;
// tests replace it.
var spawnLogged = startDetachedTo

// exposureNeedsStart reports whether this machine's server, exposed on a
// private address, was left down by that address and can start now: a
// local, deployed server whose database runs and API server does not
// (`server stop` stops the database too: a server stopped by hand stays
// stopped), and whose bind_ip is now an address of this machine. why says
// which condition decided.
func exposureNeedsStart(ctx context.Context, cfg config.Config, rt server.Runtime) (start bool, why string) {
	switch {
	case cfg.Server.Mode != "local":
		return false, "the server is on another machine"
	case cfg.Network.Exposure != "private":
		return false, "the server listens on loopback only"
	}
	if v, err := rt.Deployed(); err != nil || v == "" {
		return false, "the server is not installed on this machine"
	}
	ip := cfg.Network.BindIP
	here, err := addressHere(net.ParseIP(ip))
	switch {
	case err != nil:
		return false, "cannot list this machine's addresses: " + err.Error()
	case !here:
		return false, "waiting for " + ip + " to appear on this machine"
	}
	list, err := rt.Status(ctx)
	if err != nil {
		return false, "cannot read the server's state: " + err.Error()
	}
	db := false
	for _, c := range list {
		switch {
		case c.Service == "hindsight" && (c.State == "running" || c.State == "restarting"):
			return false, "the server runs"
		case c.Service == "db" && c.State == "running":
			db = true
		}
	}
	if !db {
		return false, "the server was stopped"
	}
	return true, ip + " is on this machine and the server is not running"
}

// addressHere reports whether ip is an address of this machine.
func addressHere(ip net.IP) (bool, error) {
	if ip == nil {
		return false, nil
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		if x := addrIP(a); x != nil && x.Equal(ip) {
			return true, nil
		}
	}
	return false, nil
}

// spawnServerStart starts `backsight server start` detached, its output
// appended to the logs directory's server-start.log; it never waits.
// server start takes the server lock and refuses during maintenance or a
// pending restore: a start then simply exits.
func spawnServerStart(d paths.Dirs) error {
	dir := filepath.Join(d.State, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "server-start.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s: starting the server, its private address is on this machine\n", now().UTC().Format(time.RFC3339))
	return spawnLogged(f, "server", "start")
}

// startLateServer spawns `server start` when exposureNeedsStart says so,
// and says what it did ("": nothing to do). Never while a restore did not
// finish (server start would resume it with nobody there) or while a
// command holds server.lock (it is restarting the server itself).
func startLateServer(ctx context.Context, d paths.Dirs, cfg config.Config) string {
	if state.RestoreIncomplete(d) {
		return ""
	}
	if h := state.Inspect(filepath.Join(d.LocksDir(), "server.lock")); h != nil && !h.Stale {
		return ""
	}
	start, why := exposureNeedsStart(ctx, cfg, runtimeFor(cfg))
	if !start {
		return ""
	}
	if err := spawnServerStart(d); err != nil {
		return "server start failed to spawn: " + err.Error()
	}
	return "server start spawned (" + why + ")"
}

// startLateServer, in SessionStart: at most every serverCheckEvery per
// machine, within serverCheckBudget, printing nothing. The stamp is
// written before the look, so a slow Docker is asked once per period.
func (h *hookEnv) startLateServer() {
	if h.cfg.Server.Mode != "local" || h.cfg.Network.Exposure != "private" || h.maintenance() {
		return
	}
	stamp := filepath.Join(h.dirs.State, serverCheckStamp)
	if raw, err := os.ReadFile(stamp); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw))); err == nil {
			if age := now().Sub(t); age >= 0 && age < serverCheckEvery {
				return
			}
		}
	}
	if err := os.MkdirAll(h.dirs.State, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(stamp, []byte(now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, serverCheckBudget)
	defer cancel()
	if got := startLateServer(ctx, h.dirs, h.cfg); got != "" {
		h.tw.Emit(telemetry.Raw{Name: "server_start", Fields: map[string]any{"result": got}})
	}
}

// exposureDetail is doctor's word on a server of this machine exposed on a
// private address that does not answer: why, and the fix when it is not
// `server start` ("" when it is not such a server).
func exposureDetail(d paths.Dirs, home string) (detail, fix string) {
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil || cfg.Server.Mode != "local" || cfg.Network.Exposure != "private" {
		return "", ""
	}
	ip := cfg.Network.BindIP
	here, err := addressHere(net.ParseIP(ip))
	switch {
	case err != nil:
		return "", ""
	case !here:
		return "the private address " + ip + " is not on this machine yet: the server starts once it appears (jobs tick, or the next Claude Code session); " +
				"to stop depending on it: backsight server expose --loopback",
			"bring " + ip + " up (start Tailscale, or connect this machine to its network), or run backsight server expose --loopback"
	}
	return "the server is exposed on " + ip + ", an address of this machine, and does not answer: start it with backsight server start", ""
}
