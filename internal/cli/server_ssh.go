package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	neturl "net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/internal/token"
	"github.com/Barben360/backsight/scripts"
)

// The remote scripts reach sh through the user's login shell, inside one
// quoted word (remoteScript): one line (csh refuses a newline inside
// quotes), no quote, no backslash (fish and csh read them otherwise) and
// no '!' (csh's history).

// probeScript prints the host's facts, one key=value line each, and
// changes nothing. The shim is where install.sh puts it on Linux.
var probeScript = strings.Join([]string{
	`set -u`,
	`echo "os=$(uname -s)"`,
	`echo "machine=$(uname -m)"`,
	`echo "conn=${SSH_CONNECTION:-}"`,
	`if [ -n "${BACKSIGHT_HOME:-}" ]; then shim=$BACKSIGHT_HOME/data/bin/current; else shim=${XDG_DATA_HOME:-$HOME/.local/share}/backsight/bin/current; fi`,
	`echo "shim=$shim"`,
	`if [ -e "$shim" ] || [ -L "$shim" ]; then echo has_shim=yes; else echo has_shim=no; fi`,
	`v=`,
	`if command -v minisign >/dev/null 2>&1; then v=minisign; elif command -v openssl >/dev/null 2>&1; then case $(openssl version 2>/dev/null) in "OpenSSL "[3-9]*) v=openssl ;; esac; fi`,
	`echo "verifier=$v"`,
	`m=`,
	`for c in tar mktemp; do command -v "$c" >/dev/null 2>&1 || m="$m $c"; done`,
	`echo "missing=$m"`,
	`echo "linger=$(loginctl show-user "$(id -u)" -p Linger --value 2>/dev/null)"`,
}, "; ")

// installScript unpacks the tar on stdin (install.sh and release/) into a
// temp directory removed on exit, also on a hangup (a killed ssh) or a
// signal, and runs that install.sh on it, trusting
// $1 too when given (a test key).
var installScript = strings.Join([]string{
	`set -eu`,
	// The traps come first: once the directory exists, a signal removes it.
	`d=`,
	`cleanup() { if [ -n "$d" ]; then rm -rf -- "$d"; fi; }`,
	`trap cleanup EXIT`,
	`trap "exit 129" HUP`, // dash and zsh run no EXIT trap on a signal's death
	`trap "exit 130" INT`,
	`trap "exit 143" TERM`,
	`d=$(mktemp -d)`,
	`tar -xf - -C "$d"`,
	`if [ -n "${1:-}" ]; then BACKSIGHT_INSTALL_PUBKEY=$1; export BACKSIGHT_INSTALL_PUBKEY; fi`,
	`BACKSIGHT_RELEASE_DIR=$d/release sh "$d/install.sh"`,
}, "; ")

// handbackScript prints the token file $1, which server token wrote on
// the host just before, as one line, and removes it on exit, also on a
// hangup (a killed ssh) or a signal.
var handbackScript = strings.Join([]string{
	`set -eu`,
	`f=${1:-}`,
	`cleanup() { if [ -n "$f" ]; then rm -f -- "$f"; fi; }`,
	`trap cleanup EXIT`,
	`trap "exit 129" HUP`,
	`trap "exit 130" INT`,
	`trap "exit 143" TERM`,
	`t=`,
	`IFS= read -r t < "$f" || :`,
	`[ -n "$t" ]`,
	`echo "BACKSIGHT-TOKEN $t"`,
}, "; ")

// hostFacts: what probeScript says about the host, read-only.
type hostFacts struct {
	OS, Machine string   // uname -s, uname -m
	GOARCH      string   // amd64 | arm64; "" unsupported
	Addr        string   // the host's end of the ssh connection (SSH_CONNECTION's third field)
	Shim        string   // <data>/bin/current, computed as install.sh does on Linux
	HasShim     bool     // the shim exists (a link, even dangling)
	Verifier    string   // minisign | openssl | "" (none able to check Ed25519)
	Missing     []string // of tar, mktemp
	Linger      string   // loginctl's Linger for this user: yes | no | ""
}

// probeHost reads the host's facts. Lines the login shell's profile
// prints around the answer are ignored.
func probeHost(ctx context.Context, target string) (hostFacts, error) {
	remote, err := remoteScript("probe", probeScript)
	if err != nil {
		return hostFacts{}, err
	}
	var out bytes.Buffer
	tail, err := sshRun(ctx, target, remote, nil, &out, nil)
	if re := (*remoteExit)(nil); errors.As(err, &re) {
		return hostFacts{}, &output.Error{Code: output.Blocked, Message: fmt.Sprintf("cannot read what %s runs (%s): nothing was changed there", target, lastLine(tail))}
	}
	if err != nil {
		return hostFacts{}, err
	}
	var f hostFacts
	seen := map[string]bool{}
	for _, l := range strings.Split(out.String(), "\n") {
		k, v, ok := strings.Cut(strings.TrimSuffix(l, "\r"), "=")
		if !ok {
			continue
		}
		seen[k] = true
		switch k {
		case "os":
			f.OS = v
		case "machine":
			f.Machine = v
		case "conn":
			if w := strings.Fields(v); len(w) >= 3 {
				f.Addr = w[2]
			}
		case "shim":
			f.Shim = v
		case "has_shim":
			f.HasShim = v == "yes"
		case "verifier":
			if v == "minisign" || v == "openssl" {
				f.Verifier = v
			}
		case "missing":
			if w := strings.Fields(v); len(w) > 0 {
				f.Missing = w
			}
		case "linger":
			if v == "yes" || v == "no" {
				f.Linger = v
			}
		default:
			seen[k] = false
		}
	}
	if !seen["os"] || !seen["machine"] || !seen["shim"] {
		return hostFacts{}, &output.Error{Code: output.Blocked,
			Message: "the shell on " + target + " did not answer the probe as sh does: check that it runs sh -c for a command (ssh " + shellWord(target) + " uname -s)"}
	}
	switch f.Machine {
	case "x86_64", "amd64":
		f.GOARCH = "amd64"
	case "aarch64", "arm64":
		f.GOARCH = "arm64"
	}
	return f, nil
}

