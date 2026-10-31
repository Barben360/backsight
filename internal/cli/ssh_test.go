package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/testtool"
)

// fakeSSH answers sshRun in-process: it records each call and writes the
// stdout and stderr it is given, then returns code.
type fakeSSH struct {
	calls  [][]string
	stdins []string
	stdout string
	stderr string
	code   int
	err    error
}

func (f *fakeSSH) Run(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.calls = append(f.calls, args)
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.stdins = append(f.stdins, in)
	if f.err != nil {
		return -1, f.err
	}
	_, _ = io.WriteString(stdout, f.stdout)
	_, _ = io.WriteString(stderr, f.stderr)
	return f.code, nil
}

func withSSH(t *testing.T, f sshCLI) {
	t.Helper()
	old := sshRunner
	sshRunner = f
	t.Cleanup(func() { sshRunner = old })
}

// fakeSSHOnPath puts an executable `ssh` running body (sh) first on PATH.
func fakeSSHOnPath(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh is a POSIX shell script: Linux only")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// The pinned options refuse an unknown or changed host key, never prompt,
// forward nothing and run no local command; "--" ends the options so the
// target can never be read as one.
func TestSSHArgsPinTheSafeOptions(t *testing.T) {
	got := sshArgs("u@h", "x")
	joined := strings.Join(got, " ")
	for _, o := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "UpdateHostKeys=no", "VerifyHostKeyDNS=no",
		"ControlMaster=no", "ControlPath=none", "ConnectTimeout=20",
		"ServerAliveInterval=15", "ServerAliveCountMax=4", "ForwardAgent=no", "ForwardX11=no",
		"ClearAllForwardings=yes", "PermitLocalCommand=no", "Tunnel=no"} {
		i := slices.Index(got, o)
		if i < 1 || got[i-1] != "-o" {
			t.Errorf("option %s not passed with -o: %q", o, got)
		}
	}
	if !slices.Contains(got, "-T") {
		t.Errorf("no -T: %q", got)
	}
	if n := len(got); n < 3 || got[n-3] != "--" || got[n-2] != "u@h" || got[n-1] != "x" {
		t.Errorf("want ... -- u@h x at the end, got %q", got)
	}
	if slices.Index(got, "--") != len(got)-3 {
		t.Errorf("an argument follows -- besides the target and command: %q", got)
	}
	for _, bad := range []string{"accept-new", "StrictHostKeyChecking=no", "UpdateHostKeys=yes", "UpdateHostKeys=ask",
		"VerifyHostKeyDNS=yes", "ControlMaster=auto", "ControlPersist"} {
		if strings.Contains(joined, bad) {
			t.Errorf("args hold %s: %q", bad, got)
		}
	}
}

