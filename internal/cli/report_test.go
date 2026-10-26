package cli

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/report"
)

// reportPlant is what a report must never show, planted where a real
// machine holds it.
type reportPlant struct {
	home, secret, memory, prompt string
	requests                     *[]string
}

// plantedMachine is an isolated machine whose server answers every call
// with an error that echoes a memory and a secret, whose telemetry holds an
// error quoting a prompt, a secret and a home path, and whose transcripts
// and state hold them too.
func plantedMachine(t *testing.T) reportPlant {
	t.Helper()
	home := isolate(t)
	withClaude(t)
	oldHost := hostname
	hostname = func() (string, error) { return "annsbox.lan", nil }
	t.Cleanup(func() { hostname = oldHost })

	p := reportPlant{
		home: home,
		// Assembled at runtime: the secret scanner flags literals.
		secret: "ghp_" + strings.Repeat("Zq7", 12),
		memory: "invoices moved to the quokkatrellis partition scheme",
		prompt: "please refactor the zebraumbrella checkout flow now",
	}
	var mu sync.Mutex
	var reqs []string
	p.requests = &reqs
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		// A raw newline: the whole answer goes, not only its first line.
		_, _ = w.Write([]byte("{\"detail\":\"extraction failed on:\n" + p.memory + " api_key=" + p.secret + "\"}"))
	}))
	t.Cleanup(srv.Close)
	_ = os.MkdirAll(filepath.Join(home, "backsight", "config"), 0o700)
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \""+srv.URL+"\"\nui_port = "+itoa(closedPort(t))+"\n"), 0o600)

	d := pathsOf(home)
	now := time.Now().UTC()
	ts := now.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
	lines := []map[string]any{
		{"ts": ts, "event": "session_state_error", "project": "ann-secret-project",
			"error": "open " + home + "/work/annsbox.lan/state.json with token=" + p.secret + " on worker backsight-annsbox: POST /v1/x: HTTP 422 (bad_request): {\"input\":\"" + p.prompt + "\",\"memory\":\"" + p.memory + "\"}"},
		{"ts": ts, "event": "prompt", "decision": "recall", "plen": len(p.prompt), "prompt": p.prompt, "text": p.memory,
			"scopes": map[string]any{"project": map[string]any{"ok": false}}},
		{"ts": ts, "event": "hook_failure", "hook": "prompt", "kind": "timeout", "ms": 9000},
		{"ts": ts, "event": "job", "name": "tick", "status": "partial", "details": map[string]any{"reason": "cannot read " + home + "/x"}},
	}
	_ = os.MkdirAll(d.TelemetryDir(), 0o700)
	var tel strings.Builder
	for _, l := range lines {
		raw, _ := json.Marshal(l)
		tel.Write(append(raw, '\n'))
	}
	_ = os.WriteFile(filepath.Join(d.TelemetryDir(), "events-"+now.Format("2006-01")+".jsonl"), []byte(tel.String()), 0o600)

	// A transcript and a session record: report never reads them.
	tr := filepath.Join(home, ".claude", "projects", "-work", "s1.jsonl")
	_ = os.MkdirAll(filepath.Dir(tr), 0o700)
	_ = os.WriteFile(tr, []byte(`{"type":"user","message":{"content":"`+p.prompt+`"}}`+"\n"), 0o600)
	_ = os.MkdirAll(d.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.SessionsDir(), "s1.json"), []byte(`{"last_prompt":"`+p.prompt+`"}`), 0o600)
	// A failed update check that quotes the home and the secret.
	_ = os.WriteFile(d.StateFile(), []byte(`{"schema_version":1,"plugin_version":"0.9.1","update_check":{"checked_at":"`+now.Format(time.RFC3339)+`","error":"reading `+home+`/.config/gh/hosts.yml: oauth_token: `+p.secret+`"}}`), 0o600)
	return p
}

// issueBodies decodes every GitHub link in out: the planted values must not
// hide behind URL escaping either.
func issueBodies(t *testing.T, out string) string {
	t.Helper()
	var b strings.Builder
	for _, link := range regexp.MustCompile(`https://github\.com/[^\s"\\]+`).FindAllString(out, -1) {
		u, err := url.Parse(link)
		if err != nil {
			t.Fatalf("bad link %q: %v", link, err)
		}
		for _, vs := range u.Query() {
			for _, v := range vs {
				b.WriteString(v + "\n")
			}
		}
	}
	return b.String()
}

