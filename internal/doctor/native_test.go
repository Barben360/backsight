package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/state"
)

// goodFacts is a healthy native install's guards.
func goodFacts() NativeFacts {
	return NativeFacts{
		Units:    map[string]string{"backsight-db.service": "active", "backsight-server.service": "active"},
		Fsync:    "on",
		Home:     "/n/home",
		HomeMode: 0o700,
		Venv:     "/n/venv/0.6.2-abc",
		Pin:      "/n/venv/0.7.0-def",
		Linger:   "yes",
	}
}

// nativeInstall makes f's config a native install whose guards read facts.
func nativeInstall(f *fixture, facts func() NativeFacts) *int {
	put(f.t, f.env.Dirs.ConfigFile(), "[server]\nruntime = \"native\"\n", 0o600)
	calls := new(int)
	f.env.Native = func(context.Context) (NativeFacts, error) { *calls++; return facts(), nil }
	return calls
}

func ids(r Report) []string {
	out := make([]string, len(r.Checks))
	for i, c := range r.Checks {
		out[i] = c.ID
	}
	return out
}

// A Docker install's checks are the ones it had before the native runtime
// (the cli golden pins the full list): the native probes add none, are
// never asked, and runtime-switch appears only once a switch is recorded.
func TestDoctorDockerChecksUnchanged(t *testing.T) {
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\n", 0o600)
	before := ids(f.run())
	asked := 0
	f.env.Native = func(context.Context) (NativeFacts, error) { asked++; return goodFacts(), nil }
	f.env.Switch = func() (*state.RuntimeSwitch, string) { return nil, "" }
	after := ids(f.run())
	if !slices.Equal(before, after) || asked != 0 {
		t.Errorf("probes changed a Docker install's checks (native asked %d times):\nbefore %v\nafter  %v", asked, before, after)
	}
	for _, id := range after {
		if strings.HasPrefix(id, "native-") || id == "runtime-switch" || id == "web-ui" {
			t.Errorf("a Docker install shows %s", id)
		}
	}
	expect(t, check(t, f.run(), "docker"), OK, "Docker 28.1.1")
	f.env.Switch = func() (*state.RuntimeSwitch, string) {
		return &state.RuntimeSwitch{From: "native", To: "docker", Step: "done", At: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}, ""
	}
	r := f.run()
	expect(t, check(t, r, "runtime-switch"), Info, "switched from native to docker")
	if got := ids(r); len(got) != len(before)+1 {
		t.Errorf("a recorded switch adds one check: %v", got)
	}
}

func TestDoctorNativeChecks(t *testing.T) {
	type want struct {
		id, summary, fix string
	}
	for _, tc := range []struct {
		name  string
		bad   func(*NativeFacts)
		want  want
		fails Status
	}{
		{"a unit failed", func(n *NativeFacts) { n.Units["backsight-server.service"] = "failed" },
			want{"native-unit", "backsight-server.service is failed", "backsight server start"}, Error},
		{"a unit not installed", func(n *NativeFacts) { n.Units["backsight-db.service"] = server.NotInstalled },
			want{"native-unit", "backsight-db.service is not installed", "backsight server install"}, Error},
		{"fsync off", func(n *NativeFacts) { n.Fsync = "off" },
			want{"native-fsync", "fsync is off", "systemctl --user cat backsight-db.service"}, Error},
		{"fsync unread", func(n *NativeFacts) {
			n.Fsync = ""
			n.Unread = map[string]error{"native-fsync": errors.New("connection refused")}
		},
			want{"native-fsync", "could not read fsync", "backsight server start"}, Error},
		{"another identity", func(n *NativeFacts) {
			n.Identity = &server.GuardError{Reason: "identity", Instance: "/n/home/.pg0/instances/hindsight", Want: "111", Got: "222"}
		}, want{"native-identity", "is not the one backsight recorded (identifier 222, recorded 111)", "backsight restore <archive>"}, Error},
		{"an empty instance", func(n *NativeFacts) {
			n.Identity = &server.GuardError{Reason: "empty", Instance: "/n/home/.pg0/instances/hindsight"}
		}, want{"native-identity", "/n/home/.pg0/instances/hindsight is empty", "backsight restore <archive>"}, Error},
		{"a stray postgres", func(n *NativeFacts) { n.Strays = []int{4242, 4343} },
			want{"native-stray", "pid 4242, 4343", "backsight server start"}, Error},
		{"linger off", func(n *NativeFacts) { n.Linger = "no" },
			want{"native-linger", "linger is off", "loginctl enable-linger"}, Error},
		{"linger unread", func(n *NativeFacts) {
			n.Linger = ""
			n.Unread = map[string]error{"native-linger": errors.New("no session")}
		},
			want{"native-linger", "could not read linger", "loginctl enable-linger"}, Warn},
		{"home open to others", func(n *NativeFacts) { n.HomeMode = 0o755 },
			want{"native-home", "/n/home has mode 0755, not 0700", "chmod 700 /n/home"}, Error},
		{"venv of another lock", func(n *NativeFacts) { n.VenvDiff = []string{"fastmcp==3.2.0"} },
			want{"native-venv", "fastmcp==3.2.0", "backsight server upgrade --allow-unordered"}, Error},
		{"the pinned venv changed", func(n *NativeFacts) { n.Pin = n.Venv; n.VenvDiff = []string{"fastmcp==3.2.0"} },
			want{"native-venv", "fastmcp==3.2.0", "rm /n/venv/0.6.2-abc/.backsight-complete; backsight server install"}, Error},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			nativeInstall(f, func() NativeFacts { n := goodFacts(); tc.bad(&n); return n })
			r := f.run()
			c := check(t, r, tc.want.id)
			expect(t, c, tc.fails, tc.want.summary)
			if !strings.Contains(c.Fix, tc.want.fix) {
				t.Errorf("%s fix %q, want %q", c.ID, c.Fix, tc.want.fix)
			}
			for _, other := range []string{"native-unit", "native-fsync", "native-identity", "native-stray", "native-linger", "native-home", "native-venv"} {
				if other != tc.want.id && check(t, r, other).Status != OK {
					t.Errorf("%s: %+v, want ok", other, check(t, r, other))
				}
			}
		})
	}
}

