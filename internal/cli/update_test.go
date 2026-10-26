package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

// releaseDir lays out one goreleaser dist/ per version under a temp root
// (internal/release.Dir's parent-of-versions layout).
func releaseDir(t *testing.T, versions ...string) string {
	t.Helper()
	root := t.TempDir()
	for i, v := range versions {
		d := filepath.Join(root, "v"+v)
		_ = os.MkdirAll(d, 0o700)
		date := time.Date(2026, 10, 20+i, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
		for name, body := range map[string]string{
			"checksums.txt":                          "aa  backsight_" + v + "_linux_amd64.tar.gz\n",
			"backsight_" + v + "_linux_amd64.tar.gz": "tgz",
			"metadata.json":                          `{"version":"` + v + `","date":"` + date + `"}`,
			"CHANGELOG.md":                           "## Changelog\n* abc1234 ✨ Changes of " + v + "\n* def5678 🐛 A fix\n",
		} {
			if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// configure appends TOML to the dev install's config.toml.
func configure(t *testing.T, home, toml string) {
	t.Helper()
	p := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(raw, []byte("\n"+toml+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
}

type updateEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Command       string        `json:"command"`
	OK            bool          `json:"ok"`
	Error         *output.Error `json:"error"`
	Data          struct {
		Installed       string `json:"installed"`
		Source          string `json:"source"`
		Since           string `json:"since"`
		Prereleases     bool   `json:"prereleases"`
		UpdateAvailable bool   `json:"update_available"`
		Latest          string `json:"latest"`
		Versions        []struct {
			Version     string    `json:"version"`
			Tag         string    `json:"tag"`
			Prerelease  bool      `json:"prerelease"`
			PublishedAt time.Time `json:"published_at"`
			Summary     string    `json:"summary"`
			Notes       string    `json:"notes"`
		} `json:"versions"`
	} `json:"data"`
}

func decodeUpdate(t *testing.T, out string) updateEnvelope {
	t.Helper()
	var env updateEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not one JSON envelope: %v\n%s", err, out)
	}
	return env
}

func (e updateEnvelope) versions() string {
	var s []string
	for _, v := range e.Data.Versions {
		s = append(s, v.Version)
	}
	return strings.Join(s, " ")
}

func TestUpdateCheckListsNewerStableReleases(t *testing.T) {
	devInstall(t, true)
	setBuild(t, "0.1.0", "abc1234")
	src := releaseDir(t, "0.0.9", "0.1.0", "0.2.0", "0.3.0-m1", "0.2.1")
	code, out, _ := run(t, "update", "check", "--json", "--source", src)
	env := decodeUpdate(t, out)
	if code != output.OK || !env.OK || env.SchemaVersion != 1 || env.Command != "update check" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	d := env.Data
	if d.Installed != "0.1.0" || d.Source != "dir:"+src || d.Prereleases || !d.UpdateAvailable || d.Latest != "0.2.1" || env.versions() != "0.2.1 0.2.0" {
		t.Errorf("%+v", d)
	}
	v := d.Versions[0]
	if v.Tag != "v0.2.1" || v.Prerelease || v.Summary != "✨ Changes of 0.2.1 (+1 more)" || !strings.Contains(v.Notes, "🐛 A fix") || v.PublishedAt.IsZero() {
		t.Errorf("entry %+v", v)
	}
	code, text, _ := run(t, "update", "check", "--source", src)
	if code != output.OK || !strings.Contains(text, "2 newer release(s)") || !strings.Contains(text, "0.2.1") || !strings.Contains(text, "Changes of 0.2.0") {
		t.Errorf("text exit %d:\n%s", code, text)
	}
}

func TestUpdateCheckUpToDate(t *testing.T) {
	devInstall(t, true)
	src := releaseDir(t, "0.1.0", "0.2.0", "0.3.0-m1")
	for _, installed := range []string{"0.2.0", "v0.2.0", "0.4.0", "v0.2.0-4-gabc1234"} {
		setBuild(t, installed, "abc1234")
		code, out, _ := run(t, "update", "check", "--json", "--source", src)
		env := decodeUpdate(t, out)
		if code != output.OK || env.Data.UpdateAvailable || env.Data.Latest != "" || len(env.Data.Versions) != 0 {
			t.Errorf("%s: exit %d %+v", installed, code, env.Data)
		}
		if _, text, _ := run(t, "update", "check", "--source", src); !strings.Contains(text, "up to date") {
			t.Errorf("%s: %q", installed, text)
		}
	}
}

// Pre-releases show when the installed version is one (or a dev build of
// one), or when the config opts in.
func TestUpdateCheckPrereleases(t *testing.T) {
	home := devInstall(t, true)
	src := releaseDir(t, "0.1.0", "0.2.0-m1", "0.2.0-m2")
	setBuild(t, "0.2.0-m1", "abc1234")
	_, out, _ := run(t, "update", "check", "--json", "--source", src)
	if env := decodeUpdate(t, out); !env.Data.Prereleases || env.versions() != "0.2.0-m2" || !env.Data.Versions[0].Prerelease {
		t.Errorf("pre-release installed: %+v", env.Data)
	}
	setBuild(t, "v0.1.0-m0-93-gfd77ae6", "fd77ae6")
	if _, out, _ = run(t, "update", "check", "--json", "--source", src); decodeUpdate(t, out).versions() != "0.2.0-m2 0.2.0-m1 0.1.0" {
		t.Errorf("dev build of a pre-release: %s", out)
	}
	setBuild(t, "0.1.0", "abc1234")
	if _, out, _ = run(t, "update", "check", "--json", "--source", src); decodeUpdate(t, out).Data.UpdateAvailable {
		t.Errorf("stable installed sees pre-releases: %s", out)
	}
	configure(t, home, "[update]\nprereleases = true")
	if _, out, _ = run(t, "update", "check", "--json", "--source", src); decodeUpdate(t, out).versions() != "0.2.0-m2 0.2.0-m1" {
		t.Errorf("opted in: %s", out)
	}
}

func TestUpdateCheckUnreachableSourceIsBlocked(t *testing.T) {
	devInstall(t, true)
	setBuild(t, "0.1.0", "abc1234")
	code, out, _ := run(t, "update", "check", "--json", "--source", filepath.Join(t.TempDir(), "absent"))
	env := decodeUpdate(t, out)
	if code != output.Blocked || env.OK || env.Error == nil || env.Error.Code != output.Blocked {
		t.Errorf("missing directory: exit %d\n%s", code, out)
	}
	// The default source is GitHub; TestMain points it at a closed port.
	if code, _, errOut := run(t, "update", "check"); code != output.Blocked || !strings.Contains(errOut, "github:Barben360/backsight") {
		t.Errorf("github: exit %d %q", code, errOut)
	}
}

// fakeReleasesAPI serves GitHub's releases list and records the
// Authorization header of its last request.
func fakeReleasesAPI(t *testing.T) (auth func() (string, bool)) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Values("Authorization")
		mu.Unlock()
		if r.URL.Path != "/repos/Barben360/backsight/releases" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `[{"tag_name":"v0.2.0","published_at":"2026-10-30T10:00:00Z","body":"* abc1234 ✨ Add update check","assets":[]}]`)
	}))
	t.Cleanup(srv.Close)
	old := githubAPI
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = old })
	return func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(got, ","), len(got) > 0
	}
}