// findRelease finds the release of this backsight's version and its
// archive for a Linux goarch host, reading only the source's listing.
func findRelease(ctx context.Context, src release.Source, version, goarch string) (rel release.Release, archive string, err error) {
	if v, err := release.Parse(version); err != nil || v.Dev() {
		return rel, "", &output.Error{Code: output.Blocked,
			Message: fmt.Sprintf("this backsight is a development build (%s): no release matches it; run a released backsight, or backsight server install --print-commands", version)}
	}
	rels, err := src.Releases(ctx)
	if err != nil {
		return rel, "", sourceError(err)
	}
	rel, ok := release.Find(rels, version)
	if !ok {
		return rel, "", &output.Error{Code: output.Blocked,
			Message: fmt.Sprintf("the release source has no version %s: server install --ssh copies the release matching this backsight", version)}
	}
	a, ok := rel.Archive("linux", goarch)
	if !ok {
		return rel, "", nothingChanged("release %s has no archive for linux/%s", rel.Version, goarch)
	}
	return rel, a.Name, nil
}

// stageRelease downloads the release of this backsight's version for a
// Linux goarch host into work and verifies it here: the host needs no
// route to the release source, and checks it again itself.
func stageRelease(ctx context.Context, src release.Source, version, goarch, work string) (archive, keyID string, err error) {
	rel, archive, err := findRelease(ctx, src, version, goarch)
	if err != nil {
		return "", "", err
	}
	if _, keyID, err = verifyRelease(ctx, src, rel, archive, work); err != nil {
		return "", "", err
	}
	return archive, keyID, nil
}

// installOnHost sends this version's install.sh and the release staged in
// work to the host on ssh's stdin, and runs install.sh there: it checks
// the signature with the key it carries and the archive's hash, and
// switches the shim, then the command on PATH, last (a failure between
// the two leaves the shim switched). tail is the last lines of its stderr.
func installOnHost(ctx context.Context, target, work, archive string) (tail string, err error) {
	var args []string
	if signing.TestKey != "" { // a testkey build: install.sh trusts it too, and says so
		args = append(args, signing.TestKey)
	}
	remote, err := remoteScript("install", installScript, args...)
	if err != nil {
		return "", err
	}
	upload := filepath.Join(work, "upload.tar")
	if err := writeUpload(upload, work, archive); err != nil {
		return "", blocked(err)
	}
	f, err := os.Open(upload)
	if err != nil {
		return "", blocked(err)
	}
	defer func() { _ = f.Close() }()
	tail, err = sshRun(ctx, target, remote, f, nil, nil)
	if re := (*remoteExit)(nil); errors.As(err, &re) {
		return tail, &output.Error{Code: output.Blocked,
			Message: fmt.Sprintf("the release did not install on %s (%s): the host may still run its previous backsight; rerunning this is safe", target, lastLine(tail))}
	}
	if err != nil {
		return tail, &sshTransport{err}
	}
	return tail, nil
}

// writeUpload writes the tar installScript unpacks: install.sh and
// release/ holding checksums.txt, its signature and the archive.
func writeUpload(dst, work, archive string) (err error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(out)
	if err := tw.WriteHeader(&tar.Header{Name: "install.sh", Mode: 0o644, Size: int64(len(scripts.Install)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(scripts.Install); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "release/", Mode: 0o700, Typeflag: tar.TypeDir}); err != nil {
		return err
	}
	for _, name := range []string{"checksums.txt", "checksums.txt.minisig", archive} {
		if err := tarFile(tw, filepath.Join(work, name), "release/"+name); err != nil {
			return err
		}
	}
	return tw.Close()
}

func tarFile(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// lastLine is the last non-blank line of s, trimmed.
func lastLine(s string) string {
	last := ""
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			last = l
		}
	}
	return last
}

// Answers by destination: one --answer list, split so each side sees only
// its own ids. consent and ack_subscription go to both; cleartext_lan to
// the host's server expose.
var (
	hostAnswers   = []string{"provider", "model", "base_url", "api_port", "ui_port", "project", "backups", "scheduler"}
	clientAnswers = []string{"retention", "backfill", "reference"}
)

// sshInstallFlags is server install --ssh.
type sshInstallFlags struct {
	target, private, source string
	keyEnv, keyFile         string
	answers                 []string
	plan, nonInteractive    bool
	configDir               string
	install                 installFlags      // --provider --model --base-url --api-port --ui-port
	changed                 func(string) bool // cmd.Flags().Changed
}