func TestDoctorNativeAllGood(t *testing.T) {
	f := newFixture(t)
	dockerAsked := false
	f.env.Docker = func(context.Context) (string, error) { dockerAsked = true; return "", exec.ErrNotFound }
	calls := nativeInstall(f, goodFacts)
	r := f.run()
	for _, id := range []string{"native-unit", "native-fsync", "native-identity", "native-stray", "native-linger", "native-home", "native-venv"} {
		if c := check(t, r, id); c.Status != OK {
			t.Errorf("%s: %+v, want ok", id, c)
		}
	}
	expect(t, check(t, r, "docker"), Info, "not used by the native runtime")
	expect(t, check(t, r, "web-ui"), Info, "none on the native runtime")
	if dockerAsked || *calls != 1 {
		t.Errorf("docker asked %v, native probe ran %d times", dockerAsked, *calls)
	}
	for _, id := range ids(r) {
		if id == "native-refused" || id == "runtime-switch" {
			t.Errorf("%s shown with nothing to show", id)
		}
	}
	if r.Errors != 0 && check(t, r, "server").Status != Error {
		t.Errorf("errors on a healthy native install: %+v", r.Checks)
	}
}

// Each database a restore moved aside is listed, so the user can delete it.
func TestDoctorListsRefusedInstances(t *testing.T) {
	f := newFixture(t)
	dirs := []string{"/n/home/.pg0/instances/hindsight.refused-1761700000", "/n/home/.pg0/instances/hindsight.refused-1761800000"}
	nativeInstall(f, func() NativeFacts { n := goodFacts(); n.Refused = dirs; return n })
	var got []Check
	for _, c := range f.run().Checks {
		if c.ID == "native-refused" {
			got = append(got, c)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want one native-refused per directory, got %+v", got)
	}
	for i, c := range got {
		if c.Status != Info || !strings.Contains(c.Summary, dirs[i]) || c.Fix != "rm -rf "+dirs[i] {
			t.Errorf("%+v", c)
		}
	}
}

// A probe that cannot read the native install at all is an error, never a
// silent pass.
func TestDoctorNativeProbeFails(t *testing.T) {
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nruntime = \"native\"\n", 0o600)
	f.env.Native = func(context.Context) (NativeFacts, error) { return NativeFacts{}, errors.New("no user manager") }
	r := f.run()
	c := check(t, r, "native-unit")
	expect(t, c, Error, "could not read the native runtime")
	if !strings.Contains(c.Detail, "no user manager") {
		t.Errorf("%+v", c)
	}
}