func TestCheckTargetRefusesOptionInjection(t *testing.T) {
	for _, bad := range []string{"-oProxyCommand=x", "", "a b", "u@h\n", "u@@h", "u@h@i", "u\th", "u@h\x00", "-h",
		"u@-oProxyCommand=x", "u$(x)@h", "h`x`", "h;id", "h|id", "u@'h'", `u@"h"`, "h&", "~u@h", "h/x", "u@h*", "u@[2001:db8::1]", "[2001:db8::1]", "u:x@h"} {
		if err := checkTarget(bad); output.CodeOf(err) != output.Usage {
			t.Errorf("checkTarget(%q) = %v (code %d), want exit 2", bad, err, output.CodeOf(err))
		}
	}
	for _, ok := range []string{"u@h", "h", "my-alias", "u@100.64.0.1", "u.name@host.example.com", "fd7a:115c:a1e0::1", "u@fd7a:115c:a1e0::1", "u@fe80::1%eth0"} {
		if err := checkTarget(ok); err != nil {
			t.Errorf("checkTarget(%q) = %v, want nil", ok, err)
		}
	}
	// sshRun checks before ssh runs: the refusing runner is never reached.
	f := &fakeSSH{}
	withSSH(t, f)
	if _, err := sshRun(context.Background(), "-oProxyCommand=x", "true", nil, io.Discard, nil); output.CodeOf(err) != output.Usage {
		t.Errorf("sshRun with an option as target: %v, want exit 2", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("ssh ran for a refused target: %q", f.calls)
	}
}

// execSSH runs the ssh found on PATH with the given argv, feeds it the
// given stdin and never this process's own, and returns its exit code.
func TestExecSSHRunsTheSystemSSH(t *testing.T) {
	dir := t.TempDir()
	ssh := testtool.Write(t, dir, "ssh", testtool.Fake{Stdout: "out-line\n", Stderr: "err-line\n", Code: 7,
		Script: "stdinto " + filepath.Join(dir, "stdin")})
	t.Setenv("PATH", dir)
	args := sshArgs("u@example.invalid", "echo hi")
	var out, errb bytes.Buffer
	code, err := execSSH{}.Run(context.Background(), args, strings.NewReader("secret-value\nline2"), &out, &errb)
	if err != nil || code != 7 {
		t.Fatalf("Run = %d, %v; want 7, nil", code, err)
	}
	if got := testtool.Calls(t, ssh); len(got) != 1 || !slices.Equal(got[0], args) {
		t.Errorf("argv = %q, want %q", got, args)
	}
	if in, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(in) != "secret-value\nline2" {
		t.Errorf("stdin = %q", in)
	}
	if out.String() != "out-line\n" || errb.String() != "err-line\n" {
		t.Errorf("stdout %q, stderr %q", out.String(), errb.String())
	}

	// nil stdin: ssh reads nothing, even when this process's stdin holds a
	// user's typing.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("typed-by-the-user\n")
	_ = w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin; _ = r.Close() })
	if code, err := (execSSH{}).Run(context.Background(), args, nil, io.Discard, io.Discard); err != nil || code != 7 {
		t.Fatalf("Run with nil stdin = %d, %v", code, err)
	}
	if in, _ := os.ReadFile(filepath.Join(dir, "stdin")); len(in) != 0 {
		t.Errorf("nil stdin reached ssh as %q", in)
	}

	// No ssh client: refused with exit 20, pointing at --print-commands.
	t.Setenv("PATH", t.TempDir())
	_, err = execSSH{}.Run(context.Background(), args, nil, io.Discard, io.Discard)
	if output.CodeOf(err) != output.Blocked || !strings.Contains(fmt.Sprint(err), "--print-commands") ||
		!strings.Contains(fmt.Sprint(err), "no ssh client") {
		t.Errorf("no ssh on PATH: %v (code %d)", err, output.CodeOf(err))
	}
}

