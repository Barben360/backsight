package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
)

func sessions(home string) session.Store {
	d := pathsOf(home)
	return session.Store{Dir: d.SessionsDir(), Locks: d.LocksDir()}
}

func TestWhyShowsTheLastInjectionsAndTheirRules(t *testing.T) {
	home := isolate(t)
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	_ = sessions(home).Update("s1", func(st *session.State) error {
		st.Project = "billing"
		for i := 1; i <= 7; i++ {
			st.Injected = append(st.Injected, session.Injection{ID: "m" + itoa(i), Text: "memory " + itoa(i), Project: "billing", Score: 0.71, Scope: "project", Prompt: i, Rule: "gap", At: at})
		}
		return nil
	})
	t.Setenv("BACKSIGHT_SESSION", "s1")
	code, out, _ := run(t, "why")
	if code != output.OK || !strings.Contains(out, "memory 7") || !strings.Contains(out, "gap") || !strings.Contains(out, "0.71") || strings.Contains(out, "memory 2 ") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if code, out, _ := run(t, "why", "--last", "10"); code != output.OK || !strings.Contains(out, "memory 1") {
		t.Errorf("--last 10: exit %d\n%s", code, out)
	}
}

func TestWhyPicksTheGivenOrTheLatestSession(t *testing.T) {
	home := isolate(t)
	_ = sessions(home).Update("old", func(st *session.State) error {
		st.Injected = []session.Injection{{ID: "o1", Text: "old memory", Rule: "first"}}
		return nil
	})
	time.Sleep(10 * time.Millisecond)
	_ = sessions(home).Update("new", func(st *session.State) error {
		st.Injected = []session.Injection{{ID: "n1", Text: "new memory", Rule: "first"}}
		return nil
	})
	t.Setenv("BACKSIGHT_SESSION", "")
	if _, out, _ := run(t, "why"); !strings.Contains(out, "new memory") {
		t.Errorf("latest: %s", out)
	}
	if _, out, _ := run(t, "why", "--session", "old"); !strings.Contains(out, "old memory") {
		t.Errorf("--session: %s", out)
	}
}

func TestWhyWithoutInjections(t *testing.T) {
	isolate(t)
	t.Setenv("BACKSIGHT_SESSION", "")
	if code, out, _ := run(t, "why"); code != output.OK || !strings.Contains(out, "no injection") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "why", "--session", "../etc"); code != output.Usage {
		t.Errorf("bad id: exit %d", code)
	}
}
