package cli

import (
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

	"github.com/Barben360/backsight/internal/output"
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
		_, _ = w.Write([]byte(`{"detail":"extraction failed on: ` + p.memory + ` api_key=` + p.secret + `"}`))
	}))
	t.Cleanup(srv.Close)
	_ = os.MkdirAll(filepath.Join(home, "backsight", "config"), 0o700)
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \""+srv.URL+"\"\nui_port = 19999\n"), 0o600)

	d := pathsOf(home)
	now := time.Now().UTC()
	ts := now.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
	lines := []map[string]any{
		{"ts": ts, "event": "session_state_error", "project": "ann-secret-project",
			"error": "open " + home + "/work/annsbox.lan/state.json with token=" + p.secret + ": POST /v1/x: HTTP 422 (bad_request): {\"input\":\"" + p.prompt + "\",\"memory\":\"" + p.memory + "\"}"},
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

func TestReportNeverShowsSecretsMemoriesPromptsOrHome(t *testing.T) {
	for _, args := range [][]string{{"report"}, {"report", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			p := plantedMachine(t)
			code, out, stderr := run(t, args...)
			if code != output.OK {
				t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
			}
			// JSON escapes <, > and &: compare the text they stand for.
			plain := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(out)
			bodies := issueBodies(t, plain)
			if !strings.Contains(bodies, "**Versions**") {
				t.Fatalf("no issue body found in:\n%s", out)
			}
			all := plain + stderr + bodies
			for what, v := range map[string]string{"secret": p.secret, "memory": p.memory, "prompt": p.prompt, "home": p.home,
				"host": "annsbox", "project": "ann-secret-project"} {
				for _, form := range []string{v, url.QueryEscape(v)} {
					if strings.Contains(all, form) {
						t.Errorf("the report shows the planted %s %q:\n%s", what, form, out)
					}
				}
			}
			// The planted values did reach the scrubber: what replaced them shows.
			for _, want := range []string{"[redacted:", "~/backsight/config/config.toml", "~/work/<host>/state.json", "[server answer omitted]",
				"doctor/server/error", "hook_failure/prompt/timeout", "recall/scope_failed", "session_state_error", "job/tick/partial"} {
				if !strings.Contains(all, want) {
					t.Errorf("the report lacks %q:\n%s", want, out)
				}
			}
		})
	}
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
	_ = os.WriteFile(cfgFile(home), []byte("[server]\nurl = \""+srv.URL+"\"\nbank = \"ann-private-notes\"\nui_port = 19999\n"), 0o600)
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
	isolate(t)
	withClaude(t)
	code, out, _ := run(t, "report", "--json")
	var env struct {
		Data report.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d, %v:\n%s", code, err, out)
	}
	// isolate's machine warns about the transcript retention only.
	if env.Data.Class != "doctor/retention/warn" || env.Data.Diagnostic.Doctor.Errors != 0 {
		t.Errorf("class %q, doctor %+v", env.Data.Class, env.Data.Diagnostic.Doctor)
	}
	if env.Data.Diagnostic.RecentErrors == nil || len(env.Data.Diagnostic.RecentErrors) != 0 {
		t.Errorf("recent errors %v, want an empty list", env.Data.Diagnostic.RecentErrors)
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
