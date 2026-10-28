package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Barben360/backsight/internal/output"
)

// sshCLI runs the system ssh (decision 0024). Tests replace it.
type sshCLI interface {
	// Run returns ssh's exit code; err only when ssh could not start.
	Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int, err error)
}

type execSSH struct{}

func (execSSH) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...)
	// Never this process's stdin: it could hold a user's typing or a piped key.
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.Is(err, exec.ErrNotFound):
		return -1, &output.Error{Code: output.Blocked,
			Message: "this PC has no ssh client (OpenSSH): install it, or print the steps with backsight server install --print-commands"}
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return ee.ExitCode(), nil
	case ctx.Err() != nil:
		return -1, ctx.Err()
	}
	return -1, err
}

var sshRunner sshCLI = execSSH{}

// sshOptions precede every target; command-line options win over the
// user's ~/.ssh/config, which still applies to everything else (User,
// Port, IdentityFile, ProxyJump). No multiplexing: a master connection
// already open would skip the host key check, and one backsight opened
// would outlive it. No SSHFP: a key absent from known_hosts is refused
// even when DNS vouches for it.
var sshOptions = []string{"-T",
	"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no",
	"-o", "VerifyHostKeyDNS=no", "-o", "ControlMaster=no", "-o", "ControlPath=none",
	"-o", "ConnectTimeout=20", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4",
	"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no"}

