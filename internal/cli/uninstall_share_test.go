package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
)

// sharedInstall is installMachine on the fake server, with this
// machine's shared document and another machine's there, as their ticks
// left them.
func sharedInstall(t *testing.T) (installed, string, *hindsight.Client) {
	t.Helper()
	m := installMachine(t)
	pointAt(t, m.home, fakeHindsight(t))
	d := pathsOf(m.home)
	machine, err := retain.MachineID(d.State)
	if err != nil {
		t.Fatal(err)
	}
	cl, _ := hindsight.New(serverURL(t, m.home), "k", 5*time.Second)
	for _, pc := range []string{machine, "pc-b"} {
		if err := retain.WriteControlDocument(context.Background(), cl, config.LegacyBank, "backsight:feedback:"+pc, "x", map[string]string{"feedback": "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := feedback.SaveSharedUpload(d.FeedbackDir(), feedback.SharedUpload{At: time.Now(), Hash: "h", HiddenHash: "h"}); err != nil {
		t.Fatal(err)
	}
	return m, machine, cl
}

// sharedDocs is the ids of the shared documents on cl's server.
func sharedDocs(t *testing.T, cl *hindsight.Client) []string {
	t.Helper()
	items, err := retain.ControlDocuments(context.Background(), cl, config.LegacyBank, "backsight:feedback:")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, strings.TrimPrefix(it.ID, "backsight:feedback:"))
	}
	return ids
}

func stepIndex(r uninstallResult, name string) int {
	for i, s := range r.Steps {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// An uninstall that deletes this machine's id (--purge-local) deletes its
// shared document first, before the server stops, and leaves the other
// machines'; --plan lists it, and an uninstall that keeps the id keeps
// the document.
func TestUninstallDeletesThisMachinesDocument(t *testing.T) {
	m, machine, cl := sharedInstall(t)
	code, env, out := uninstall(t, "--plan", "--stop", "--purge-local")
	if s := stepNamed(env.Data, "shared counts"); code != output.OK || s == nil || s.Status != "planned" || !strings.Contains(s.Detail, machine) {
		t.Fatalf("plan: exit %d\n%s", code, out)
	}
	if code, env, out := uninstall(t); code != output.OK || stepNamed(env.Data, "shared counts") != nil {
		t.Fatalf("without --purge-local: exit %d\n%s", code, out)
	}
	if got := sharedDocs(t, cl); len(got) != 2 {
		t.Fatalf("an uninstall keeping the id deleted %v", got)
	}

	code, env, out = uninstall(t, "--stop", "--purge-local", "--answer", "identity_saved=yes")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if s := stepNamed(env.Data, "shared counts"); s == nil || s.Status != "done" {
		t.Fatalf("no delete step:\n%s", out)
	}
	if i, j := stepIndex(env.Data, "shared counts"), stepIndex(env.Data, "server"); i > j || env.Data.Steps[j].Status != "done" {
		t.Errorf("the server stopped before the delete: %+v", env.Data.Steps)
	}
	if got := sharedDocs(t, cl); len(got) != 1 || got[0] != "pc-b" {
		t.Errorf("server holds %v, want pc-b's alone", got)
	}
	if onDisk(pathsOf(m.home).StateFile()) {
		t.Error("state not purged")
	}
}

// A server that fails or never answers: the uninstall goes on within its
// budget, and says in one line that the document stays and that the other
// machines ignore it after 30 days.
func TestUninstallSurvivesAnUnreachableServer(t *testing.T) {
	old := uninstallShareBudget
	uninstallShareBudget = 300 * time.Millisecond
	t.Cleanup(func() { uninstallShareBudget = old })
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(hang.Close)
	hangPort, _ := strconv.Atoi(hang.URL[strings.LastIndex(hang.URL, ":")+1:])
	// A server that refuses with a body of several lines.
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Internal error\n  at line two\r\n\tand three"))
	}))
	t.Cleanup(refuse.Close)
	refusePort, _ := strconv.Atoi(refuse.URL[strings.LastIndex(refuse.URL, ":")+1:])
	for name, port := range map[string]int{"down": closedPort(t), "hanging": hangPort, "500": refusePort} {
		m, _, _ := sharedInstall(t)
		pointAt(t, m.home, port)
		start := time.Now()
		code, env, out := uninstall(t, "--purge-local", "--answer", "identity_saved=yes")
		if code != output.OK {
			t.Fatalf("%s: exit %d\n%s", name, code, out)
		}
		if took := time.Since(start); took > 4*time.Second {
			t.Errorf("%s: took %s", name, took)
		}
		s := stepNamed(env.Data, "shared counts")
		if s == nil || s.Status != "kept" || !strings.Contains(s.Detail, "30 days") || strings.ContainsAny(s.Detail, "\n\r\t") {
			t.Errorf("%s: %+v", name, s)
		}
		if name == "500" && !strings.Contains(s.Detail, "Internal error at line two and three") {
			t.Errorf("500: the server's answer is not on the line: %q", s.Detail)
		}
		if onDisk(pathsOf(m.home).StateFile()) {
			t.Errorf("%s: the uninstall stopped short", name)
		}
	}
}