// deadPID is the pid of a process that has ended.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("no true: ", err)
	}
	return cmd.Process.Pid
}

func TestDoctorReportsACutOffSwitch(t *testing.T) {
	f := newFixture(t)
	rs := &state.RuntimeSwitch{From: "docker", To: "native", Step: "running", PID: deadPID(t), Since: time.Now().Add(-time.Minute)}
	f.env.Switch = func() (*state.RuntimeSwitch, string) { return rs, "" }
	c := check(t, f.run(), "runtime-switch")
	expect(t, c, Error, "a runtime switch to native was cut off: run backsight server runtime switch native, which first puts docker back")
	if c.Fix != "backsight server runtime switch native" {
		t.Errorf("fix %q", c.Fix)
	}
	// One still running is not cut off.
	rs.PID, rs.Since = os.Getpid(), time.Now()
	expect(t, check(t, f.run(), "runtime-switch"), Info, "a runtime switch to native is running")
}

func TestDoctorReportsKeptSourceData(t *testing.T) {
	f := newFixture(t)
	at := time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC)
	kept := "the Docker server of project backsight: the database volume backsight_hindsight-data (2.0 GB)"
	f.env.Switch = func() (*state.RuntimeSwitch, string) {
		return &state.RuntimeSwitch{From: "docker", To: "native", Step: "done", At: at}, kept
	}
	c := check(t, f.run(), "runtime-switch")
	expect(t, c, Info, "switched from docker to native on 2026-10-29")
	if c.Detail != kept+" kept from before the switch: backsight server runtime clean deletes it" || c.Fix != "backsight server runtime clean" {
		t.Errorf("%+v", c)
	}
	// Nothing kept: no clean to suggest.
	f.env.Switch = func() (*state.RuntimeSwitch, string) {
		return &state.RuntimeSwitch{From: "docker", To: "native", Step: "done", At: at}, ""
	}
	if c := check(t, f.run(), "runtime-switch"); c.Detail != "" || c.Fix != "" {
		t.Errorf("nothing kept: %+v", c)
	}
	// A rolled-back switch says the memory stayed where it was.
	f.env.Switch = func() (*state.RuntimeSwitch, string) {
		return &state.RuntimeSwitch{From: "docker", To: "native", Step: "rolled-back", At: at}, ""
	}
	expect(t, check(t, f.run(), "runtime-switch"), Info, "rolled back")
}

// venvWith is a venv whose site-packages hold dist-info dirs for pkgs
// (name → version, the name as the wheel spells it).
func venvWith(t *testing.T, pkgs map[string]string) string {
	t.Helper()
	venv := t.TempDir()
	sp := filepath.Join(venv, "lib", "python3.11", "site-packages")
	for name, v := range pkgs {
		if err := os.MkdirAll(filepath.Join(sp, name+"-"+v+".dist-info"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return venv
}

func TestNativeVenvDiff(t *testing.T) {
	lock := nativelock.Versions()
	if lock["fastmcp"] != "3.2.0" {
		t.Fatalf("the lock pins fastmcp %q: the test assumes 3.2.0", lock["fastmcp"])
	}
	pkgs := map[string]string{}
	for name, v := range lock {
		pkgs[strings.ReplaceAll(name, "-", "_")] = v // as a wheel's dist-info names it
	}
	pkgs["fastmcp"] = "4.1.4"
	pkgs["not_in_the_lock"] = "1.0" // an extra package is no difference
	diff, err := NativeVenvDiff(venvWith(t, pkgs))
	if err != nil || !slices.Equal(diff, []string{"fastmcp==3.2.0"}) {
		t.Errorf("one other version: %v %v, want [fastmcp==3.2.0]", diff, err)
	}
	delete(pkgs, "fastmcp")
	diff, err = NativeVenvDiff(venvWith(t, pkgs))
	if err != nil || !slices.Equal(diff, []string{"fastmcp==3.2.0"}) {
		t.Errorf("one missing: %v %v, want [fastmcp==3.2.0]", diff, err)
	}
	pkgs["fastmcp"] = "3.2.0"
	if diff, err := NativeVenvDiff(venvWith(t, pkgs)); err != nil || len(diff) != 0 {
		t.Errorf("the lock's set: %v %v, want none", diff, err)
	}
	if _, err := NativeVenvDiff(filepath.Join(t.TempDir(), "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no venv: %v, want not exist", err)
	}
}
