package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
)

// Seams replaced by tests.
var (
	// interfaceAddrs lists this machine's addresses.
	interfaceAddrs = net.InterfaceAddrs
	// dialPrivate checks that something accepts connections at addr.
	dialPrivate = func(ctx context.Context, addr string) error {
		c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
)

// cleartextWarning is why a LAN address needs --answer cleartext_lan=yes.
const cleartextWarning = "the API key travels unencrypted on this network; prefer a tailnet address (Tailscale: 100.x)"

// tokenElsewhere is what expose says when the token cannot go to this
// output.
const tokenElsewhere = "backsight server token --out FILE gives it"

// exposeResult is what --json says: never the token, which holds the key.
type exposeResult struct {
	Exposure string `json:"exposure"`
	BindIP   string `json:"bind_ip,omitempty"`
	// URL is the API address other PCs use, on a private exposure.
	URL           string   `json:"url,omitempty"`
	Plan          bool     `json:"plan,omitempty"`
	ConfigFile    string   `json:"config_file"`
	ConfigChanged bool     `json:"config_changed"`
	Config        []string `json:"config"` // the keys set, as lines
	server.Changes
	ComposeDir string       `json:"compose_dir"`
	Token      *tokenResult `json:"token,omitempty"`
	Notes      []string     `json:"notes,omitempty"`
}

type exposeFlags struct {
	private, out         string
	loopback, plan, json bool
	answers              []string
}

// command is this run as a command line, for a needs_user resume.
func (f exposeFlags) command() string {
	cmd := "backsight server expose --loopback"
	if f.private != "" {
		cmd = "backsight server expose --private " + shellWord(f.private)
	}
	if f.plan {
		cmd += " --plan"
	}
	if f.out != "" {
		cmd += " --out " + shellWord(f.out)
	}
	for _, a := range f.answers {
		cmd += " --answer " + shellWord(a)
	}
	if f.json {
		cmd += " --json"
	}
	return cmd
}

func newServerExposeCmd(e *env) *cobra.Command {
	var f exposeFlags
	cmd := &cobra.Command{
		Use:   "expose",
		Short: "Open the local server to your other PCs on a private address (--private <ip>), or close it again (--loopback)",
		Long: "--private <ip> publishes the server's API and UI on that address of this machine as well as on 127.0.0.1, checks it answers on both, " +
			"and hands out the connection token as backsight server token does. A tailnet address (Tailscale: 100.x) keeps the API key encrypted; " +
			"a LAN address carries it in clear and needs --answer cleartext_lan=yes. --loopback publishes it on 127.0.0.1 only again. " +
			"If the server does not run healthy afterwards, config.toml is put back as it was and the server started on it again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExpose(cmd.Context(), e, f)
		},
	}
	cmd.Flags().StringVar(&f.private, "private", "", "open the server on this private (LAN or tailnet) address of this machine")
	cmd.Flags().BoolVar(&f.loopback, "loopback", false, "close the server to other PCs: 127.0.0.1 only")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "show the config and server changes without making them")
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "answer a question: cleartext_lan=yes (a LAN address carries the API key in clear)")
	cmd.Flags().StringVar(&f.out, "out", "", "with --private: write the token to this file (mode 0600) instead of the terminal")
	return cmd
}

