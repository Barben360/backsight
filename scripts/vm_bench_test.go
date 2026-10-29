package scripts

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The fakes of scripts/vm-bench.sh's tests: each logs its argv, one line a
// call, to <fakes>/<name>.calls. virsh answers `list --all --name` from
// <fakes>/domains and `vol-list` from <fakes>/vols (one name a line),
// net-dhcp-leases --mac from the rows of <fakes>/leases with that MAC,
// pool-info's Available from <fakes>/pool-avail (default 1.1 TB), net-info,
// domstate, snapshot-list and domblklist as a healthy network and pool
// and a running domain with its bs-m6-clean snapshot and a seed cdrom
// would; every other verb succeeds silently.
var benchFakes = map[string]string{
	"virsh": `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/virsh.calls"
[ "$1" = -c ] && shift 2
case "$1" in
  list) [ ! -e "$d/domains" ] || cat "$d/domains"; echo ;;
  vol-list)
    echo ' Name                     Path'
    echo '------------------------------------------------------------'
    if [ -e "$d/vols" ]; then while read -r v; do echo " $v   /var/lib/libvirt/images/$v"; done < "$d/vols"; fi
    echo ;;
  net-info) printf 'Name:           default\nActive:         yes\n' ;;
  pool-info)
    avail=1196613906432
    [ ! -e "$d/pool-avail" ] || avail=$(cat "$d/pool-avail")
    printf 'Name:           default\nState:          running\nAvailable:      %s\n' "$avail" ;;
  snapshot-list) printf 'bs-m6-clean\n\n' ;;
  domblklist)
    printf ' Type   Device   Target   Source\n------------------------------------\n'
    printf ' file   disk     vda      /var/lib/libvirt/images/%s.qcow2\n' "$2"
    printf ' file   cdrom    sda      /var/lib/libvirt/images/%s-seed.iso\n\n' "$2" ;;
  domstate) printf 'running\n\n' ;;
  net-dhcp-leases)
    echo ' Expiry Time           MAC address         Protocol   IP address           Hostname       Client ID or DUID'
    echo '------------------------------------------------------------------------------------------------------------'
    [ ! -e "$d/leases" ] || grep -F -- " $4 " "$d/leases"
    echo ;;
esac
exit 0
`,
	"virt-install": `#!/bin/sh
printf '%s\n' "$*" >> "$(dirname "$0")/virt-install.calls"
case "$*" in *"--osinfo list"*) printf 'ubuntu24.10, ubuntuoracular\nubuntu24.04, ubuntunoble\n' ;; esac
exit 0
`,
	"ssh": `#!/bin/sh
printf '%s\n' "$*" >> "$(dirname "$0")/ssh.calls"
exit 0
`,
	"scp":      "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$(dirname \"$0\")/scp.calls\"\nexit 0\n",
	"curl":     "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$(dirname \"$0\")/curl.calls\"\nexit 1\n",
	"xorriso":  "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$(dirname \"$0\")/xorriso.calls\"\nexit 0\n",
	"minisign": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$(dirname \"$0\")/minisign.calls\"\nexit 0\n",
}

func newBenchFakes(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for name, body := range benchFakes {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// benchCalls returns the argv lines a fake logged.
func benchCalls(t *testing.T, fakes, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fakes, name+".calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// vmBench sources scripts/vm-bench.sh under set -euo pipefail, with the
// fakes first on PATH, HOME and BS_M6_DIR as given and off CI (unless env
// says otherwise), then runs script; it returns stdout, stderr and the
// exit code.
func vmBench(t *testing.T, fakes, home, benchDir, script string, env ...string) (stdout, stderr string, code int) {
	t.Helper()
	path := mustAbs(t, "vm-bench.sh")
	// Read here so Go's test cache keys on the script (what a child reads
	// is not tracked).
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . "$1"; `+script, "vb", path)
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "CI", "GITHUB_ACTIONS", "HOME", "PATH", "BS_M6_DIR", "XDG_STATE_HOME":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, "PATH="+fakes+":"+os.Getenv("PATH"), "HOME="+home, "BS_M6_DIR="+benchDir)
	cmd.Env = append(cmd.Env, env...)
	cmd.Dir = t.TempDir()
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return o.String(), e.String(), ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String(), 0
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// virshVerbs maps each logged virsh call to "<verb> <args>", checking that
// every call names the bench's URI.
func virshVerbs(t *testing.T, calls []string) []string {
	t.Helper()
	var out []string
	for _, c := range calls {
		rest, ok := strings.CutPrefix(c, "-c qemu:///system ")
		if !ok {
			t.Errorf("a virsh call without -c qemu:///system: %q", c)
			continue
		}
		out = append(out, rest)
	}
	return out
}

// RF 3: down removes only what is named bs-m6-<…>: the domains destroyed
// and undefined with their snapshots, the volumes deleted, nothing else
// called that changes libvirt; the bench directory goes but results/ and
// cache/ (--purge: cache/ too).
func TestVMBenchDownRemovesOnlyItsOwn(t *testing.T) {
	for _, purge := range []bool{false, true} {
		fakes := newBenchFakes(t)
		writeLines(t, filepath.Join(fakes, "domains"), "bs-m6-server", "bs-m6-client", "bs-m6x", "win11")
		writeLines(t, filepath.Join(fakes, "vols"), "bs-m6-server.qcow2", "bs-m6-client-seed.iso", "bs-m6-base-noble.qcow2", "bs-m6.qcow2", "other.qcow2")
		bench := filepath.Join(t.TempDir(), "bs-m6")
		for _, d := range []string{"results", "cache", "work", "client-b", "hostkeys/server"} {
			if err := os.MkdirAll(filepath.Join(bench, d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		writeLines(t, filepath.Join(bench, "results", "run.jsonl"), "{}")
		writeLines(t, filepath.Join(bench, "cache", "image.img"), "x")
		writeLines(t, filepath.Join(bench, "key"), "k")

		call := "vb_down"
		if purge {
			call = "vb_down --purge"
		}
		stdout, stderr, code := vmBench(t, fakes, t.TempDir(), bench, call)
		if code != 0 {
			t.Fatalf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", call, code, stdout, stderr)
		}

		var mutating []string
		for _, v := range virshVerbs(t, benchCalls(t, fakes, "virsh")) {
			switch strings.Fields(v)[0] {
			case "list", "vol-list", "domstate":
				continue
			}
			mutating = append(mutating, v)
		}
		sort.Strings(mutating)
		want := []string{
			"destroy bs-m6-client",
			"destroy bs-m6-server",
			"undefine bs-m6-client --nvram --snapshots-metadata",
			"undefine bs-m6-server --nvram --snapshots-metadata",
			"vol-delete --pool default bs-m6-base-noble.qcow2",
			"vol-delete --pool default bs-m6-client-seed.iso",
			"vol-delete --pool default bs-m6-server.qcow2",
		}
		if strings.Join(mutating, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: the changing virsh calls are\n%s\nwant\n%s", call, strings.Join(mutating, "\n"), strings.Join(want, "\n"))
		}
		for _, n := range []string{"bs-m6-server", "bs-m6-client", "bs-m6-server.qcow2", "bs-m6-client-seed.iso", "bs-m6-base-noble.qcow2"} {
			if !strings.Contains(stdout, n) {
				t.Errorf("%s does not say it removed %s:\n%s", call, n, stdout)
			}
		}

		if _, err := os.Stat(filepath.Join(bench, "results", "run.jsonl")); err != nil {
			t.Errorf("%s: results/ not kept: %v", call, err)
		}
		_, err := os.Stat(filepath.Join(bench, "cache", "image.img"))
		if purge && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("down --purge kept cache/ (%v)", err)
		}
		if !purge && err != nil {
			t.Errorf("down removed cache/: %v", err)
		}
		for _, gone := range []string{"work", "client-b", "hostkeys", "key"} {
			if _, err := os.Stat(filepath.Join(bench, gone)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s left %s (%v)", call, gone, err)
			}
		}
	}
}

// down empties no directory that is not named bs-m6: a BS_M6_DIR pointed
// at the wrong place loses nothing.
func TestVMBenchDownRefusesADirectoryNotNamedForTheBench(t *testing.T) {
	fakes := newBenchFakes(t)
	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "precious"), "x")
	_, stderr, code := vmBench(t, fakes, t.TempDir(), dir, "vb_down")
	if code == 0 {
		t.Fatalf("vb_down with BS_M6_DIR=%s succeeded", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "precious")); err != nil {
		t.Errorf("the directory lost its file: %v", err)
	}
	if !strings.Contains(stderr, "bs-m6") {
		t.Errorf("stderr does not say why:\n%s", stderr)
	}
	if calls := benchCalls(t, fakes, "virsh"); calls != nil {
		t.Errorf("virsh was called: %q", calls)
	}
}

// RF 1: the bench never reads or writes the user's ~/.ssh or ~/.claude:
// every ssh goes through its own config, which trusts only the host keys
// it generated (no first use) and offers only its own key.
func TestVMBenchNeverTouchesTheUsersSSHOrClaude(t *testing.T) {
	fakes := newBenchFakes(t)
	home := t.TempDir()
	user := map[string]string{
		".ssh/known_hosts":      "github.com ssh-ed25519 AAAAuser\n",
		".ssh/config":           "Host *\n  User me\n",
		".claude/settings.json": `{"hooks":{}}`,
	}
	for p, body := range user {
		if err := os.MkdirAll(filepath.Join(home, filepath.Dir(p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, p), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bench := filepath.Join(t.TempDir(), "bs-m6")
	cfg := filepath.Join(bench, "ssh_config")
	stdout, stderr, code := vmBench(t, fakes, home, bench,
		`vb_write_ssh_config 192.168.122.10 192.168.122.11; cp "$BS_M6_DIR/ssh_config" "$BS_M6_DIR.config.copy"; vb_ssh bs-m6-server true; vb_ssh client-bs true; vb_down`)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	for p, body := range user {
		got, err := os.ReadFile(filepath.Join(home, p))
		if err != nil || string(got) != body {
			t.Errorf("~/%s changed: %q, %v", p, got, err)
		}
	}
	var files []string
	if err := filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) != len(user) {
		t.Errorf("HOME holds new files: %q", files)
	}

	calls := benchCalls(t, fakes, "ssh")
	wantCalls := []string{"-F " + cfg + " bs-m6-server true", "-F " + cfg + " bs-m6-client-bs true"}
	if strings.Join(calls, "\n") != strings.Join(wantCalls, "\n") {
		t.Errorf("ssh calls:\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(wantCalls, "\n"))
	}
	if calls := benchCalls(t, fakes, "scp"); calls != nil {
		t.Errorf("scp was called: %q", calls)
	}

	b, err := os.ReadFile(bench + ".config.copy")
	if err != nil {
		t.Fatal(err)
	}
	conf := string(b)
	want := `Host bs-m6-server bs-m6-server-bs
  HostName 192.168.122.10
  HostKeyAlias bs-m6-server
Host bs-m6-client bs-m6-client-bs
  HostName 192.168.122.11
  HostKeyAlias bs-m6-client
Host bs-m6-server bs-m6-client
  User ubuntu
Host bs-m6-server-bs bs-m6-client-bs
  User bs
Host *
  IdentityFile ` + bench + `/key
  IdentitiesOnly yes
  IdentityAgent none
  UserKnownHostsFile ` + bench + `/known_hosts
  GlobalKnownHostsFile /dev/null
  StrictHostKeyChecking yes
  UpdateHostKeys no
  CheckHostIP no
  BatchMode yes
  ForwardAgent no
  ConnectTimeout 10
`
	if conf != want {
		t.Errorf("ssh_config:\n%s\nwant\n%s", conf, want)
	}
	for _, bad := range []string{"accept-new", "StrictHostKeyChecking no", "~/.ssh", home} {
		if strings.Contains(conf, bad) {
			t.Errorf("ssh_config holds %q", bad)
		}
	}
}

// The VMs get the bench's host keys (none generated on first boot), its
// login key for both users, bs's login shell (tcsh on the server: the
// install must run through a non-POSIX shell) and the client's netem
// modules; known_hosts names each VM by its alias with that host key.
func TestVMBenchUserDataPinsKeysAndShells(t *testing.T) {
	fakes := newBenchFakes(t)
	bench := filepath.Join(t.TempDir(), "bs-m6")
	stdout, stderr, code := vmBench(t, fakes, t.TempDir(), bench,
		`vb_user_data server > "$BS_M6_DIR.server.yaml"; vb_user_data client > "$BS_M6_DIR.client.yaml"; vb_write_ssh_config 192.168.122.10 192.168.122.11`)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	server := read(bench + ".server.yaml")
	client := read(bench + ".client.yaml")
	login := strings.TrimSpace(read(filepath.Join(bench, "key.pub")))
	hostPub := func(vm string) string {
		f := strings.Fields(read(filepath.Join(bench, "hostkeys", vm, "ssh_host_ed25519_key.pub")))
		return f[0] + " " + f[1]
	}
	// The private key's third line: the first base64 line is the same in
	// every OpenSSH ed25519 key, the second starts the public key.
	hostPriv := func(vm string) string {
		return strings.Split(read(filepath.Join(bench, "hostkeys", vm, "ssh_host_ed25519_key")), "\n")[3]
	}
	if hostPub("server") == hostPub("client") || hostPub("server") == login {
		t.Fatal("the server, the client and the login share a key")
	}

	for vm, ud := range map[string]string{"server": server, "client": client} {
		other := map[string]string{"server": "client", "client": "server"}[vm]
		if !strings.HasPrefix(ud, "#cloud-config\n") {
			t.Errorf("%s: no #cloud-config header", vm)
		}
		for _, s := range []string{"ssh_deletekeys: true", "ssh_genkeytypes: [ed25519]", "ssh_pwauth: false", "  ed25519_public: " + hostPub(vm), "    " + hostPriv(vm), "enable-linger, bs]", "sudo: ALL=(ALL) NOPASSWD:ALL", "  - name: ubuntu", "  - name: bs"} {
			if !strings.Contains(ud, s) {
				t.Errorf("%s user-data lacks %q:\n%s", vm, s, ud)
			}
		}
		if strings.Contains(ud, hostPub(other)) || strings.Contains(ud, hostPriv(other)) {
			t.Errorf("%s user-data holds the %s's host key", vm, other)
		}
		if n := strings.Count(ud, "      - "+login+"\n"); n != 2 {
			t.Errorf("%s: the bench login key is authorized %d times, want 2 (ubuntu and bs)", vm, n)
		}
	}
	bs := regexp.MustCompile(`(?s)  - name: bs\n(.*?)\n[a-z_]+:`)
	srvBS, cliBS := bs.FindStringSubmatch(server), bs.FindStringSubmatch(client)
	if srvBS == nil || !strings.Contains(srvBS[1], "    shell: /usr/bin/tcsh\n") || !strings.Contains(srvBS[1], "    groups: [docker]\n") {
		t.Errorf("server: bs is not a tcsh user in docker:\n%s", server)
	}
	if cliBS == nil || !strings.Contains(cliBS[1], "    shell: /bin/bash\n") || strings.Contains(cliBS[1], "docker") {
		t.Errorf("client: bs is not a bash user outside docker:\n%s", client)
	}
	for _, p := range []string{"docker.io", "docker-compose-v2", "tcsh", "csh", "dash", "zsh", "fish", "jq"} {
		if !strings.Contains(server, "\n  - "+p+"\n") {
			t.Errorf("server: package %s missing", p)
		}
	}
	for _, s := range []string{"\n  - jq\n", "\n  - curl\n", "linux-modules-extra-$(uname -r)", "ssh-keygen -q -t ed25519 -N '' -f ~/.ssh/id_ed25519"} {
		if !strings.Contains(client, s) {
			t.Errorf("client user-data lacks %q:\n%s", s, client)
		}
	}
	if strings.Contains(client, "docker") || strings.Contains(server, "linux-modules-extra") {
		t.Error("the server's packages and the client's mixed")
	}

	kh := read(filepath.Join(bench, "known_hosts"))
	wantKH := "bs-m6-server " + hostPub("server") + "\nbs-m6-client " + hostPub("client") + "\n"
	if kh != wantKH {
		t.Errorf("known_hosts:\n%s\nwant\n%s", kh, wantKH)
	}
	for _, p := range []string{"", "key", "hostkeys/server/ssh_host_ed25519_key", "known_hosts", "ssh_config"} {
		info, err := os.Stat(filepath.Join(bench, p))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s/%s is %v: readable by others", bench, p, info.Mode().Perm())
		}
	}
}

// up never stacks a second bench on the first (reset or down first), and
// nothing of the bench runs on CI: both refuse with exit 2 before
// creating anything.
func TestVMBenchUpRefusesAnExistingBenchOrCI(t *testing.T) {
	changing := func(t *testing.T, fakes string) []string {
		t.Helper()
		var out []string
		for _, v := range virshVerbs(t, benchCalls(t, fakes, "virsh")) {
			switch strings.Fields(v)[0] {
			case "list", "vol-list", "net-info", "pool-info", "domstate":
				continue
			}
			out = append(out, v)
		}
		for _, c := range benchCalls(t, fakes, "virt-install") {
			if c != "--osinfo list" {
				out = append(out, "virt-install "+c)
			}
		}
		return out
	}

	fakes := newBenchFakes(t)
	writeLines(t, filepath.Join(fakes, "domains"), "win11", "bs-m6-server")
	_, stderr, code := vmBench(t, fakes, t.TempDir(), filepath.Join(t.TempDir(), "bs-m6"), "vb_main up")
	if code != 2 || !strings.Contains(stderr, "reset") || !strings.Contains(stderr, "down") || !strings.Contains(stderr, "bs-m6-server") {
		t.Errorf("up over an existing bench: exit %d, stderr:\n%s", code, stderr)
	}
	if c := changing(t, fakes); c != nil {
		t.Errorf("up over an existing bench called %q", c)
	}

	// Positive control: without the bench domain, the same up goes past
	// that check (and stops at the next one, too little memory here).
	fakes = newBenchFakes(t)
	writeLines(t, filepath.Join(fakes, "domains"), "win11", "bs-m6x")
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	writeLines(t, meminfo, "MemTotal:       32000000 kB", "MemAvailable:    8000000 kB")
	_, stderr, code = vmBench(t, fakes, t.TempDir(), filepath.Join(t.TempDir(), "bs-m6"), "vb_main up", "VB_MEMINFO="+meminfo)
	if code != 2 || strings.Contains(stderr, "reset") || !strings.Contains(stderr, "MemAvailable") {
		t.Errorf("up without a bench, low memory: exit %d, stderr:\n%s", code, stderr)
	}
	if c := changing(t, fakes); c != nil {
		t.Errorf("up refused for memory called %q", c)
	}
	// A leftover volume of a failed up is the bench's too: down first.
	writeLines(t, filepath.Join(fakes, "vols"), "bs-m6-base-noble.qcow2", "other.qcow2")
	_, stderr, code = vmBench(t, fakes, t.TempDir(), filepath.Join(t.TempDir(), "bs-m6"), "vb_main up")
	if code != 2 || !strings.Contains(stderr, "bs-m6-base-noble.qcow2") || !strings.Contains(stderr, "down") {
		t.Errorf("up over a leftover volume: exit %d, stderr:\n%s", code, stderr)
	}

	for _, ci := range []string{"CI=true", "GITHUB_ACTIONS=true"} {
		for _, cmd := range []string{"up", "down", "reset", "status", "ssh server"} {
			fakes := newBenchFakes(t)
			_, stderr, code := vmBench(t, fakes, t.TempDir(), filepath.Join(t.TempDir(), "bs-m6"), "vb_main "+cmd, ci)
			if code != 2 || !strings.Contains(stderr, "CI") {
				t.Errorf("%s with %s: exit %d, stderr:\n%s", cmd, ci, code, stderr)
			}
			for _, tool := range []string{"virsh", "virt-install", "ssh"} {
				if c := benchCalls(t, fakes, tool); c != nil {
					t.Errorf("%s with %s called %s: %q", cmd, ci, tool, c)
				}
			}
		}
	}
}

// vb_owned is the one gate of down: bs-m6- and a name, lower case.
func TestVMBenchOwnsOnlyItsNames(t *testing.T) {
	fakes := newBenchFakes(t)
	script := `for n in bs-m6-server bs-m6-client-seed.iso bs-m6-base-noble.qcow2 bs-m6- bs-m6 bs-m6x bs-m6.qcow2 BS-M6-server xbs-m6-server 'bs-m6-a b' 'bs-m6-a/b' "bs-m6-a
b"; do if vb_owned "$n"; then printf 'own %s\n' "$n"; fi; done`
	stdout, stderr, code := vmBench(t, fakes, t.TempDir(), filepath.Join(t.TempDir(), "bs-m6"), script)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	want := "own bs-m6-server\nown bs-m6-client-seed.iso\nown bs-m6-base-noble.qcow2\n"
	if stdout != want {
		t.Errorf("owned:\n%s\nwant\n%s", stdout, want)
	}
}

// A VM made again by up keeps its MAC, and the leases of its earlier boots
// stay listed until they expire: its address is the lease that expires
// last, and up ignores the client ids leased before its domains existed
// (the second up of the bench's first real run read a stale lease).
func TestVMBenchIPIsTheNewestLeaseOfThisBoot(t *testing.T) {
	const (
		old = " 2026-10-29 03:12:41   52:54:00:b6:06:01   ipv4       192.168.122.189/24   -              ff:56:50:4d:98:00:02:00:00:ab:11:f5:c6"
		cur = " 2026-10-29 03:14:23   52:54:00:b6:06:01   ipv4       192.168.122.191/24   bs-m6-server   ff:56:50:4d:98:00:02:00:00:ab:11:7a:3d"
	)
	for _, rows := range [][]string{{old, cur}, {cur, old}} {
		fakes := newBenchFakes(t)
		writeLines(t, filepath.Join(fakes, "leases"), rows...)
		stdout, stderr, code := vmBench(t, fakes, t.TempDir(), filepath.Join(t.TempDir(), "bs-m6"),
			`vb_ip server; vb_ip server ff:56:50:4d:98:00:02:00:00:ab:11:7a:3d; vb_lease_ids server | sort; if vb_ip server $(vb_lease_ids server); then echo found; fi`)
		want := "192.168.122.191\n192.168.122.189\nff:56:50:4d:98:00:02:00:00:ab:11:7a:3d\nff:56:50:4d:98:00:02:00:00:ab:11:f5:c6\n"
		if code != 0 || stdout != want {
			t.Errorf("rows %q: exit %d, stdout:\n%s\nwant\n%s\nstderr:\n%s", rows, code, stdout, want, stderr)
		}
		for _, c := range virshVerbs(t, benchCalls(t, fakes, "virsh")) {
			if c != "net-dhcp-leases default --mac 52:54:00:b6:06:01" {
				t.Errorf("virsh %s", c)
			}
		}
	}
}

// A bs-m6 symlink to another directory is not the bench's: down empties
// nothing through it.
func TestVMBenchDownRefusesASymlinkedBenchDir(t *testing.T) {
	fakes := newBenchFakes(t)
	real := t.TempDir()
	writeLines(t, filepath.Join(real, "precious"), "x")
	link := filepath.Join(t.TempDir(), "bs-m6")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := vmBench(t, fakes, t.TempDir(), link, "vb_down")
	if code != 2 || !strings.Contains(stderr, "symlink") {
		t.Errorf("down through a symlink: exit %d, stderr:\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(real, "precious")); err != nil {
		t.Errorf("the linked directory lost its file: %v", err)
	}
	if calls := benchCalls(t, fakes, "virsh"); calls != nil {
		t.Errorf("virsh was called: %q", calls)
	}
	// Positive control: the same down on a real bs-m6 directory runs.
	dir := filepath.Join(t.TempDir(), "bs-m6")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := vmBench(t, fakes, t.TempDir(), dir, "vb_down"); code != 0 {
		t.Errorf("down on a directory: exit %d, stderr:\n%s", code, stderr)
	}
}

// up refuses a pool with less than 80 GiB free, before making anything.
func TestVMBenchUpRefusesAPoolTooSmall(t *testing.T) {
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	writeLines(t, meminfo, "MemTotal:       40000000 kB", "MemAvailable:   32000000 kB")
	for _, tc := range []struct {
		avail string
		ok    bool
	}{{"75161927680", false}, {"85899345920", true}} { // 70 GiB, 80 GiB
		fakes := newBenchFakes(t)
		writeLines(t, filepath.Join(fakes, "pool-avail"), tc.avail)
		// Past the preflight, up builds, and stops there: the fake
		// minisign makes no key.
		_, stderr, code := vmBench(t, fakes, t.TempDir(), filepath.Join(t.TempDir(), "bs-m6"), "vb_main up", "VB_MEMINFO="+meminfo)
		refused := code == 2 && strings.Contains(stderr, "the pool default has 70 GiB free")
		if refused == tc.ok {
			t.Errorf("pool with %s bytes free: exit %d, stderr:\n%s", tc.avail, code, stderr)
		}
		if tc.ok && !strings.Contains(stderr, "bs-m6: building backsight") {
			t.Errorf("80 GiB free: up did not go past the preflight:\n%s", stderr)
		}
		for _, c := range benchCalls(t, fakes, "virt-install") {
			if c != "--osinfo list" {
				t.Errorf("pool with %s bytes free: virt-install %s", tc.avail, c)
			}
		}
		for _, v := range virshVerbs(t, benchCalls(t, fakes, "virsh")) {
			switch strings.Fields(v)[0] {
			case "list", "vol-list", "net-info", "pool-info":
			default:
				t.Errorf("pool with %s bytes free: virsh %s", tc.avail, v)
			}
		}
	}
}

// reset reverts both VMs to bs-m6-clean and follows a renewed lease that
// carries another address into the ssh config; an unchanged lease leaves
// the config as it was.
func TestVMBenchResetFollowsANewAddress(t *testing.T) {
	const (
		srv = " 2026-10-29 03:14:23   52:54:00:b6:06:01   ipv4       %s/24   bs-m6-server   ff:00:01"
		cli = " 2026-10-29 03:14:27   52:54:00:b6:06:02   ipv4       192.168.122.11/24   bs-m6-client   ff:00:02"
	)
	for _, serverIP := range []string{"192.168.122.20", "192.168.122.10"} {
		fakes := newBenchFakes(t)
		writeLines(t, filepath.Join(fakes, "leases"), strings.Replace(srv, "%s", serverIP, 1), cli)
		bench := filepath.Join(t.TempDir(), "bs-m6")
		stdout, stderr, code := vmBench(t, fakes, t.TempDir(), bench,
			`vb_write_ssh_config 192.168.122.10 192.168.122.11; cp "$BS_M6_DIR/ssh_config" "$BS_M6_DIR.before"; vb_reset`)
		if code != 0 || !strings.Contains(stdout, "reset to bs-m6-clean") {
			t.Fatalf("reset: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		b, err := os.ReadFile(filepath.Join(bench, "ssh_config"))
		if err != nil {
			t.Fatal(err)
		}
		conf := string(b)
		if !strings.Contains(conf, "  HostName "+serverIP+"\n") || !strings.Contains(conf, "  HostName 192.168.122.11\n") {
			t.Errorf("server at %s: ssh_config:\n%s", serverIP, conf)
		}
		before, err := os.ReadFile(bench + ".before")
		if err != nil {
			t.Fatal(err)
		}
		if changed := conf != string(before); changed != (serverIP != "192.168.122.10") {
			t.Errorf("server at %s: config rewritten = %v", serverIP, changed)
		}
		var reverts []string
		for _, v := range virshVerbs(t, benchCalls(t, fakes, "virsh")) {
			if strings.HasPrefix(v, "snapshot-revert ") {
				reverts = append(reverts, v)
			}
		}
		if strings.Join(reverts, "\n") != "snapshot-revert bs-m6-server bs-m6-clean --running\nsnapshot-revert bs-m6-client bs-m6-clean --running" {
			t.Errorf("reverts: %q", reverts)
		}
	}
}

// The seed ISO holds the VM's private host key: it is ejected before the
// snapshot every run starts from is taken.
func TestVMBenchSnapshotEjectsTheSeedFirst(t *testing.T) {
	fakes := newBenchFakes(t)
	bench := filepath.Join(t.TempDir(), "bs-m6")
	_, stderr, code := vmBench(t, fakes, t.TempDir(), bench, `mkdir -p "$BS_M6_DIR/logs"; vb_snapshot server`)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	eject, snap := -1, -1
	for i, v := range virshVerbs(t, benchCalls(t, fakes, "virsh")) {
		switch {
		case v == "change-media bs-m6-server sda --eject --live --config --force":
			eject = i
		case v == "snapshot-create-as bs-m6-server bs-m6-clean":
			snap = i
		case strings.HasPrefix(v, "change-media") || strings.HasPrefix(v, "snapshot"):
			t.Errorf("unexpected virsh %s", v)
		}
	}
	if eject < 0 || snap < 0 || eject > snap {
		t.Errorf("eject at call %d, snapshot at call %d: want both, eject first\n%s", eject, snap, strings.Join(benchCalls(t, fakes, "virsh"), "\n"))
	}
}