// installFlagAnswers pairs the install flags with the host's setup ids.
var installFlagAnswers = [][2]string{{"provider", "provider"}, {"model", "model"}, {"base-url", "base_url"}, {"api-port", "api_port"}, {"ui-port", "ui_port"}}

// flagValue is the value of an install flag given on the command line.
func (f sshInstallFlags) flagValue(flag string) (string, bool) {
	if f.changed == nil || !f.changed(flag) {
		return "", false
	}
	switch flag {
	case "provider":
		return f.install.provider, true
	case "model":
		return f.install.model, true
	case "base-url":
		return f.install.base, true
	case "api-port":
		return strconv.Itoa(f.install.apiPort), true
	case "ui-port":
		return strconv.Itoa(f.install.uiPort), true
	}
	return "", false
}

// planCommand is this run's plan: command without the key flags, which
// --plan refuses (it reads no key).
func (f sshInstallFlags) planCommand() string {
	f.keyEnv, f.keyFile = "", ""
	return f.command() + " --plan"
}

// command is this run as a command line, for every resume: flag names and
// paths, never a key.
func (f sshInstallFlags) command() string {
	cmd := "backsight server install --ssh " + shellWord(f.target)
	if f.private != "" {
		cmd += " --private " + shellWord(f.private)
	}
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	if f.source != "" {
		cmd += " --source " + shellWord(abs(f.source))
	}
	if f.keyEnv != "" {
		cmd += " --llm-key-from-env " + shellWord(f.keyEnv)
	}
	if f.keyFile != "" {
		cmd += " --llm-key-from-file " + shellWord(abs(f.keyFile))
	}
	for _, p := range installFlagAnswers {
		if v, ok := f.flagValue(p[0]); ok {
			cmd += " --" + p[0] + " " + shellWord(v)
		}
	}
	for _, a := range f.answers {
		cmd += " --answer " + shellWord(a)
	}
	if f.nonInteractive {
		cmd += " --non-interactive"
	}
	if f.configDir != "" {
		cmd += " --config-dir " + shellWord(abs(f.configDir))
	}
	return cmd
}

// hostInstall is what backsight on the host says of its install, read-only.
type hostInstall struct {
	Version        string // the host's backsight; "" none
	Config         bool
	Role, Mode     string
	LLMKeySet      bool
	LLMKeyRequired bool // its config's provider needs a key
	StateDir       string
}

// sshTransport is ssh's own failure on a step on the host (exit 255, or
// ssh not started): the step may not have run, or have run in part.
type sshTransport struct{ err error }

func (s *sshTransport) Error() string { return s.err.Error() }
func (s *sshTransport) Unwrap() error { return s.err }

// quietEnv runs a read-only step on the host: nothing to relay.
var quietEnv = &env{stdout: io.Discard, stderr: io.Discard, json: true}

// runOnHost runs the host's backsight (shim) with args over ssh and returns
// its envelope's data. It relays the host's progress lines from its
// stderr: with --json as JSON lines whose step is host:<step>, else one
// line <target>: <step> when the step changes. The host's error keeps its
// code, its message and actions' reasons are named for the host, and a
// relogin is dropped (each ssh run is a new login); the caller sets the
// resume. ssh's own failure comes back as an *sshTransport.
func runOnHost(ctx context.Context, e *env, target, shim string, stdin io.Reader, args ...string) (json.RawMessage, error) {
	remote, err := remoteShim(shim, args...)
	if err != nil {
		return nil, err
	}
	last := ""
	relay := func(line []byte) {
		var ev map[string]any
		if json.Unmarshal(line, &ev) != nil || ev["event"] != "progress" {
			return
		}
		st, _ := ev["step"].(string)
		if st == "" {
			return
		}
		if e.json {
			ev["step"] = "host:" + st
			b, _ := json.Marshal(ev)
			fmt.Fprintln(e.stderr, string(b))
		} else if st != last {
			fmt.Fprintf(e.stderr, "%s: %s\n", target, st)
		}
		last = st
	}
	var out bytes.Buffer
	tail, err := sshRun(ctx, target, remote, stdin, &out, relay)
	re := (*remoteExit)(nil)
	if err != nil && !errors.As(err, &re) {
		return nil, &sshTransport{err}
	}
	ans, ok := hostEnvelope(out.Bytes())
	what := "backsight " + strings.Join(args, " ")
	switch {
	case !ok:
		code := 0
		if re != nil {
			code = re.Code
		}
		return nil, &output.Error{Code: output.Blocked, Message: fmt.Sprintf("on %s: %s gave no answer backsight can read (exit %d): %s", target, what, code, lastLine(tail))}
	case ans.Error != nil:
		he := *ans.Error
		he.Message, he.Resume, he.Actions = "on "+target+": "+he.Message, "", nil
		for _, a := range ans.Error.Actions {
			if a.Kind != "relogin" {
				a.Why = "on " + target + ": " + a.Why
				he.Actions = append(he.Actions, a)
			}
		}
		return nil, &he
	case re != nil:
		return nil, &output.Error{Code: output.Blocked, Message: fmt.Sprintf("on %s: %s exited with code %d: %s", target, what, re.Code, lastLine(tail))}
	}
	return ans.Data, nil
}