// sshTarget is [user@]host: letters, digits and . _ - in both, and : % in
// the host for an IPv6 address, unbracketed (ssh strips brackets only in
// ssh:// URIs).
var sshTarget = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9._:%-]+$`)

// checkTarget refuses a target ssh could read as an option or a shell
// could expand: one [user@]host or host alias only.
func checkTarget(target string) error {
	why := ""
	_, host, hasUser := strings.Cut(target, "@")
	switch {
	case target == "":
		why = "it is empty"
	case strings.HasPrefix(target, "-") || hasUser && strings.HasPrefix(host, "-"):
		why = "it starts with '-', as an ssh option does"
	case strings.IndexFunc(target, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		why = "it holds a space or a control character"
	case strings.Count(target, "@") > 1:
		why = "it holds more than one '@'"
	case !sshTarget.MatchString(target):
		why = "it holds a character other than letters, digits, '.', '_', '-' (and ':', '%' in an IPv6 host)"
	default:
		return nil
	}
	return &output.Error{Code: output.Usage, Message: fmt.Sprintf("the ssh target %q is not user@host or a host alias: %s", target, why)}
}

// sshArgs is ssh's argv for running remote on target.
func sshArgs(target, remote string) []string {
	return append(append([]string{}, sshOptions...), "--", target, remote)
}

// checkRemoteWord refuses a word the host's login shell could read
// otherwise than sh does: a backslash or a control character (fish, csh)
// or a leading '=' (zsh's =cmd expansion).
func checkRemoteWord(w string) error {
	if !strings.ContainsRune(w, '\\') && strings.IndexFunc(w, unicode.IsControl) < 0 && !strings.HasPrefix(w, "=") {
		return nil
	}
	return &output.Error{Code: output.Usage, Message: fmt.Sprintf("%q cannot be passed to the host: it holds a backslash or a control character (a newline too) or starts with '=', which the host's login shell (fish, csh, zsh) could read otherwise than sh", w)}
}

// remoteScript runs script under sh on the host, whatever the user's login
// shell, with $0 naming the step and args as $1….
func remoteScript(step, script string, args ...string) (string, error) {
	if shellWord(step) != step || checkRemoteWord(step) != nil {
		return "", fmt.Errorf("remote step %q is not one plain word", step)
	}
	cmd := "sh -c " + shellQuote(script) + " backsight-" + step
	for _, a := range args {
		if err := checkRemoteWord(a); err != nil {
			return "", err
		}
		cmd += " " + shellWord(a)
	}
	return cmd, nil
}

// remoteShim is the command line running the host's shim with args.
func remoteShim(shim string, args ...string) (string, error) {
	words := make([]string, 0, 1+len(args))
	for _, a := range append([]string{shim}, args...) {
		if err := checkRemoteWord(a); err != nil {
			return "", err
		}
		words = append(words, shellWord(a))
	}
	return strings.Join(words, " "), nil
}

// remoteExit is the remote command's own non-zero exit.
type remoteExit struct {
	Code int
	Tail string
}

func (e *remoteExit) Error() string {
	if e.Tail == "" {
		return fmt.Sprintf("the command on the host exited with code %d", e.Code)
	}
	return fmt.Sprintf("the command on the host exited with code %d:\n%s", e.Code, e.Tail)
}

// sshTailLines bounds the stderr kept for an error.
const sshTailLines = 40

// stderrLineMax bounds a line kept in memory: a longer one is handed over
// in pieces.
const stderrLineMax = 64 << 10

// stderrLines splits ssh's stderr into lines, hands each to on and keeps
// the last sshTailLines.
type stderrLines struct {
	mu   sync.Mutex
	buf  []byte
	tail []string
	on   func(line []byte)
}

func (s *stderrLines) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		if i := bytes.IndexByte(s.buf, '\n'); i >= 0 && i <= stderrLineMax {
			s.line(s.buf[:i])
			s.buf = s.buf[i+1:]
		} else if len(s.buf) >= stderrLineMax {
			s.line(s.buf[:stderrLineMax])
			s.buf = s.buf[stderrLineMax:]
		} else {
			break
		}
	}
	s.buf = append([]byte(nil), s.buf...) // drop what was handed over
	return len(p), nil
}

func (s *stderrLines) line(l []byte) {
	l = bytes.TrimSuffix(l, []byte("\r"))
	if s.on != nil {
		s.on(l)
	}
	s.tail = append(s.tail, string(l))
	if len(s.tail) > sshTailLines {
		s.tail = s.tail[len(s.tail)-sshTailLines:]
	}
}

func (s *stderrLines) close() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) > 0 {
		s.line(s.buf)
		s.buf = nil
	}
	return strings.Join(s.tail, "\n")
}

// sshRun runs remote on target through sshRunner. tail is the last lines
// of ssh's stderr; err is ssh's own failure (exit 255, mapped by
// sshFailure) or the remote command's non-zero exit (*remoteExit).
func sshRun(ctx context.Context, target, remote string, stdin io.Reader, stdout io.Writer, onStderrLine func(line []byte)) (tail string, err error) {
	if err := checkTarget(target); err != nil {
		return "", err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	se := &stderrLines{on: onStderrLine}
	code, err := sshRunner.Run(ctx, sshArgs(target, remote), stdin, stdout, se)
	tail = se.close()
	switch {
	case err != nil:
		return tail, err
	case code == 255:
		return tail, sshFailure(target, tail)
	case code != 0:
		return tail, &remoteExit{Code: code, Tail: tail}
	}
	return tail, nil
}

// sshFailure maps ssh's own failure, from its stderr, to what the user must
// do (NeedsUser) or what the environment refuses (Blocked).
func sshFailure(target, stderr string) error {
	last := ""
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			last = l
		}
	}
	switch {
	case strings.Contains(stderr, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return &output.Error{Code: output.Blocked,
			Message: "the host key of " + target + " changed since you last connected: an attack or a reinstalled host. Check with whoever runs it; backsight never accepts or removes a host key. Nothing was changed"}
	case strings.Contains(stderr, "REVOKED HOST KEY DETECTED"):
		return &output.Error{Code: output.Blocked,
			Message: "the host key of " + target + " is marked revoked in your ssh configuration: an attack or a retired key. Check with whoever runs it; backsight never accepts or removes a host key. Nothing was changed"}
	case strings.Contains(stderr, "Host key verification failed"):
		return &output.Error{Code: output.NeedsUser,
			Message: target + "'s host key is not in your known_hosts and backsight never accepts an unknown one: connect once yourself, compare the fingerprint with the host's (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub there), then run this again",
			Actions: []output.Action{{Kind: "run_as_user", Cmd: "ssh " + shellWord(target), Why: "accept the host key yourself once its fingerprint matches the host's"}}}
	case strings.Contains(stderr, "Permission denied ("):
		return &output.Error{Code: output.NeedsUser,
			Message: "ssh could not log in to " + target + " without a prompt: load your key into the agent or install it on the host, then run this again",
			Actions: []output.Action{
				{Kind: "run_as_user", Cmd: "ssh-add", Why: "load your key into the ssh agent"},
				{Kind: "run_as_user", Cmd: "ssh-copy-id " + shellWord(target), Why: "install your public key on the host"}}}
	case !strings.Contains(stderr, "kex_exchange_identification") && !strings.Contains(stderr, "ssh_exchange_identification") &&
		(strings.Contains(stderr, "closed by remote host") || strings.Contains(stderr, "Connection reset") ||
			strings.Contains(stderr, "Broken pipe") || strings.Contains(stderr, "Timeout, server ")):
		// The session was up: the step on the host may have run in part.
		return &output.Error{Code: output.Blocked,
			Message: "the ssh connection to " + target + " dropped (" + last + "): the step on the host may have run in part; every step can run again, so rerun this"}
	}
	msg := "ssh could not reach " + target
	if last != "" {
		msg += ": " + last
	}
	return &output.Error{Code: output.Blocked, Message: msg}
}