// OpenSSH's own messages map to what the user must do (10) or what the
// environment refuses (20); backsight never offers to drop a host key.
func TestSSHFailureClassifies(t *testing.T) {
	changed := "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\r\n" +
		"@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\r\n" +
		"@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\r\n" +
		"IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!\r\n" +
		"Offending ED25519 key in /home/u/.ssh/known_hosts:3\r\n" +
		"  remove with:\r\n  ssh-keygen -f '/home/u/.ssh/known_hosts' -R 'h'\r\n" +
		"Host key for h has changed and you have requested strict checking.\r\n" +
		"Host key verification failed.\r\n"
	for _, tc := range []struct {
		name, stderr string
		code         int
		msg          []string
		actions      []string
	}{
		{"changed", changed, output.Blocked,
			[]string{"the host key of u@h changed since you last connected: an attack or a reinstalled host. Check with whoever runs it; backsight never accepts or removes a host key. Nothing was changed"}, nil},
		{"unknown", "No ED25519 host key is known for h and you have requested strict checking.\r\nHost key verification failed.\r\n", output.NeedsUser,
			[]string{"u@h's host key is not in your known_hosts and backsight never accepts an unknown one: connect once yourself, compare the fingerprint with the host's (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub there), then run this again"},
			[]string{"ssh u@h"}},
		{"denied", "u@h: Permission denied (publickey,password).\r\n", output.NeedsUser,
			[]string{"ssh could not log in to u@h without a prompt: load your key into the agent or install it on the host, then run this again"},
			[]string{"ssh-add", "ssh-copy-id u@h"}},
		{"revoked", "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\r\n" +
			"@       WARNING: REVOKED HOST KEY DETECTED!               @\r\n" +
			"@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\r\n" +
			"The ED25519 host key for h is marked as revoked.\r\n" +
			"This could mean that a stolen key is being used to\r\nimpersonate this host.\r\n" +
			"Host key verification failed.\r\n", output.Blocked,
			[]string{"the host key of u@h is marked revoked in your ssh configuration: an attack or a retired key. Check with whoever runs it; backsight never accepts or removes a host key. Nothing was changed"}, nil},
		{"denied-not-auth", "bash: /x: Permission denied\nConnection to h closed by remote host.\r\n", output.Blocked,
			[]string{"the ssh connection to u@h dropped (Connection to h closed by remote host.): the step on the host may have run in part; every step can run again, so rerun this"}, nil},
		{"dropped-reset", "client_loop: send disconnect: Connection reset by 100.64.0.2 port 22\r\n", output.Blocked,
			[]string{"the ssh connection to u@h dropped (client_loop: send disconnect: Connection reset by 100.64.0.2 port 22): the step on the host may have run in part; every step can run again, so rerun this"}, nil},
		{"dropped-pipe", "client_loop: send disconnect: Broken pipe\r\n", output.Blocked,
			[]string{"the ssh connection to u@h dropped (client_loop: send disconnect: Broken pipe): the step on the host may have run in part; every step can run again, so rerun this"}, nil},
		{"keepalive", "Timeout, server h not responding.\r\n", output.Blocked,
			[]string{"the ssh connection to u@h dropped (Timeout, server h not responding.): the step on the host may have run in part; every step can run again, so rerun this"}, nil},
		{"refused-at-old-kex", "ssh_exchange_identification: Connection closed by remote host\r\n", output.Blocked,
			[]string{"ssh could not reach u@h: ssh_exchange_identification: Connection closed by remote host"}, nil},
		{"refused-at-kex", "kex_exchange_identification: Connection closed by remote host\r\nConnection closed by 100.64.0.2 port 22\r\n", output.Blocked,
			[]string{"ssh could not reach u@h: Connection closed by 100.64.0.2 port 22"}, nil},
		{"unreachable", "ssh: connect to host h port 22: Connection refused\r\n\r\n", output.Blocked,
			[]string{"ssh could not reach u@h: ssh: connect to host h port 22: Connection refused"}, nil},
		{"dns", "ssh: Could not resolve hostname h: Name or service not known\n", output.Blocked,
			[]string{"ssh could not reach u@h: ssh: Could not resolve hostname h: Name or service not known"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sshFailure("u@h", tc.stderr)
			var oe *output.Error
			if !errors.As(err, &oe) {
				t.Fatalf("not an output.Error: %v", err)
			}
			if oe.Code != tc.code {
				t.Errorf("code %d, want %d", oe.Code, tc.code)
			}
			for _, m := range tc.msg {
				if oe.Message != m {
					t.Errorf("message %q\nwant    %q", oe.Message, m)
				}
			}
			var cmds []string
			for _, a := range oe.Actions {
				if a.Kind != "run_as_user" || a.Why == "" {
					t.Errorf("action %+v", a)
				}
				cmds = append(cmds, a.Cmd)
			}
			if !slices.Equal(cmds, tc.actions) {
				t.Errorf("actions %q, want %q", cmds, tc.actions)
			}
			if b := fmt.Sprintf("%+v", oe); strings.Contains(b, "ssh-keygen -R") || strings.Contains(b, "-R 'h'") || strings.Contains(b, "known_hosts:3") ||
				strings.Contains(b, "accept-new") {
				t.Errorf("the failure offers to drop or accept a host key: %s", b)
			}
		})
	}
}

// The script and every argument reach the remote sh verbatim, whatever
// they hold; $0 names the step.
func TestRemoteScriptQuoting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	cmd, err := remoteScript("t", `printf "%s\n" "$0" "$@"`, "/home/a b/x", "it's", "$HOME", "a=b", "*")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	if want := "backsight-t\n/home/a b/x\nit's\n$HOME\na=b\n*\n"; string(out) != want {
		t.Errorf("sh -c %s\nprinted %q\nwant    %q", cmd, out, want)
	}
	if got, err := remoteShim("/opt/b s/backsight", "secret", "set", "llm_api_key"); err != nil || got != "'/opt/b s/backsight' secret set llm_api_key" {
		t.Errorf("remoteShim = %q, %v", got, err)
	}
}