func TestUpdateCheckReadsGitHubWithTheEnvironmentToken(t *testing.T) {
	devInstall(t, true)
	setBuild(t, "0.1.0", "abc1234")
	auth := fakeReleasesAPI(t)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	code, out, _ := run(t, "update", "check", "--json")
	if env := decodeUpdate(t, out); code != output.OK || env.Data.Latest != "0.2.0" || env.Data.Source != "github:Barben360/backsight" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if h, sent := auth(); sent {
		t.Errorf("Authorization %q sent without a token", h)
	}
	t.Setenv("GITHUB_TOKEN", "ghs_test")
	if code, _, _ := run(t, "update", "check", "--json"); code != output.OK {
		t.Fatalf("exit %d", code)
	}
	if h, _ := auth(); h != "Bearer ghs_test" {
		t.Errorf("Authorization %q", h)
	}
}

// update check records its result for the banner when it reads the
// configured source; a one-off --source leaves the record alone.
func TestUpdateCheckRecordsOnlyTheConfiguredSource(t *testing.T) {
	home := devInstall(t, true)
	setBuild(t, "0.1.0", "abc1234")
	configure(t, home, "[update]\nsource = \""+releaseDir(t, "0.2.0")+"\"")
	if code, _, _ := run(t, "update", "check", "--source", releaseDir(t, "0.9.0")); code != output.OK {
		t.Fatalf("exit %d", code)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.UpdateCheck != nil {
		t.Errorf("a --source check recorded: %+v", st.UpdateCheck)
	}
	if code, _, _ := run(t, "update", "check"); code != output.OK {
		t.Fatalf("exit %d", code)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.UpdateCheck == nil || st.UpdateCheck.Latest != "0.2.0" {
		t.Errorf("record %+v", st.UpdateCheck)
	}
}

func TestChangelogSince(t *testing.T) {
	devInstall(t, true)
	setBuild(t, "0.3.0", "abc1234")
	src := releaseDir(t, "0.1.0", "0.2.0", "0.3.0", "0.4.0-m1")
	code, out, _ := run(t, "changelog", "--json", "--source", src)
	env := decodeUpdate(t, out)
	if code != output.OK || env.Command != "changelog" || env.versions() != "0.3.0 0.2.0 0.1.0" {
		t.Errorf("everything: exit %d %s", code, out) // the installed one included; pre-releases hidden for a stable install
	}
	_, out, _ = run(t, "changelog", "--json", "--source", src, "--since", "v0.1.0")
	if env = decodeUpdate(t, out); env.versions() != "0.3.0 0.2.0" || env.Data.Since != "v0.1.0" {
		t.Errorf("since 0.1.0: %s", out)
	}
	code, text, _ := run(t, "changelog", "--source", src, "--since", "0.2.0")
	if code != output.OK || !strings.Contains(text, "Changes of 0.3.0") || strings.Contains(text, "Changes of 0.2.0") {
		t.Errorf("text exit %d:\n%s", code, text)
	}
	if code, _, _ := run(t, "changelog", "--source", src, "--since", "yesterday"); code != output.Usage {
		t.Errorf("a bad --since: exit %d", code)
	}
	if code, _, _ := run(t, "changelog", "--source", filepath.Join(t.TempDir(), "absent")); code != output.Blocked {
		t.Errorf("unreachable: exit %d", code)
	}
}

// jobs maintain records a successful check, and a failed one without
// failing the job, keeping the release the last good check found.
func TestMaintainRecordsTheUpdateCheck(t *testing.T) {
	home := devInstall(t, true)
	configure(t, home, "[backup]\nenabled = false")
	setBuild(t, "0.1.0", "abc1234")
	src := releaseDir(t, "0.1.0", "0.2.0")
	configure(t, home, "[update]\nsource = \""+src+"\"")
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	code, out, _ := run(t, "jobs", "maintain")
	if code != output.OK || !strings.Contains(out, "update 0.2.0 available") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	uc := st.UpdateCheck
	if uc == nil || uc.Latest != "0.2.0" || uc.Summary != "✨ Changes of 0.2.0 (+1 more)" || uc.Newer != 1 || uc.Error != "" || uc.Installed != "0.1.0" ||
		!uc.CheckedAt.Equal(time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("record %+v", uc)
	}
	_ = os.RemoveAll(src)
	withClock(t, time.Date(2026, 10, 26, 4, 0, 0, 0, time.UTC))
	code, out, _ = run(t, "jobs", "maintain", "--json")
	if code != output.OK || !strings.Contains(out, `"update": "failed: `) {
		t.Fatalf("a failed check fails the job: exit %d\n%s", code, out)
	}
	st, _ = state.Load(pathsOf(home).StateFile())
	if uc = st.UpdateCheck; uc == nil || uc.Error == "" || uc.Latest != "0.2.0" || !uc.CheckedAt.Equal(time.Date(2026, 10, 26, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("failure record %+v", uc)
	}
}

func TestSessionStartBannerShowsANewerRelease(t *testing.T) {
	home := hookHome(t)
	record := func(latest string) {
		_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
			s.UpdateCheck = &state.UpdateCheck{CheckedAt: time.Now(), Installed: "0.1.0", Latest: latest, Summary: "✨ Add update check (+3 more)"}
			return nil
		})
	}
	start := func() string {
		return fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")}).SystemMessage
	}
	setBuild(t, "0.1.0", "abc1234")
	if msg := start(); strings.Contains(msg, "update") {
		t.Errorf("no check recorded: %q", msg)
	}
	record("0.2.0")
	// The version only: release notes are unsigned and never reach a
	// Claude session.
	if msg := start(); !strings.Contains(msg, "⬆️ backsight update 0.2.0 available · see `backsight changelog`") || strings.Contains(msg, "Add update check") {
		t.Errorf("banner %q", msg)
	}
	setBuild(t, "0.2.0", "def5678") // updated since the check
	if msg := start(); strings.Contains(msg, "update") {
		t.Errorf("the running version is the latest: %q", msg)
	}
	setBuild(t, "0.1.0", "abc1234")
	record("")
	if msg := start(); strings.Contains(msg, "update") {
		t.Errorf("up to date: %q", msg)
	}
}

// A failed check keeps the release the last good check of the same source
// found, never one another source found (it may not exist there).
func TestAFailedCheckKeepsOnlyItsSourcesRelease(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	recordUpdateCheck(d, updateCheckResult{Source: "dir:/a", Installed: "0.1.0", UpdateAvailable: true, Latest: "0.2.0", Versions: []versionEntry{{Version: "0.2.0", Summary: "s"}}}, nil)
	recordUpdateCheck(d, updateCheckResult{Source: "dir:/a", Installed: "0.1.0"}, errors.New("unreadable"))
	if st, _ := state.Load(d.StateFile()); st.UpdateCheck == nil || st.UpdateCheck.Latest != "0.2.0" || st.UpdateCheck.Error != "unreadable" {
		t.Errorf("same source: %+v", st.UpdateCheck)
	}
	recordUpdateCheck(d, updateCheckResult{Source: "github:Barben360/backsight", Installed: "0.1.0"}, errors.New("answered 404"))
	if st, _ := state.Load(d.StateFile()); st.UpdateCheck == nil || st.UpdateCheck.Latest != "" || st.UpdateCheck.Source != "github:Barben360/backsight" {
		t.Errorf("another source kept dir:/a's release: %+v", st.UpdateCheck)
	}
}