// encodedForms are the ways a value can show in a report: as is, and
// escaped for a query or a path (hindsight's bankPath escapes ':' too).
func encodedForms(v string) []string {
	return []string{v, url.QueryEscape(v), url.PathEscape(v), strings.ReplaceAll(url.PathEscape(v), ":", "%3A")}
}

// assertScrubbed runs report with each output form and checks that no
// planted value shows, in any encoded form, in the text, the JSON or the
// decoded links, and that every placeholder that stands for them does.
func assertScrubbed(t *testing.T, planted map[string]string, wants []string) {
	t.Helper()
	for _, args := range [][]string{{"report"}, {"report", "--json"}} {
		code, out, stderr := run(t, args...)
		if code != output.OK {
			t.Fatalf("%v: exit %d\n%s\n%s", args, code, out, stderr)
		}
		// JSON escapes <, > and &: compare the text they stand for.
		plain := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(out)
		bodies := issueBodies(t, plain)
		if !strings.Contains(bodies, "**Versions**") {
			t.Fatalf("%v: no issue body found in:\n%s", args, out)
		}
		all := plain + stderr + bodies
		for what, v := range planted {
			for _, form := range encodedForms(v) {
				if strings.Contains(all, form) {
					t.Errorf("%v shows the planted %s %q:\n%s", args, what, form, out)
				}
			}
		}
		// The planted values did reach the scrubber: what replaced them shows.
		for _, want := range wants {
			if !strings.Contains(all, want) {
				t.Errorf("%v lacks %q:\n%s", args, want, out)
			}
		}
	}
}

func TestReportNeverShowsSecretsMemoriesPromptsOrHome(t *testing.T) {
	p := plantedMachine(t)
	assertScrubbed(t, map[string]string{"secret": p.secret, "memory": p.memory, "prompt": p.prompt, "home": p.home,
		"host": "annsbox", "project": "ann-secret-project"},
		[]string{"[redacted:", "~/backsight/config/config.toml", "~/work/<host>/state.json", "worker backsight-<host>", "[server answer omitted]",
			"doctor/server/error", "hook_failure/prompt/timeout", "recall/scope_failed", "session_state_error", "job/tick/partial"})
}