// Another server answers at the joined address: nothing is deleted there.
func TestUninstallLeavesAnotherServerAlone(t *testing.T) {
	m, _, cl := sharedInstall(t)
	recordMismatch(t, m.home)
	code, env, out := uninstall(t, "--purge-local", "--answer", "identity_saved=yes")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if s := stepNamed(env.Data, "shared counts"); s == nil || s.Status != "skipped" {
		t.Errorf("step: %+v", s)
	}
	if got := sharedDocs(t, cl); len(got) != 2 {
		t.Errorf("deleted on another server: %v left", got)
	}
	for _, c := range curation {
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("a delete reached another server: %s", c)
		}
	}
}

// A machine that never sent its counts (no record of a send) or has no
// machine id: nothing to delete, the server is not asked.
func TestUninstallSkipsAMachineThatNeverShared(t *testing.T) {
	for name, spoil := range map[string]func(installed) error{
		"no send recorded": func(m installed) error {
			return os.Remove(feedback.SharedUploadFile(pathsOf(m.home).FeedbackDir()))
		},
		"no machine id": func(m installed) error { return os.Remove(filepath.Join(pathsOf(m.home).State, "machine_id")) },
	} {
		m, _, cl := sharedInstall(t)
		if err := spoil(m); err != nil {
			t.Fatal(err)
		}
		code, env, out := uninstall(t, "--purge-local", "--answer", "identity_saved=yes")
		if code != output.OK {
			t.Fatalf("%s: exit %d\n%s", name, code, out)
		}
		if s := stepNamed(env.Data, "shared counts"); s == nil || s.Status != "skipped" {
			t.Errorf("%s: step %+v", name, s)
		}
		if got := sharedDocs(t, cl); len(got) != 2 {
			t.Errorf("%s: deleted, %v left", name, got)
		}
		for _, c := range curation {
			if strings.HasPrefix(c, "DELETE") {
				t.Errorf("%s: a delete was sent: %s", name, c)
			}
		}
	}
}

// A connect while uninstall runs (after it read its config): this
// machine's counts are not deleted on the server it loaded.
func TestUninstallDeletesNothingAfterAConnect(t *testing.T) {
	m, _, cl := sharedInstall(t)
	connectAfterLoad(t, m.home)
	code, env, out := uninstall(t, "--purge-local", "--answer", "identity_saved=yes")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if s := stepNamed(env.Data, "shared counts"); s == nil || s.Status != "skipped" || !strings.Contains(s.Detail, "joined another server") {
		t.Errorf("step: %+v", s)
	}
	if got := sharedDocs(t, cl); len(got) != 2 {
		t.Errorf("deleted after the connect: %v left", got)
	}
}