func runExpose(ctx context.Context, e *env, f exposeFlags) error {
	f.json = e.json
	if (f.private == "") == !f.loopback {
		return &output.Error{Code: output.Usage, Message: "say where the server listens: --private <ip> (this machine's LAN or tailnet address) or --loopback (127.0.0.1 only)"}
	}
	if f.loopback && f.out != "" {
		return &output.Error{Code: output.Usage, Message: "--out writes the token of a private exposure: --loopback hands out none"}
	}
	answers, err := parseAnswers(f.answers, "")
	if err != nil {
		return err
	}
	for k, v := range answers {
		if k != "cleartext_lan" {
			return &output.Error{Code: output.Usage, Message: "unknown answer " + k + ": server expose asks cleartext_lan only"}
		}
		if v != "yes" {
			return &output.Error{Code: output.Usage, Message: "answer cleartext_lan=" + v + ": want yes"}
		}
	}

	// 1. The change, validated as config.toml is, on an address of this
	// machine. Nothing changes before step 4.
	if err := tokenFileRefusal(f.out); err != nil {
		return err
	}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return err
	}
	if err := requireLocal(cfg, "server expose"); err != nil {
		return err
	}
	d, home, err := machine()
	if err != nil {
		return err
	}
	raw, sha, err := fsutil.ReadHashed(d.ConfigFile())
	if err != nil {
		return blocked(err)
	}
	old, err := config.Parse(raw, d.ConfigFile(), d, home)
	if err != nil {
		return blocked(err)
	}
	res := exposeResult{Exposure: "loopback", ConfigFile: d.ConfigFile(), ComposeDir: cfg.Server.ComposeDir}
	ip := net.ParseIP(f.private)
	if f.private != "" {
		res.Exposure, res.BindIP = "private", f.private
		if ip != nil {
			res.BindIP = ip.String()
		}
	}
	newRaw, err := config.SetNetwork(raw, res.Exposure, res.BindIP)
	if err != nil {
		return &output.Error{Code: output.Usage, Message: err.Error()}
	}
	next, err := config.Parse(newRaw, d.ConfigFile(), d, home)
	if err != nil {
		return &output.Error{Code: output.Usage, Message: err.Error() + ": nothing was changed"}
	}
	res.ConfigChanged = string(newRaw) != string(raw)
	res.Config = []string{"network.exposure = " + strconv.Quote(res.Exposure), "network.bind_ip = " + strconv.Quote(res.BindIP)}
	if res.BindIP == "" {
		res.Config[1] = "network.bind_ip: none"
	}
	giveToken := false
	if f.private != "" {
		if err := onThisMachine(ip); err != nil {
			return err
		}
		// 2. A LAN address carries the key in clear: the user's choice.
		if !config.IsTailnet(ip) && answers["cleartext_lan"] != "yes" {
			return &output.Error{Code: output.NeedsUser, Resume: f.command() + " --answer cleartext_lan=yes",
				Questions: []setup.Question{{ID: "cleartext_lan", Required: true,
					Prompt:  res.BindIP + " is not a tailnet address: " + cleartextWarning + ". Answer yes to open the server on it anyway",
					Options: []setup.Option{{Value: "yes", Label: "open the server on " + res.BindIP + ", the API key in clear on this network"}}}},
				Message: res.BindIP + " is not a tailnet address: " + cleartextWarning + ": rerun with --answer cleartext_lan=yes to open the server on it anyway: nothing was changed"}
		}
		if res.URL, err = clientURL(next); err != nil {
			return err
		}
		// No file and no terminal: the exposure goes on, without the token.
		if giveToken = tokenOut(e, f.out) == nil; !giveToken {
			res.Notes = append(res.Notes, "the connection token holds the API key, so it goes to a file or a terminal only: "+tokenElsewhere)
		}
	}
	// Docker in its own process group: a second Ctrl-C must not kill the
	// rollback's docker compose up.
	rtNext := ownGroupRuntime(next)
	if err := pinGuard(next, rtNext); err != nil {
		return err
	}

	// 3. The plan: the config change and what the server's files would become.
	if f.plan {
		files, err := renderOn(rtNext, sec, f.command())
		if err != nil {
			return err
		}
		if res.Changes, err = rtNext.Diff(files); err != nil {
			return blocked(err)
		}
		res.Plan = true
		return e.emit("server expose", res, func() string { return exposePlanText(res, f.out, giveToken) })
	}

	// 4. The change, rolled back on any failure, under the maintenance lock
	// (hooks queue, jobs skip) and server.lock.
	unlock, note, err := serverMaintenanceLock(d, "server expose")
	if err != nil {
		return err
	}
	defer unlock()
	if note != "" {
		res.Notes = append(res.Notes, note)
	}
	// Again under the lock: another command may have changed them.
	if _, sec, err = loadServerConfig(); err != nil {
		return err
	}
	// Both renders before any change: a missing secret changes nothing.
	nextFiles, err := renderOn(rtNext, sec, f.command())
	if err != nil {
		return err
	}
	rtOld := ownGroupRuntime(old)
	oldFiles, err := renderOn(rtOld, sec, "backsight server start")
	if err != nil {
		return err
	}
	if err := fsutil.ReplaceIfUnchanged(d.ConfigFile(), newRaw, 0o600, sha); err != nil {
		if errors.Is(err, fsutil.ErrConflict) {
			return blocked(fmt.Errorf("%s changed while server expose ran: nothing was changed, run it again", d.ConfigFile()))
		}
		return blocked(fmt.Errorf("writing %s: %w: nothing was changed", d.ConfigFile(), err))
	}
	ch, err := deployExposure(ctx, rtNext, next, sec, nextFiles)
	if err != nil {
		cause := redacted(err, sec).Error()
		// Put the old exposure back even when the command is interrupted.
		ctx = context.WithoutCancel(ctx)
		target := "on " + res.BindIP
		if res.BindIP == "" {
			target = "on 127.0.0.1 only"
		}
		if werr := fsutil.WriteAtomic(d.ConfigFile(), raw, 0o600); werr != nil {
			return redacted(fmt.Errorf("the server did not run %s (%s), and putting %s back failed (%s): it still sets %s; "+
				"run backsight server expose again with the exposure you had, then backsight server status", target, cause, d.ConfigFile(), werr.Error(), strings.Join(res.Config, ", ")), sec)
		}
		if _, rerr := deployExposure(ctx, rtOld, old, sec, oldFiles); rerr != nil {
			return redacted(fmt.Errorf("the server did not run %s (%s); config.toml is back as it was, but the server did not start healthy on it (%s): "+
				"run backsight server start, then backsight server status", target, cause, rerr.Error()), sec)
		}
		return redacted(&output.Error{Code: output.RolledBack, Message: fmt.Sprintf("the server did not run %s (%s): config.toml is back as it was and the server started again on it", target, cause)}, sec)
	}
	res.Changes = ch

	// 5. The token other PCs join with. The server is changed already: a
	// token that cannot be made is a note, not a failure.
	if giveToken {
		id, v, err := serverIdentity(ctx, next, sec)
		var where string
		if err == nil {
			where, err = writeToken(e, connectionToken(next, res.URL, sec, id, v), f.out)
		}
		if err != nil {
			res.Notes = append(res.Notes, "the connection token was not made ("+redacted(err, sec).Error()+"): "+tokenElsewhere)
		} else {
			res.Token = &tokenResult{URL: res.URL, Bank: next.Server.Bank, ServerID: id, Written: where}
		}
	}
	return e.emit("server expose", res, func() string { return exposeText(res) })
}