func TestReportJSONLinksAndSendsNothing(t *testing.T) {
	p := plantedMachine(t)
	before := tree(t, p.home)
	code, out, _ := run(t, "report", "--json")
	var env struct {
		OK   bool          `json:"ok"`
		Data report.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK || !env.OK {
		t.Fatalf("exit %d, %v:\n%s", code, err, out)
	}
	r := env.Data
	if r.Sent {
		t.Error("sent must be false")
	}
	if r.Class != "doctor/server/error" {
		t.Errorf("class %q, want the first doctor error", r.Class)
	}
	if !strings.HasPrefix(r.IssueURL, "https://github.com/Barben360/backsight/issues/new?") || len(r.IssueURL) > report.MaxURL {
		t.Errorf("issue link (%d bytes) %s", len(r.IssueURL), r.IssueURL)
	}
	if want := "https://github.com/Barben360/backsight/issues?q=" + url.QueryEscape(`is:issue "doctor/server/error"`); r.SearchURL != want {
		t.Errorf("search link %s, want %s", r.SearchURL, want)
	}
	u, _ := url.Parse(r.IssueURL)
	if title := u.Query().Get("title"); !strings.HasPrefix(title, "report: doctor/server/error: ") {
		t.Errorf("title %q", title)
	}
	if body := u.Query().Get("body"); !strings.Contains(body, "backsight report") || !strings.Contains(body, "Claude Code 2.1.310") {
		t.Errorf("body lacks the paste placeholder or the versions:\n%s", body)
	}
	v := r.Diagnostic.Versions
	if v.Plugin != "0.9.1" || v.ClaudeCode != "2.1.310" || v.HindsightPin == "" || v.OS == "" {
		t.Errorf("versions %+v", v)
	}
	if r.Diagnostic.Doctor.Server == nil || !r.Diagnostic.Doctor.Server.Loopback || len(r.Diagnostic.Doctor.Checks) < 8 {
		t.Errorf("doctor %+v", r.Diagnostic.Doctor)
	}
	if c := reportCheck(r, "server"); !strings.Contains(c.Detail, "HTTP 500") || !strings.Contains(c.Detail, "[server answer omitted]") || strings.Contains(c.Detail, "quokka") {
		t.Errorf("server check %+v", c)
	}
	if c := r.Diagnostic.Counts; c.SessionsTracked != 1 || c.ErrorClasses["hook_failure/prompt/timeout"] != 1 || c.Days != 7 {
		t.Errorf("counts %+v", c)
	}
	if !strings.Contains(r.Paste, "```json") || !strings.Contains(r.Paste, `"recent_errors"`) {
		t.Errorf("paste block:\n%s", r.Paste)
	}
	for _, req := range *p.requests {
		if !strings.HasPrefix(req, "GET ") {
			t.Errorf("report made a request that is not a read: %s", req)
		}
	}
	if after := tree(t, p.home); after != before {
		t.Errorf("report changed files:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// tree lists every file under dir with its size and modification time.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		b.WriteString(p + " " + fi.Mode().String() + " " + fi.ModTime().String() + "\n")
		if !d.IsDir() {
			b.WriteString(itoa(int(fi.Size())) + "\n")
		}
		return nil
	})
	return b.String()
}

func TestReportHidesTheBankName(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"healthy"}`))
		case "/version":
			_, _ = w.Write([]byte(`{"api_version":"1.1.0"}`))
		default:
			_, _ = w.Write([]byte(`{"banks":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	_ = os.MkdirAll(filepath.Join(home, "backsight", "config"), 0o700)
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \""+srv.URL+"\"\nbank = \"ann-private-notes\"\nui_port = "+itoa(closedPort(t))+"\n"), 0o600)
	code, out, _ := run(t, "report")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	all := out + issueBodies(t, out)
	if strings.Contains(all, "ann-private-notes") {
		t.Errorf("the bank name shows:\n%s", out)
	}
	if !strings.Contains(all, "bank <bank> does not exist on the server") {
		t.Errorf("the bank check is not in the report:\n%s", out)
	}
}