// hostEnv is the envelope backsight prints on the host.
type hostEnv struct {
	SchemaVersion int             `json:"schema_version"`
	Data          json.RawMessage `json:"data"`
	Error         *output.Error   `json:"error"`
}

// hostEnvelope finds backsight's envelope in the host's stdout, past any
// line the login shell's profile printed first.
func hostEnvelope(out []byte) (hostEnv, bool) {
	for i := 0; i < len(out); i++ {
		if out[i] != '{' || i > 0 && out[i-1] != '\n' {
			continue
		}
		var env hostEnv
		if json.NewDecoder(bytes.NewReader(out[i:])).Decode(&env) == nil && env.SchemaVersion > 0 {
			return env, true
		}
	}
	return hostEnv{}, false
}

// inspectHost reads the host's backsight, when it has one: its version,
// whether it is set up and how, its state directory and whether its LLM
// key is set. Read-only.
func inspectHost(ctx context.Context, target string, facts hostFacts) (hostInstall, error) {
	var h hostInstall
	if !facts.HasShim {
		return h, nil
	}
	raw, err := runOnHost(ctx, quietEnv, target, facts.Shim, nil, "version", "--json")
	if err != nil {
		return h, err
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Version == "" {
		return h, unreadableHost(target, "version")
	}
	h.Version = v.Version
	if h, err = hostPaths(ctx, target, facts.Shim, h); err != nil {
		return h, err
	}
	if raw, err = runOnHost(ctx, quietEnv, target, facts.Shim, nil, "secret", "list", "--json"); err != nil {
		return h, err
	}
	var list []secretState
	if json.Unmarshal(raw, &list) != nil {
		return h, unreadableHost(target, "secret list")
	}
	for _, s := range list {
		if s.Name == "llm_api_key" {
			h.LLMKeySet, h.LLMKeyRequired = s.Set, s.Required
		}
	}
	return h, nil
}

// hostPaths adds what backsight paths says on the host to h.
func hostPaths(ctx context.Context, target, shim string, h hostInstall) (hostInstall, error) {
	raw, err := runOnHost(ctx, quietEnv, target, shim, nil, "paths", "--json")
	if err != nil {
		return h, err
	}
	var p struct {
		State        string `json:"state"`
		ConfigExists bool   `json:"config_exists"`
		Role         string `json:"server_role"`
		Mode         string `json:"server_mode"`
	}
	if json.Unmarshal(raw, &p) != nil || !strings.HasPrefix(p.State, "/") {
		return h, unreadableHost(target, "paths")
	}
	h.StateDir, h.Config, h.Role, h.Mode = p.State, p.ConfigExists, p.Role, p.Mode
	return h, nil
}

func unreadableHost(target, what string) error {
	return &output.Error{Code: output.Blocked, Message: "on " + target + ": backsight " + what + " answered something backsight here cannot read: nothing was changed there"}
}

// hostAddress is the address the host's server opens on for the other
// PCs: --private when given, else the one this PC's ssh reached. A
// tailnet address is used as is, another private one with
// cleartext_lan=yes; any other refuses.
func hostAddress(target string, facts hostFacts, private string, cleartext bool) (string, error) {
	addr := private
	if addr == "" {
		addr = facts.Addr
	}
	ip := net.ParseIP(addr)
	switch {
	case ip != nil && config.IsTailnet(ip):
		return ip.String(), nil
	case ip != nil && ip.IsPrivate() && cleartext:
		return ip.String(), nil
	case ip != nil && ip.IsPrivate():
		a := ip.String()
		return "", &output.Error{Code: output.NeedsUser,
			Questions: []setup.Question{{ID: "cleartext_lan", Required: true,
				Prompt:  a + " is not a tailnet address: " + cleartextWarning + ". Answer yes to open the server on it anyway",
				Options: []setup.Option{{Value: "yes", Label: "open the server on " + a + ", the API key in clear on this network"}}}},
			Message: a + " is not a tailnet address: " + cleartextWarning + ": rerun with --answer cleartext_lan=yes to open the server on it anyway, or with --private <ip> for another address of the host: nothing was changed"}
	case private != "":
		return "", &output.Error{Code: output.Usage, Message: "--private " + private + " is not a LAN or tailnet address: give one of the host's (tailscale ip -4 or ip addr there)"}
	}
	if addr == "" {
		addr = "an address ssh did not say"
	}
	return "", &output.Error{Code: output.NeedsUser,
		Message: "the ssh connection reached " + target + " at " + addr + ", which other PCs cannot use: rerun with --private <ip>, a LAN or tailnet address of the host (tailscale ip -4 or ip addr there)"}
}

// handbackFile is where server token writes the token on the host, in
// its state directory (mode 0600), for handbackScript to bring back.
const handbackFile = "ssh-handback-token"

// tokenFromHost has the host's server token written to a file in its
// state directory, then reads it back over ssh, the file removed. The
// token is decoded here and never written nor shown.
func tokenFromHost(ctx context.Context, target, shim string, h hostInstall) (token.Token, error) {
	file := strings.TrimSuffix(h.StateDir, "/") + "/" + handbackFile
	if _, err := runOnHost(ctx, quietEnv, target, shim, nil, "server", "token", "--out", file, "--json"); err != nil {
		return token.Token{}, err
	}
	remote, err := remoteScript("handback", handbackScript, file)
	if err != nil {
		return token.Token{}, err
	}
	var out bytes.Buffer
	tail, err := sshRun(ctx, target, remote, nil, &out, nil)
	if re := (*remoteExit)(nil); errors.As(err, &re) {
		return token.Token{}, &output.Error{Code: output.Blocked, Message: fmt.Sprintf("on %s: the connection token could not be read back (%s)", target, lastLine(tail))}
	}
	if err != nil {
		return token.Token{}, &sshTransport{err}
	}
	for _, l := range strings.Split(out.String(), "\n") {
		if raw, ok := strings.CutPrefix(strings.TrimSuffix(l, "\r"), "BACKSIGHT-TOKEN "); ok {
			tok, err := token.Decode(raw)
			if err != nil {
				return token.Token{}, &output.Error{Code: output.Blocked, Message: "on " + target + ": the connection token came back unreadable (" + err.Error() + ")"}
			}
			return tok, nil
		}
	}
	return token.Token{}, &output.Error{Code: output.Blocked, Message: "on " + target + ": the connection token did not come back over ssh"}
}

// sshInstallResult is what server install --ssh did or would do. It never
// holds a key nor the token.
type sshInstallResult struct {
	Target          string         `json:"target"`
	Address         string         `json:"address"`
	Version         string         `json:"version"`
	Plan            bool           `json:"plan"`
	Declined        bool           `json:"declined,omitempty"`
	Steps           []step         `json:"steps"`
	Warnings        []string       `json:"warnings,omitempty"`
	Connect         *connectResult `json:"connect,omitempty"`
	RestartRequired bool           `json:"restart_required"`
	NextSteps       []string       `json:"next_steps,omitempty"`
	shown           int            // steps already printed, in text mode
	warned          int            // warnings already printed, in text mode
}

// sshInstallText is what text mode prints at the end: the steps not yet
// printed, then connect's own summary, the warnings and what to do next.
func sshInstallText(r sshInstallResult) string {
	var b strings.Builder
	if r.shown < len(r.Steps) {
		for _, s := range r.Steps[r.shown:] {
			b.WriteString(stepLine(s))
		}
	}
	if r.Connect != nil {
		b.WriteString(connectText(*r.Connect))
	} else if r.Declined {
		b.WriteString("nothing changed\n")
	}
	if r.warned < len(r.Warnings) {
		for _, w := range r.Warnings[r.warned:] {
			fmt.Fprintf(&b, "warning  %s\n", w)
		}
	}
	if r.Connect == nil {
		for _, n := range r.NextSteps {
			b.WriteString("- " + n + "\n")
		}
	}
	return b.String()
}

// sshInstallAnswers checks the run's flags and answers, here only.
func sshInstallAnswers(f sshInstallFlags) (map[string]string, error) {
	values := []string{f.target, f.private, f.source, f.keyFile, f.configDir}
	for _, a := range f.answers {
		_, v, _ := strings.Cut(a, "=")
		values = append(values, a, v)
	}
	if slices.ContainsFunc(values, looksLikeToken) {
		return nil, &output.Error{Code: output.Usage, Message: tokenArgument}
	}
	if err := checkTarget(f.target); err != nil {
		return nil, err
	}
	keyFlag := f.keyEnv != "" || f.keyFile != ""
	switch {
	case f.keyEnv != "" && f.keyFile != "":
		return nil, &output.Error{Code: output.Usage, Message: "use --llm-key-from-env or --llm-key-from-file, not both"}
	case f.plan && keyFlag:
		return nil, &output.Error{Code: output.Usage, Message: "--plan reads no key: drop --llm-key-from-env and --llm-key-from-file"}
	}
	answers, err := parseAnswers(f.answers, "")
	if err != nil {
		return nil, err
	}
	for k, v := range answers {
		switch {
		case k == "cleartext_lan" && v != "yes":
			return nil, &output.Error{Code: output.Usage, Message: "answer cleartext_lan=" + v + ": want yes"}
		case k == "consent", k == "ack_subscription", k == "cleartext_lan", slices.Contains(hostAnswers, k), slices.Contains(clientAnswers, k):
		default:
			return nil, &output.Error{Code: output.Usage, Message: "unknown answer " + k + ": server install --ssh takes the host's setup answers (" + strings.Join(hostAnswers, ", ") +
				"), this PC's (" + strings.Join(clientAnswers, ", ") + "), consent, ack_subscription and cleartext_lan"}
		}
	}
	for _, p := range installFlagAnswers {
		if v, ok := f.flagValue(p[0]); ok && answers[p[1]] != "" && answers[p[1]] != v {
			return nil, &output.Error{Code: output.Usage, Message: "--" + p[0] + " " + v + " and --answer " + p[1] + "=" + answers[p[1]] + " disagree: give one"}
		}
	}
	return answers, nil
}

// runSSHInstall installs the memory server on f.target over ssh and joins
// it from this PC: probe, plan, consent and key first, with nothing
// changed anywhere; then the release, the key, setup, expose and the
// token there, and connect here. The host is never rolled back: every
// step runs again, so a rerun goes on.
func runSSHInstall(ctx context.Context, e *env, f sshInstallFlags) (sshInstallResult, error) {
	t := f.target
	res := sshInstallResult{Target: t, Version: buildinfo.Version, Plan: f.plan, Steps: []step{}}
	resume := f.command()
	var key string
	var tok token.Token
	var localSec config.Secrets
	// Every error is masked with each secret this run holds.
	hide := func(err error) error {
		if err == nil {
			return nil
		}
		return redacted(redacted(err, localSec), config.Secrets{LLMAPIKey: key, APIKey: tok.APIKey})
	}

	// 1. This PC.
	answers, err := sshInstallAnswers(f)
	if err != nil {
		return res, err
	}
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	local, err := config.Load(d.ConfigFile(), d, home)
	hasLocal := err == nil
	switch {
	case errors.Is(err, fs.ErrNotExist):
		local = config.Config{}
	case err != nil:
		return res, blocked(err)
	case local.ServerOnly():
		return res, &output.Error{Code: output.Usage, Message: "this PC is a server-only host: run server install --ssh from a PC that uses the memory"}
	}
	if localSec, err = config.LoadSecrets(d.SecretsFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, blocked(err)
	}
	refuse := func(err error) (sshInstallResult, error) { return res, hide(withResume(err, resume)) }
	// The host's progress reaches this PC's stderr masked too.
	he := &env{stdout: e.stdout, json: e.json, stderr: redactingWriter{e.stderr, func() []config.Secrets {
		return []config.Secrets{localSec, {LLMAPIKey: key, APIKey: tok.APIKey}}
	}}}

	// 2. The host's facts.
	facts, err := probeHost(ctx, t)
	if err != nil {
		return refuse(err)
	}
	switch {
	case facts.OS != "Linux":
		return refuse(&output.Error{Code: output.Blocked, Message: t + " runs " + facts.OS + ": server install --ssh installs on a Linux host only for now; for another one, backsight server install --print-commands"})
	case facts.GOARCH == "":
		return refuse(&output.Error{Code: output.Blocked, Message: t + " is a " + facts.Machine + " machine, for which backsight has no release: server install --ssh installs on x86_64 and aarch64 Linux hosts; for another one, backsight server install --print-commands"})
	case facts.Verifier == "":
		return refuse(&output.Error{Code: output.NeedsUser, Message: t + " has neither minisign nor OpenSSL 3 to check the release signature: install one there, then run this again",
			Actions: []output.Action{{Kind: "install", Cmd: "minisign", Why: "on " + t + ": install minisign (or OpenSSL 3), which checks the release signature"}}})
	case len(facts.Missing) > 0:
		return refuse(&output.Error{Code: output.Blocked, Message: t + " lacks " + strings.Join(facts.Missing, ", ") + ", which the install needs there: install them, then run this again"})
	}
	res.Steps = append(res.Steps, step{"probe", "done", fmt.Sprintf("%s %s, reached at %s", facts.OS, facts.Machine, facts.Addr)})
	if facts.Linger == "no" {
		user := "<user>"
		if u, _, ok := strings.Cut(t, "@"); ok {
			user = u
		}
		res.Warnings = append(res.Warnings, "on "+t+", lingering is off: its backups and update checks run only while you are logged in there (sudo loginctl enable-linger "+user+" there)")
	}

	// 3. What backsight the host runs, if any.
	h, err := inspectHost(ctx, t, facts)
	if err != nil {
		return refuse(err)
	}
	hostDetail := "no backsight there yet"
	switch {
	case h.Version != "" && h.Version != buildinfo.Version:
		return refuse(&output.Error{Code: output.Blocked, Message: fmt.Sprintf("%s runs backsight %s and this PC %s: bring both to the same release (backsight update apply on the older one), then run this again", t, h.Version, buildinfo.Version)})
	case h.Config && h.Mode == "remote":
		return refuse(&output.Error{Code: output.Blocked, Message: t + " is a client of another memory server: install the server on that server's host instead, or join it from this PC with a token from there (backsight server token --out FILE there)"})
	case h.Config && h.Role != config.RoleServer:
		return refuse(&output.Error{Code: output.Blocked, Message: t + " is set up as a PC with Claude Code: open its server there (backsight server expose --private <ip>, then backsight server token --out FILE) and run backsight connect --token-file FILE here"})
	case h.Config:
		hostDetail = "backsight " + h.Version + ", server role"
	case h.Version != "":
		hostDetail = "backsight " + h.Version + ", not set up yet"
	}
	res.Steps = append(res.Steps, step{"host", "done", hostDetail})

	// 4. The address the other PCs reach the server on.
	addr, err := hostAddress(t, facts, f.private, answers["cleartext_lan"] == "yes")
	if err != nil {
		var oe *output.Error
		if errors.As(err, &oe) && oe.Code == output.NeedsUser {
			out := *oe
			out.Resume = resume + " --private <ip>"
			if oe.Questions != nil {
				out.Resume = resume + " --answer cleartext_lan=yes"
			}
			return res, hide(&out)
		}
		return refuse(err)
	}
	res.Address = addr

	// 5. The release, unless the host runs this one.
	var src release.Source
	if h.Version == buildinfo.Version {
		res.Steps = append(res.Steps, step{"release", "skipped", "the host runs backsight " + h.Version})
	} else {
		src = releaseSource(local, f.source)
		if _, _, err := findRelease(ctx, src, buildinfo.Version, facts.GOARCH); err != nil {
			return refuse(err)
		}
		res.Steps = append(res.Steps, step{"release", "planned", fmt.Sprintf("backsight %s for linux/%s, verified here then on the host", buildinfo.Version, facts.GOARCH)})
	}

	// 6. The plan.
	provider, given := answers["provider"], answers["provider"] != ""
	if v, ok := f.flagValue("provider"); ok && v != "" {
		provider, given = v, true
	}
	keyNeeded := h.LLMKeyRequired
	if given || !h.Config {
		if !given {
			provider = config.Default(d, home).LLM.Provider
		}
		keyNeeded = !config.KeylessProviders[provider]
	}
	keyFlag := f.keyEnv != "" || f.keyFile != ""
	needKey := keyNeeded && (keyFlag || !h.LLMKeySet)
	switch {
	case !keyNeeded:
		res.Steps = append(res.Steps, step{"llm-key", "skipped", "the host's provider needs no key"})
	case !needKey:
		res.Steps = append(res.Steps, step{"llm-key", "skipped", "already set on the host"})
	case f.keyEnv != "":
		res.Steps = append(res.Steps, step{"llm-key", "planned", "from --llm-key-from-env " + f.keyEnv + ", on the stdin of backsight secret set there"})
	case f.keyFile != "":
		res.Steps = append(res.Steps, step{"llm-key", "planned", "from --llm-key-from-file " + f.keyFile + ", on the stdin of backsight secret set there"})
	case stdinIsTerminal():
		res.Steps = append(res.Steps, step{"llm-key", "planned", "from a hidden prompt, on the stdin of backsight secret set there"})
	default:
		res.Steps = append(res.Steps, step{"llm-key", "planned", "from the first line of stdin, on the stdin of backsight secret set there"})
	}
	var hostAns []string
	for _, id := range hostAnswers {
		v, ok := answers[id]
		for _, p := range installFlagAnswers {
			if fv, set := f.flagValue(p[0]); set && p[1] == id {
				v, ok = fv, true
			}
		}
		if id == "provider" && !ok && provider != "" {
			v, ok = provider, true
		}
		if ok {
			hostAns = append(hostAns, id+"="+v)
		}
	}
	if v, ok := answers["ack_subscription"]; ok {
		hostAns = append(hostAns, "ack_subscription="+v)
	}
	setupDetail := "backsight setup --role server there"
	if len(hostAns) > 0 {
		setupDetail += ", answering " + strings.Join(hostAns, ", ")
	}
	connectDetail := "join it: this PC's sessions go to the host's memory"
	switch {
	case hasLocal && local.Server.Mode == "remote" && sameHost(local.Server.URL, t, addr):
		connectDetail = "already joined (the server at " + local.Server.URL + "): connect checks it is the same server and updates its address and key"
	case hasLocal && local.Server.Mode == "remote":
		connectDetail = "leave the server at " + local.Server.URL + " for it"
	case hasLocal:
		connectDetail = "back up, verify and stop this PC's own server, then send its sessions to the host (connect --undo goes back)"
	}
	res.Steps = append(res.Steps,
		step{"setup", "planned", setupDetail},
		step{"expose", "planned", "backsight server expose --private " + addr + " there: the server answers other PCs on " + addr},
		step{"token", "planned", "brought back over ssh, never shown"},
		step{"connect", "planned", connectDetail})
	if f.plan {
		return res, nil
	}

	// 7. Consent, then the key: all before any change on the host.
	interactive := !e.json && !f.nonInteractive && stdinIsTerminal() && stdoutIsTerminal()
	switch consent := answers["consent"]; {
	case consent == "" && interactive:
		fmt.Fprint(e.stdout, sshInstallText(res))
		res.shown, res.warned = len(res.Steps), len(res.Warnings)
		if !confirm(e, "Install the memory server on "+t+" and join it from this PC? [y/N] ") {
			res.Declined = true
			return res, nil
		}
		// The steps done so far were in the plan; the others show as done.
		for res.shown = 0; res.shown < len(res.Steps) && res.Steps[res.shown].Status != "planned"; res.shown++ {
		}
	case consent == "":
		return res, hide(&output.Error{Code: output.NeedsUser, Resume: resume + " --answer consent=yes",
			Message: "server install --ssh changes " + t + " and this PC: read the plan (" + f.planCommand() + "), then rerun with --answer consent=yes: nothing was changed"})
	case consent != "yes":
		res.Declined = true
		return res, nil
	}
	if needKey {
		noKey := &output.Error{Code: output.NeedsUser, Resume: resume + " --llm-key-from-file F",
			Message: "no terminal and nothing on stdin: pass the key with --llm-key-from-file F or --llm-key-from-env VAR: nothing was changed"}
		if f.nonInteractive && !keyFlag && stdinIsTerminal() { // never a prompt
			noKey.Message = "--non-interactive asks no key at a prompt: pass it with --llm-key-from-file F or --llm-key-from-env VAR: nothing was changed"
			return res, noKey
		}
		key, err = readSecret(e, "LLM API key for "+t, f.keyEnv, f.keyFile)
		var oe *output.Error
		if errors.As(err, &oe) && oe.Code == output.NeedsUser {
			return res, noKey
		}
		if err != nil {
			return refuse(err)
		}
		if key == "" {
			return res, &output.Error{Code: output.Usage, Message: "the LLM API key is empty: nothing was changed"}
		}
	}

	// 8-13. The changes, each shown as it is done.
	show := func() {
		if e.json {
			return
		}
		for ; res.shown < len(res.Steps) && res.Steps[res.shown].Status != "planned"; res.shown++ {
			fmt.Fprint(e.stdout, stepLine(res.Steps[res.shown]))
		}
	}
	mark := func(name, status string) {
		for i := range res.Steps {
			if res.Steps[i].Name == name {
				res.Steps[i].Status = status
			}
		}
		show()
	}
	fail := func(name string, err error) (sshInstallResult, error) {
		mark(name, "failed")
		var tr *sshTransport
		if errors.As(err, &tr) {
			pe := &output.Error{Code: output.Partial, Message: "the install on " + t + " stopped part way: " + tr.Error()}
			var oe *output.Error
			if errors.As(tr.err, &oe) { // ssh's own class: what the user runs before the rerun
				pe.Actions = oe.Actions
			}
			err = pe
		}
		return res, hide(withResume(err, resume))
	}
	show()
	if src != nil {
		if err := installRelease(ctx, t, src, facts.GOARCH); err != nil {
			return fail("release", err)
		}
		mark("release", "done")
	}
	if h.StateDir == "" {
		if h, err = hostPaths(ctx, t, facts.Shim, h); err != nil {
			return fail("", err)
		}
	}
	if needKey {
		if _, err := runOnHost(ctx, he, t, facts.Shim, strings.NewReader(key+"\n"), "secret", "set", "llm_api_key", "--json"); err != nil {
			return fail("llm-key", err)
		}
		mark("llm-key", "done")
	}
	args := []string{"setup", "--role", "server", "--non-interactive", "--json", "--answer", "consent=yes"}
	for _, a := range hostAns {
		args = append(args, "--answer", a)
	}
	if _, err := runOnHost(ctx, he, t, facts.Shim, nil, args...); err != nil {
		return fail("setup", err)
	}
	mark("setup", "done")
	args = []string{"server", "expose", "--private", addr, "--json"}
	if answers["cleartext_lan"] == "yes" {
		args = append(args, "--answer", "cleartext_lan=yes")
	}
	if _, err := runOnHost(ctx, he, t, facts.Shim, nil, args...); err != nil {
		return fail("expose", err)
	}
	mark("expose", "done")
	if tok, err = tokenFromHost(ctx, t, facts.Shim, h); err != nil {
		return fail("token", err)
	}
	mark("token", "done")

	// 13. This PC joins it: at a terminal, connect shows its own plan and asks.
	var cargs []string
	for _, a := range f.answers {
		k, _, _ := strings.Cut(a, "=")
		if k = strings.TrimSpace(k); k == "consent" || k == "ack_subscription" || slices.Contains(clientAnswers, k) {
			cargs = append(cargs, a)
		}
	}
	cres, err := runConnect(ctx, e, connectFlags{preread: &tok, resume: resume, answers: cargs, nonInteractive: f.nonInteractive, configDir: f.configDir})
	if cres.URL != "" {
		res.Connect = &cres
	}
	res.RestartRequired, res.NextSteps = cres.RestartRequired, cres.NextSteps
	if err != nil {
		mark("connect", "failed")
		return res, hide(withResume(err, resume))
	}
	if cres.Declined {
		res.Declined = true
		mark("connect", "skipped")
		return res, nil
	}
	mark("connect", "done")
	return res, nil
}

// installRelease stages this version's release for the host in a private
// temporary directory, verified here, and installs it there.
func installRelease(ctx context.Context, target string, src release.Source, goarch string) error {
	work, err := os.MkdirTemp("", "backsight-ssh-")
	if err != nil {
		return blocked(err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	archive, _, err := stageRelease(ctx, src, buildinfo.Version, goarch, work)
	if err != nil {
		return err
	}
	_, err = installOnHost(ctx, target, work, archive)
	return err
}

// withResume is err as an *output.Error resumed by resume.
func withResume(err error, resume string) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		out := *oe
		out.Resume = resume
		return &out
	}
	return &output.Error{Code: output.Blocked, Message: err.Error(), Resume: resume}
}

// sameHost says whether url's host is the ssh target's host or the
// address the server opens on: this PC already joined that server.
func sameHost(url, target, addr string) bool {
	u, err := neturl.Parse(url)
	if err != nil {
		return false
	}
	h := u.Hostname()
	_, host, ok := strings.Cut(target, "@")
	if !ok {
		host = target
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.Equal(net.ParseIP(addr)) || ip.Equal(net.ParseIP(host))
	}
	return strings.EqualFold(h, host)
}

// redactingWriter masks the secrets of sec in each write: the host's
// progress lines, one write each.
type redactingWriter struct {
	w   io.Writer
	sec func() []config.Secrets
}

func (r redactingWriter) Write(p []byte) (int, error) {
	text := string(p)
	for _, s := range r.sec() {
		text = server.Redact(text, s)
	}
	if _, err := io.WriteString(r.w, text); err != nil {
		return 0, err
	}
	return len(p), nil
}
