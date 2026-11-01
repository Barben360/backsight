//go:build windows

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

// statusRefusal runs server status --json: its exit code, data.refusal
// and error message.
func statusRefusal(t *testing.T) (int, string, string) {
	t.Helper()
	code, out, _ := run(t, "server", "status", "--json")
	var env struct {
		Data struct {
			Refusal string `json:"refusal"`
		} `json:"data"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("server status --json: %v\n%s", err, out)
	}
	return code, env.Data.Refusal, env.Error.Message
}

// server status gives the native server's refusal on Windows (decision
// 0080: the reason where server status reads it): the supervisor's while
// it answers refused, in the JSON and the text; with no supervisor, the
// identity check's, which server start refused with, else the last
// supervisor's recorded refusal; none for the recorded instance once
// nothing is recorded.
func TestServerStatusGivesTheRefusal(t *testing.T) {
	p := "bs-test-" + strconv.Itoa(os.Getpid()) + "-cli-status"
	n, _ := superviseHost(t, p)
	// The API on a port nothing listens on: never healthy.
	if err := os.WriteFile(n.Dirs.ConfigFile(), []byte("[server]\nruntime = \"native\"\nproject = \""+p+"\"\nurl = \"http://127.0.0.1:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withInstance(t, n, "111", "")
	done := runSupervise(t, p)
	superviseStatus(t, p, "refused")
	code, refusal, msg := statusRefusal(t)
	if code != output.Blocked || !strings.Contains(refusal, "has no recorded identity") || !strings.Contains(msg, refusal) {
		t.Errorf("a refused supervisor: exit %d, refusal %q, message %q: want exit %d and the refusal in both", code, refusal, msg, output.Blocked)
	}
	if _, out, _ := run(t, "server", "status"); !strings.Contains(out, "refused: the native database ") {
		t.Errorf("the text does not give the refusal:\n%s", out)
	}
	stopSupervise(t, p, done)

	withInstance(t, n, "111", "222")
	if code, refusal, _ := statusRefusal(t); code != output.Blocked || !strings.Contains(refusal, "is not the one backsight recorded (identifier 111, recorded 222)") {
		t.Errorf("no supervisor, another identity recorded: exit %d, refusal %q", code, refusal)
	}
	// The recorded instance: the identity check passes, and the last
	// supervisor's refusal (<Root>\refused) is what status gives until a
	// start passes the guard, which removes it.
	withInstance(t, n, "111", "111")
	if code, refusal, _ := statusRefusal(t); code != output.Blocked || !strings.Contains(refusal, "has no recorded identity") {
		t.Errorf("no supervisor, the recorded instance, the last run refused: exit %d, refusal %q, want the last run's", code, refusal)
	}
	if err := os.Remove(filepath.Join(n.Root(), "refused")); err != nil {
		t.Fatal(err)
	}
	if code, refusal, _ := statusRefusal(t); code != output.Blocked || refusal != "" {
		t.Errorf("no supervisor, the recorded instance: exit %d, refusal %q, want none", code, refusal)
	}
}