func TestReportHealthyMachineHasNoClass(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	// Transcripts kept long enough: isolate's machine warns about nothing else.
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"cleanupPeriodDays": 3650}`), 0o600)
	code, out, _ := run(t, "report", "--json")
	var env struct {
		Data report.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d, %v:\n%s", code, err, out)
	}
	r := env.Data
	if r.Class != "" || r.SearchURL != "" || r.Diagnostic.Doctor.Errors != 0 || r.Diagnostic.Doctor.Warnings != 0 {
		t.Errorf("class %q, search %q, doctor %+v", r.Class, r.SearchURL, r.Diagnostic.Doctor)
	}
	if r.Diagnostic.RecentErrors == nil || len(r.Diagnostic.RecentErrors) != 0 {
		t.Errorf("recent errors %v, want an empty list", r.Diagnostic.RecentErrors)
	}
	if u, _ := url.Parse(r.IssueURL); !strings.HasPrefix(u.Query().Get("title"), "report: backsight ") {
		t.Errorf("issue link %s", r.IssueURL)
	}
	code, out, _ = run(t, "report")
	if code != output.OK || !strings.Contains(out, "doctor found no error and the telemetry holds no recent one.") || strings.Contains(out, "Error class:") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestReportWithoutClaudeCode(t *testing.T) {
	isolate(t)
	withClaude(t).missing = true
	code, out, _ := run(t, "report", "--json")
	if code != output.OK || !strings.Contains(out, `"claude_code": "not found"`) {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestReportRefusesArguments(t *testing.T) {
	isolate(t)
	if code, _, _ := run(t, "report", "send"); code != output.Usage {
		t.Errorf("exit %d, want %d", code, output.Usage)
	}
}

// reportCheck is the doctor check id of r ("" fields when absent).
func reportCheck(r report.Report, id string) doctor.Check {
	for _, c := range r.Diagnostic.Doctor.Checks {
		if c.ID == id {
			return c
		}
	}
	return doctor.Check{}
}

// reportJSON runs report --json and decodes its data.
func reportJSON(t *testing.T) report.Report {
	t.Helper()
	code, out, _ := run(t, "report", "--json")
	var env struct {
		Data report.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d, %v:\n%s", code, err, out)
	}
	return env.Data
}

func setUser(t *testing.T, name string) {
	t.Helper()
	old := osUser
	osUser = func() string { return name }
	t.Cleanup(func() { osUser = old })
}

func TestReportHidesARemoteServerHost(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	_ = os.MkdirAll(filepath.Join(home, "backsight", "config"), 0o700)
	// .example never resolves: the lookup error names the host.
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \"http://annsmemory.example:1\"\nui_port = "+itoa(closedPort(t))+"\n"), 0o600)
	assertScrubbed(t, map[string]string{"server host": "annsmemory.example"}, []string{"<server>"})
	r := reportJSON(t)
	if s := r.Diagnostic.Doctor.Server; s == nil || s.Loopback || s.Scheme != "http" || s.Source != "config" {
		t.Errorf("server %+v", s)
	}
}

func TestReportHidesTheEscapedBankAndAMultiLineAnswer(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	memory := "the zebraumbrella ledger moved to quokkatrellis"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"healthy"}`))
		case "/version":
			_, _ = w.Write([]byte(`{"api_version":"1.1.0"}`))
		case "/v1/default/banks":
			_, _ = w.Write([]byte(`{"banks":[{"bank_id":"coding-agent::memoire"}]}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("{\"detail\":\"stats failed\n" + memory + "\"}"))
		}
	}))
	t.Cleanup(srv.Close)
	_ = os.MkdirAll(filepath.Join(home, "backsight", "config"), 0o700)
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\nui_port = "+itoa(closedPort(t))+"\n"), 0o600)
	assertScrubbed(t, map[string]string{"bank": "coding-agent::memoire", "memory": memory, "memory word": "quokkatrellis"},
		[]string{"/v1/default/banks/<bank>/stats: HTTP 500", "cannot read bank <bank>"})
	if c := reportCheck(reportJSON(t), "bank"); !strings.HasSuffix(c.Detail, "HTTP 500 (server_error): [server answer omitted]") {
		t.Errorf("bank check %+v", c)
	}
}

func TestReportHidesRootsUserAndSecretsOutsideHome(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	setUser(t, "annuser")
	ext := t.TempDir()
	bs := filepath.Join(ext, "run", "media", "annuser", "bs")
	t.Setenv("BACKSIGHT_HOME", bs)
	d, err := paths.Resolve("linux", home, os.Getenv)
	if err != nil || d.Data != filepath.Join(bs, "data") {
		t.Fatalf("dirs %+v, %v", d, err)
	}
	vault := filepath.Join(ext, "mnt", "annsvault", "backups")
	// Secrets no redaction rule recognises: only secrets.toml and .env say
	// they are secrets.
	tomlKey, phrase, envKey := "quokkaplainwordtrellis", "pass phrase/with slash zebra", "staleumbrellaledgerkey"

	_ = os.MkdirAll(d.Config, 0o700)
	_ = os.WriteFile(d.ConfigFile(), []byte("[server]\nurl = \"http://127.0.0.1:"+itoa(closedPort(t))+"\"\nui_port = "+itoa(closedPort(t))+
		"\n[backup]\ndir = \""+vault+"\"\n"), 0o600)
	_ = os.WriteFile(d.SecretsFile(), []byte("api_key = \""+tomlKey+"\"\nllm_api_key = \""+phrase+"\"\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(d.Data, "server"), 0o700)
	_ = os.WriteFile(filepath.Join(d.Data, "server", ".env"), []byte("# old render\nHINDSIGHT_API_TENANT_API_KEY='"+envKey+"'\nHINDSIGHT_API_LLM_PROVIDER=openai\n"), 0o600)

	now := time.Now().UTC()
	var tel strings.Builder
	for ev, msg := range map[string]string{
		"open_failed":   "open /run/media/annuser/usb/notes.json on annuser-laptop: permission denied",
		"dial_failed":   "dial " + tomlKey + ": refused",
		"lookup_failed": "lookup " + url.QueryEscape(phrase) + ": no such host",
		"stat_failed":   "stat " + envKey + ": gone",
		"read_failed":   "read " + d.State + "/sessions/a.json and " + d.Bin + "/backsight",
		"copy_failed":   "copy to " + vault + "/x.tar: no space",
	} {
		raw, _ := json.Marshal(map[string]any{"ts": now.Add(-time.Minute).Format(time.RFC3339Nano), "event": ev, "error": msg})
		tel.Write(append(raw, '\n'))
	}
	_ = os.MkdirAll(d.TelemetryDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.TelemetryDir(), "events-"+now.Format("2006-01")+".jsonl"), []byte(tel.String()), 0o600)

	assertScrubbed(t, map[string]string{"user": "annuser", "temp dir": ext, "BACKSIGHT_HOME": bs, "backup dir": vault,
		"secrets.toml key": tomlKey, "secrets.toml phrase": phrase, ".env key": envKey},
		[]string{"<config>/config.toml", "no backup in <backups>", "/run/media/<user>/usb/notes.json on <user>-laptop", "<state>/sessions/a.json and <bin>/backsight",
			"copy to <backups>/x.tar", "dial [redacted:secret]: refused", "lookup [redacted:secret]: no such host", "stat [redacted:secret]: gone"})
}

func TestSecretValuesReadsWhatParsersRefuse(t *testing.T) {
	dir := t.TempDir()
	sec, env := filepath.Join(dir, "secrets.toml"), filepath.Join(dir, ".env")
	// Invalid TOML and readable by others: doctor refuses it, the report
	// still scrubs its values.
	_ = os.WriteFile(sec, []byte("api_key = \"quokkaplainwordtrellis\"\n[[[ broken\nui_key = 'zebraumbrellaledger'\n"), 0o644)
	_ = os.WriteFile(env, []byte("# HINDSIGHT_API_LLM_API_KEY='commentedoutvalue'\nMY_TOKEN=unquotedtokenvalue\nHINDSIGHT_API_LLM_PROVIDER=openai\nHINDSIGHT_API_LLM_BASE_URL='http://llm.example:8080/v1'\n"), 0o600)
	got := strings.Join(secretValues(sec, env, true), " | ")
	for _, want := range []string{"quokkaplainwordtrellis", "zebraumbrellaledger", "unquotedtokenvalue", "http://llm.example:8080/v1"} {
		if !strings.Contains(got, want) {
			t.Errorf("values %q lack %q", got, want)
		}
	}
	for _, not := range []string{"openai", "commentedoutvalue", "broken"} {
		if strings.Contains(got, not) {
			t.Errorf("values %q hold %q", got, not)
		}
	}
	if v := secretValues(filepath.Join(dir, "none"), filepath.Join(dir, "none"), true); len(v) != 0 {
		t.Errorf("missing files gave %q", v)
	}
	// Redaction outside the report keeps a quoted value that names no
	// secret: the LLM base URL is ordinary text.
	got = strings.Join(secretValues(sec, env, false), " | ")
	if !strings.Contains(got, "unquotedtokenvalue") || !strings.Contains(got, "zebraumbrellaledger") || strings.Contains(got, "llm.example") {
		t.Errorf("values without quoted ones: %q", got)
	}
}

// doctor's remotes check names projects and repositories: the report
// shows neither (a private repository's name is the user's).
func TestReportHidesProjectNamesAndRemotes(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	sessions := pathsOf(home).SessionsDir()
	_ = os.MkdirAll(sessions, 0o700)
	_ = os.WriteFile(filepath.Join(sessions, "s1.json"), []byte(`{"project":"skunkworks","remote":"github.com/annsco/skunkworks"}`), 0o600)
	_ = os.WriteFile(filepath.Join(sessions, "s2.json"), []byte(`{"project":"skunkworks","remote":"gitlab.com/rival/stealth-skunkworks"}`), 0o600)
	// git's own messages quote a remote as scp, without its host, by its
	// owner, in the repository's case.
	d := pathsOf(home)
	ts := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	raw, _ := json.Marshal(map[string]any{"ts": ts, "event": "git_failed",
		"error": "fetch git@github.com:Annsco/Skunkworks.git: repository Annsco/Skunkworks not found; owner annsco; mirror git@gitlab.com:/rival/stealth-skunkworks.git"})
	_ = os.MkdirAll(d.TelemetryDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.TelemetryDir(), "events-"+time.Now().UTC().Format("2006-01")+".jsonl"), append(raw, '\n'), 0o600)
	assertScrubbed(t, map[string]string{"project": "skunkworks", "remote": "annsco", "remote owner": "Annsco", "other remote": "rival/stealth", "other owner": "rival"},
		[]string{"fetch git@<remote>.git: repository <remote> not found; owner <remote>; mirror git@<remote>.git"})
}

// A reference server on another machine (~/.hindsight/coding-agent.json)
// is named by doctor's reference-server check: its host is hidden as
// <server>. A loopback one is not a name and stays.
func TestReportHidesANonLoopbackReferenceServer(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	old := probes
	probes = func(e *doctor.Env) {
		old(e)
		e.ReferenceServer = func(context.Context, string) (doctor.RefServer, error) { return doctor.RefServer{Running: true}, nil }
	}
	_ = os.MkdirAll(filepath.Dir(cfgFile(home)), 0o700)
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \"http://127.0.0.1:"+itoa(closedPort(t))+"\"\nui_port = "+itoa(closedPort(t))+"\n"), 0o600)
	ref := filepath.Join(home, ".hindsight", "coding-agent.json")
	_ = os.MkdirAll(filepath.Dir(ref), 0o700)
	_ = os.WriteFile(ref, []byte(`{"apiUrl":"http://annsref.example:8888","bankId":"coding-agent::memoire"}`), 0o600)
	assertScrubbed(t, map[string]string{"reference host": "annsref"}, []string{"the older Hindsight server at http://<server>:8888 is still running"})

	_ = os.WriteFile(ref, []byte(`{"apiUrl":"http://localhost:8888","bankId":"coding-agent::memoire"}`), 0o600)
	if c := reportCheck(reportJSON(t), "reference-server"); !strings.Contains(c.Summary, "the older Hindsight server at http://localhost:8888 is still running") {
		t.Errorf("reference-server check %+v", c)
	}
}

// localProjects plants one project name in each local source report reads
// (none of them a transverse alias): session states and their injections,
// a live record whose directory's .backsight.toml names a project and a
// related one, cursors, telemetry. An error names them all, and the
// server-only extra names, so each shows unless report hides it.
func localProjects(t *testing.T, home string, serverOnly ...string) []string {
	t.Helper()
	d := pathsOf(home)
	put := func(p, body string) {
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(d.SessionsDir(), "s1.json"), `{"project":"brambleway","injected":[{"id":"m1","project":"fernhollow"}]}`)
	repo := filepath.Join(home, "work", "mg")
	put(filepath.Join(repo, ".backsight.toml"), "project = \"mossgarden\"\nrelated = [\"tidewater\"]\n")
	put(filepath.Join(d.SessionsDir(), "by-pid", "4242.json"), `{"session_id":"s1","cwd":"`+repo+`","project":"brambleway"}`)
	put(filepath.Join(d.CursorsDir(), "s9.json"), `{"transcript":"","document":"conversation:s9@cinderfield","home":"emberlane","segments":{"cinderfield":1,"emberlane":2}}`)
	names := []string{"brambleway", "fernhollow", "mossgarden", "tidewater", "cinderfield", "emberlane", "quillpond"}
	ts := time.Now().UTC().Add(-time.Hour)
	var tel strings.Builder
	for _, l := range []map[string]any{
		{"ts": ts.Format(time.RFC3339Nano), "event": "prompt", "project": "quillpond", "decision": "skip"},
		{"ts": ts.Format(time.RFC3339Nano), "event": "flush_failed", "project": "brambleway",
			"error": "flush of " + strings.Join(append(append([]string{}, names...), serverOnly...), ", ") + " failed"},
	} {
		raw, _ := json.Marshal(l)
		tel.Write(append(raw, '\n'))
	}
	put(filepath.Join(d.TelemetryDir(), "events-"+ts.Format("2006-01")+".jsonl"), tel.String())
	return names
}

// projectBank answers doctor as a healthy server whose bank holds tags
// and a transverse pool of 20 observations, each naming lanternfish.
func projectBank(t *testing.T, bank string, tags ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "/v1/default/banks/" + bank
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"healthy"}`))
		case "/version":
			_, _ = w.Write([]byte(`{"api_version":"1.1.0"}`))
		case "/v1/default/banks":
			_, _ = w.Write([]byte(`{"banks":[{"bank_id":"` + bank + `"}]}`))
		case base + "/stats":
			_, _ = w.Write([]byte(`{"total_nodes":30,"total_documents":3}`))
		case base + "/tags":
			items := []map[string]any{}
			for _, tg := range tags {
				if strings.HasPrefix(tg, strings.TrimSuffix(r.URL.Query().Get("q"), "*")) {
					items = append(items, map[string]any{"tag": tg, "count": 1})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": len(items)})
		case base + "/memories/list":
			items := []map[string]any{}
			if r.URL.Query().Get("tags") != "" && r.URL.Query().Get("offset") == "0" {
				for i := range 20 {
					items = append(items, map[string]any{"id": "o" + itoa(i), "fact_type": "observation", "text": "lanternfish deploys on Fridays.", "tags": []string{"project:workspace"}})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": len(items)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Every project name the machine knows is hidden, whatever its source:
// the bank's tags (one name only there), the local states (one only
// there), a configured transverse alias, and the names doctor's checks
// show, a short one included; check ids and error classes are unchanged.
func TestReportHidesEveryKnownProjectName(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	url := projectBank(t, "ann-memory", "project:workspace", "project:.claude", "project:lanternfish", "project:saltmarsh", "project:ops")
	_ = os.MkdirAll(filepath.Dir(cfgFile(home)), 0o700)
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \""+url+"\"\nbank = \"ann-memory\"\nui_port = "+itoa(closedPort(t))+
		"\n[projects]\ntransverse = [\"workspace\", \".claude\"]\n"), 0o600)
	names := localProjects(t, home, "saltmarsh", "--project ops")
	// doctor's remotes check names a short project: two repositories share api.
	sessions := pathsOf(home).SessionsDir()
	_ = os.WriteFile(filepath.Join(sessions, "s2.json"), []byte(`{"project":"api","remote":"github.com/annsco/api"}`), 0o600)
	_ = os.WriteFile(filepath.Join(sessions, "s3.json"), []byte(`{"project":"api","remote":"gitlab.com/rival/api"}`), 0o600)

	planted := map[string]string{"server-only project": "saltmarsh", "doctor-listed server project": "lanternfish", "transverse alias": "workspace",
		"remote": "annsco", "other remote": "rival/api"}
	for _, n := range names {
		planted["local project "+n] = n
	}
	assertScrubbed(t, planted, []string{"two repositories share project:<project>", "(project:<project>) name another project: <project> (20)",
		"--project <project> failed", "flush of <project>, <project>,"})
	for _, args := range [][]string{{"report"}, {"report", "--json"}} {
		_, out, _ := run(t, args...)
		all := out + issueBodies(t, strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(out))
		for _, short := range []string{"project:api", "project%3Aapi", "--project ops"} {
			if strings.Contains(all, short) {
				t.Errorf("%v shows the short project %q:\n%s", args, short, out)
			}
		}
	}
	r := reportJSON(t)
	if c := reportCheck(r, "remotes"); c.Status != doctor.Warn {
		t.Errorf("remotes check %+v", c)
	}
	if c := reportCheck(r, "noise-transverse"); c.Status != doctor.Warn {
		t.Errorf("noise-transverse check %+v", c)
	}
	if r.Diagnostic.Counts.ErrorClasses["flush_failed"] != 1 {
		t.Errorf("classes %v", r.Diagnostic.Counts.ErrorClasses)
	}
}

// With the server down the bank's tags are out of reach: the local names
// are hidden all the same.
func TestReportHidesLocalProjectsWithTheServerDown(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	_ = os.MkdirAll(filepath.Dir(cfgFile(home)), 0o700)
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \"http://127.0.0.1:"+itoa(closedPort(t))+"\"\nui_port = "+itoa(closedPort(t))+
		"\n[projects]\ntransverse = [\"workspace\", \".claude\"]\n"), 0o600)
	names := localProjects(t, home, "workspace")
	planted := map[string]string{"transverse alias": "workspace"}
	for _, n := range names {
		planted["local project "+n] = n
	}
	assertScrubbed(t, planted, []string{"flush of <project>, <project>,", "doctor/server/error"})
}

