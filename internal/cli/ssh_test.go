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
		t.Skip("the fake ssh is a shell script")
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
	for _, o := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "UpdateHostKeys=no", "ConnectTimeout=20",
		"ServerAliveInterval=15", "ServerAliveCountMax=4", "ForwardAgent=no", "ForwardX11=no",
		"ClearAllForwardings=yes", "PermitLocalCommand=no"} {
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
	for _, bad := range []string{"accept-new", "StrictHostKeyChecking=no", "UpdateHostKeys=yes", "UpdateHostKeys=ask"} {
		if strings.Contains(joined, bad) {
			t.Errorf("args hold %s: %q", bad, got)
		}
	}
}

func TestCheckTargetRefusesOptionInjection(t *testing.T) {
	for _, bad := range []string{"-oProxyCommand=x", "", "a b", "u@h\n", "u@@h", "u@h@i", "u\th", "u@h\x00", "-h"} {
		if err := checkTarget(bad); output.CodeOf(err) != output.Usage {
			t.Errorf("checkTarget(%q) = %v (code %d), want exit 2", bad, err, output.CodeOf(err))
		}
	}
	for _, ok := range []string{"u@h", "h", "my-alias", "u@100.64.0.1"} {
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
	dir := fakeSSHOnPath(t, `log="$(dirname "$0")"
printf '%s\n' "$@" > "$log/argv"
cat > "$log/stdin"
echo out-line
echo err-line >&2
exit 7`)
	args := sshArgs("u@example.invalid", "echo hi")
	var out, errb bytes.Buffer
	code, err := execSSH{}.Run(context.Background(), args, strings.NewReader("secret-value\nline2"), &out, &errb)
	if err != nil || code != 7 {
		t.Fatalf("Run = %d, %v; want 7, nil", code, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if got := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"); !slices.Equal(got, args) {
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
			if b := fmt.Sprintf("%+v", oe); strings.Contains(b, "ssh-keygen -R") || strings.Contains(b, "-R 'h'") ||
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
	cmd := remoteScript("t", `printf "%s\n" "$0" "$@"`, "/home/a b/x", "it's", "$HOME")
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	if want := "backsight-t\n/home/a b/x\nit's\n$HOME\n"; string(out) != want {
		t.Errorf("sh -c %s\nprinted %q\nwant    %q", cmd, out, want)
	}
	if got := remoteShim("/opt/b s/backsight", "secret", "set", "llm_api_key"); got != "'/opt/b s/backsight' secret set llm_api_key" {
		t.Errorf("remoteShim = %q", got)
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