// deployExposure writes files, starts the server and checks it answers:
// on 127.0.0.1 (/health) and, exposed, on its private address (a TCP dial).
func deployExposure(ctx context.Context, rt server.Runtime, cfg config.Config, sec config.Secrets, files server.Files) (server.Changes, error) {
	ch, err := rt.Write(files)
	if err != nil {
		return ch, err
	}
	if err := rt.Start(ctx); err != nil {
		return ch, err
	}
	if _, err := waitHealthy(ctx, rt, cfg, sec); err != nil {
		return ch, err
	}
	if cfg.Network.Exposure != "private" {
		return ch, nil
	}
	port, err := cfg.APIPort()
	if err != nil {
		return ch, err
	}
	addr := net.JoinHostPort(cfg.Network.BindIP, strconv.Itoa(port))
	if err := dialPrivate(ctx, addr); err != nil {
		return ch, fmt.Errorf("it answers on 127.0.0.1 but nothing accepts connections at %s (%w)", addr, err)
	}
	return ch, nil
}

// onThisMachine refuses an address no interface of this machine has: the
// server could not listen on it. The error names the ones it could.
func onThisMachine(ip net.IP) error {
	addrs, err := interfaceAddrs()
	if err != nil {
		return blocked(fmt.Errorf("listing this machine's addresses: %w", err))
	}
	var found []string
	for _, a := range addrs {
		x := addrIP(a)
		switch {
		case x == nil:
		case x.Equal(ip):
			return nil
		case config.IsTailnet(x):
			found = append(found, x.String()+" (tailnet)")
		case x.IsPrivate():
			found = append(found, x.String())
		}
	}
	msg := ip.String() + " is not an address of this machine"
	if len(found) == 0 {
		return &output.Error{Code: output.Usage, Message: msg + ", which has no private (LAN or tailnet) address: connect it to the network, or start Tailscale, first"}
	}
	return &output.Error{Code: output.Usage, Message: msg + ": its private addresses are " + strings.Join(found, ", ")}
}

// addrIP is the IP of an interface address (nil for another kind).
func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	return nil
}

func exposePlanText(r exposeResult, out string, giveToken bool) string {
	var b strings.Builder
	verb := map[bool]string{true: "would set", false: "already sets"}[r.ConfigChanged]
	fmt.Fprintf(&b, "%s in %s:\n", verb, r.ConfigFile)
	for _, l := range r.Config {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	b.WriteString(renderText(renderResult{Dir: r.ComposeDir, Changes: r.Changes, Plan: true}))
	check := "127.0.0.1"
	if r.URL != "" {
		check += " and on " + strings.TrimPrefix(r.URL, "http://")
	}
	fmt.Fprintf(&b, "would restart the server and check it answers on %s (on failure: config.toml put back and the server restarted on it)\n", check)
	switch {
	case r.URL == "":
		b.WriteString("other PCs could no longer connect to it\n")
	case !giveToken:
		b.WriteString("the connection token would not be shown here: " + tokenElsewhere + "\n")
	case out != "":
		b.WriteString("then write the connection token to " + out + " (mode 0600)\n")
	default:
		b.WriteString("then print the connection token to this terminal\n")
	}
	return b.String()
}

func exposeText(r exposeResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", r.ConfigFile, strings.Join(r.Config, ", "))
	if r.URL == "" {
		b.WriteString("the server listens on 127.0.0.1 only: other PCs can no longer connect to it (backsight server expose --private <ip> opens it again)\n")
	} else {
		fmt.Fprintf(&b, "the server answers on 127.0.0.1 and on %s\n", r.URL)
	}
	if r.Token != nil {
		b.WriteString(tokenText(*r.Token, false))
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	return b.String()
}