// The live records and their .backsight.toml are read whatever the
// transverse setting: with projects.transverse empty (an invalid config)
// their names are hidden all the same, and a record in the home directory
// resolves to the default alias instead of failing.
func TestReportHidesLiveRecordProjectsWithoutTransverse(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	_ = os.MkdirAll(filepath.Dir(cfgFile(home)), 0o700)
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \"http://127.0.0.1:"+itoa(closedPort(t))+"\"\nui_port = "+itoa(closedPort(t))+
		"\n[projects]\ntransverse = []\n"), 0o600)
	localProjects(t, home)
	_ = os.WriteFile(filepath.Join(pathsOf(home).SessionsDir(), "by-pid", "4343.json"), []byte(`{"session_id":"s2","cwd":"`+home+`"}`), 0o600)
	assertScrubbed(t, map[string]string{"project from .backsight.toml": "mossgarden", "related from .backsight.toml": "tidewater"},
		[]string{"flush of <project>, <project>, <project>, <project>,"})
}

// Where the user's machines live never shows: LAN addresses (private,
// CGNAT, link-local, ULA; with ports, inside dial errors), hosts under a
// LAN suffix and a URL's user part, in the text, the JSON and the decoded
// links; loopback and public addresses stay (M5 review rules, Report).
func TestReportHidesLANAddressesHostsAndURLUsers(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	old := probes
	probes = func(e *doctor.Env) {
		old(e)
		e.ReferenceServer = func(context.Context, string) (doctor.RefServer, error) { return doctor.RefServer{Running: true}, nil }
	}
	t.Cleanup(func() { probes = old })
	_ = os.MkdirAll(filepath.Dir(cfgFile(home)), 0o700)
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \"http://127.0.0.1:"+itoa(closedPort(t))+"\"\nui_port = "+itoa(closedPort(t))+"\n"), 0o600)
	// A reference server on the LAN, reached with a user and a password
	// (assembled at runtime: the secret scanner flags literals).
	refPass := "quokka" + "Trellis7"
	ref := filepath.Join(home, ".hindsight", "coding-agent.json")
	_ = os.MkdirAll(filepath.Dir(ref), 0o700)
	_ = os.WriteFile(ref, []byte(`{"apiUrl":"http://annsrefuser:`+refPass+`@192.168.7.40:8888","bankId":"coding-agent::memoire"}`), 0o600)

	now := time.Now().UTC()
	var tel strings.Builder
	for ev, msg := range map[string]string{
		"dial_v4_failed":      "dial tcp 192.168.1.20:18888: connect: connection refused",
		"dial_url_failed":     `Get "http://10.20.30.40:8888/health": dial tcp 10.20.30.40:8888: i/o timeout`,
		"dial_cgnat_failed":   "dial tcp 100.101.102.103:8888: connect: connection refused",
		"dial_link_failed":    "dial tcp [fe80::abcd%eth0]:8888: connect: no route to host",
		"dial_ula_failed":     "dial tcp [fd12:3456::77]:8888: connect: connection refused",
		"lookup_lan_failed":   "dial tcp: lookup quokkanas.lan on 127.0.0.53:53: no such host",
		"lookup_local_failed": "open http://zebrabox.local:8888 and zebraback.home.arpa and zebradb.corp.internal",
		"url_user_failed":     "GET https://annsurluser@8.8.4.4/v1 via 127.0.0.1:8888",
	} {
		raw, _ := json.Marshal(map[string]any{"ts": now.Add(-time.Minute).Format(time.RFC3339Nano), "event": ev, "error": msg})
		tel.Write(append(raw, '\n'))
	}
	d := pathsOf(home)
	_ = os.MkdirAll(d.TelemetryDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.TelemetryDir(), "events-"+now.Format("2006-01")+".jsonl"), []byte(tel.String()), 0o600)

	assertScrubbed(t, map[string]string{
		"private ip": "192.168.1.20", "url ip": "10.20.30.40", "cgnat ip": "100.101.102.103", "link-local ip": "fe80::abcd", "ula ip": "fd12:3456::77",
		"reference ip": "192.168.7.40", "reference user": "annsrefuser", "reference password": refPass, "url user": "annsurluser",
		"lan host": "quokkanas", "local host": "zebrabox", "home.arpa host": "zebraback", "internal host": "zebradb",
	}, []string{
		"dial tcp <lan-ip>:18888", `"http://<lan-ip>:8888/health"`, "dial tcp [<lan-ip>]:8888", "lookup <lan-host> on 127.0.0.53:53",
		"http://<lan-host>:8888 and <lan-host> and <lan-host>", "https://<user>@8.8.4.4/v1 via 127.0.0.1:8888",
	})
}