// A word fish, csh or zsh would read otherwise than sh never reaches the
// host: refused with exit 2 before any ssh call, saying why.
func TestRemoteWordsRefuseShellSurprises(t *testing.T) {
	for _, bad := range []string{`C:\x`, "a\nb", "a\rb", "a\x00b", "\x1b[31m", "=ls", "%1", "%job", "a!b", "!!", "x!"} {
		if _, err := remoteScript("t", "true", "ok", bad); output.CodeOf(err) != output.Usage ||
			!strings.Contains(err.Error(), "backslash") || !strings.Contains(err.Error(), "login shell") {
			t.Errorf("remoteScript arg %q: %v", bad, err)
		}
		if _, err := remoteShim("/x/backsight", "secret", bad); output.CodeOf(err) != output.Usage {
			t.Errorf("remoteShim arg %q: %v", bad, err)
		}
		if _, err := remoteShim(bad, "version"); output.CodeOf(err) != output.Usage {
			t.Errorf("remoteShim shim %q: %v", bad, err)
		}
	}
	for _, step := range []string{"a b", "x;y", "=x", ""} {
		if _, err := remoteScript(step, "true"); err == nil {
			t.Errorf("remoteScript step %q accepted", step)
		}
	}
	if _, err := remoteScript("t", "true", "a=b", "/p/x y", "it's", "50%", "a%b", "a==b"); err != nil {
		t.Errorf("plain words refused: %v", err)
	}
}

// sshRun keeps the tail of ssh's stderr, streams each line, and tells
// ssh's own failures (255) from the remote command's exit.
func TestSSHRunMapsExitsAndKeepsTheTail(t *testing.T) {
	var lines []string
	for i := range 45 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	f := &fakeSSH{stdout: "hello\n", stderr: strings.Join(lines, "\r\n") + "\npartial", code: 0}
	withSSH(t, f)
	var out bytes.Buffer
	var seen []string
	tail, err := sshRun(context.Background(), "u@h", "backsight version", strings.NewReader("in"), &out,
		func(l []byte) { seen = append(seen, string(l)) })
	if err != nil {
		t.Fatalf("sshRun: %v", err)
	}
	if want := append(slices.Clone(lines), "partial"); !slices.Equal(seen, want) {
		t.Errorf("lines handed over %q", seen)
	}
	if want := strings.Join(append(slices.Clone(lines[6:]), "partial"), "\n"); tail != want {
		t.Errorf("tail = %q\nwant   %q", tail, want)
	}
	if out.String() != "hello\n" || f.stdins[0] != "in" || !slices.Equal(f.calls[0], sshArgs("u@h", "backsight version")) {
		t.Errorf("stdout %q, stdin %q, args %q", out.String(), f.stdins[0], f.calls[0])
	}

	f.stderr, f.code = "u@h: Permission denied (publickey).\n", 255
	if _, err := sshRun(context.Background(), "u@h", "true", nil, io.Discard, nil); output.CodeOf(err) != output.NeedsUser ||
		!strings.Contains(err.Error(), "could not log in to u@h") {
		t.Errorf("255: %v", err)
	}

	f.stderr, f.code = "backsight: setup failed\n", 3
	_, err = sshRun(context.Background(), "u@h", "true", nil, io.Discard, nil)
	var re *remoteExit
	if !errors.As(err, &re) || re.Code != 3 || re.Tail != "backsight: setup failed" ||
		!strings.Contains(err.Error(), "backsight: setup failed") {
		t.Errorf("remote exit: %#v", err)
	}

	f.err = errors.New("could not start")
	if _, err := sshRun(context.Background(), "u@h", "true", nil, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "could not start") {
		t.Errorf("start failure: %v", err)
	}
}

// A stderr that never ends a line is handed over in bounded pieces: the
// memory held stays capped and nothing is lost.
func TestSSHRunCapsALongStderrLine(t *testing.T) {
	long := strings.Repeat("x", 3*stderrLineMax+100)
	se := &stderrLines{}
	for i := 0; i < len(long); i += 1000 {
		_, _ = se.Write([]byte(long[i:min(i+1000, len(long))]))
		if len(se.buf) >= stderrLineMax || cap(se.buf) > 2*stderrLineMax {
			t.Fatalf("buffer grew to %d (cap %d)", len(se.buf), cap(se.buf))
		}
	}

	f := &fakeSSH{stderr: long + "\nend", code: 0}
	withSSH(t, f)
	var got []string
	tail, err := sshRun(context.Background(), "u@h", "true", nil, io.Discard, func(l []byte) { got = append(got, string(l)) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[4] != "end" || strings.Join(got[:4], "") != long {
		t.Fatalf("handed %d lines", len(got))
	}
	for _, l := range got {
		if len(l) > stderrLineMax {
			t.Errorf("a line of %d bytes", len(l))
		}
	}
	if !strings.HasSuffix(tail, "\nend") {
		t.Errorf("tail ends %q", tail[max(0, len(tail)-20):])
	}
}
