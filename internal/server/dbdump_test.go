package server

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptRunner records each call as one line and fails the first one
// containing failOn.
type scriptRunner struct {
	calls  []string
	failOn string
	out    map[string]string // substring → stdout
}

func (s *scriptRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	line := strings.Join(args, " ")
	s.calls = append(s.calls, line)
	if s.failOn != "" && strings.Contains(line, s.failOn) {
		return []byte("boom"), errors.New("exit status 1")
	}
	for k, v := range s.out {
		if strings.Contains(line, k) {
			return []byte(v), nil
		}
	}
	return nil, nil
}

// The database is replaced whole: dropped (other connections cut), created
// empty, then restored from the dump copied into the container, which is
// removed afterwards whatever happens.
func TestDBRestoreReplacesTheDatabase(t *testing.T) {
	r := &scriptRunner{}
	db := DB{Compose: Compose{Dir: t.TempDir(), Runner: r, Project: "p"}}
	if err := db.Restore(context.Background(), "/state/staging/restore/db.dump"); err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, c := range r.calls {
		_, sub, _ := strings.Cut(c, "compose.yaml ")
		steps = append(steps, sub)
	}
	got := strings.Join(steps, "\n")
	tmp := strings.Fields(steps[0])[2][len("db:"):]
	want := strings.Join([]string{
		"cp /state/staging/restore/db.dump db:" + tmp,
		"exec -T db dropdb -U hindsight --if-exists --force hindsight",
		"exec -T db createdb -U hindsight hindsight",
		"exec -T db pg_restore -U hindsight -d hindsight --no-owner --exit-on-error " + tmp,
		"exec -T db rm -f " + tmp,
	}, "\n")
	if got != want || !strings.HasPrefix(tmp, "/tmp/backsight-") {
		t.Errorf("calls:\n%s\nwant:\n%s", got, want)
	}

	r = &scriptRunner{failOn: "pg_restore"}
	db.Compose.Runner = r
	if err := db.Restore(context.Background(), "/x.dump"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a failed pg_restore: %v", err)
	}
	if last := r.calls[len(r.calls)-1]; !strings.Contains(last, "exec -T db rm -f /tmp/backsight-") {
		t.Errorf("the copy was not removed after a failure: %q", r.calls)
	}
}

func TestDBCounts(t *testing.T) {
	r := &scriptRunner{out: map[string]string{"from memory_units": "12\n", "from documents": "3\n"}}
	got, err := DB{Compose: Compose{Dir: t.TempDir(), Runner: r}}.Counts(context.Background(), []string{"memory_units", "documents"})
	if err != nil || got["memory_units"] != 12 || got["documents"] != 3 || len(got) != 2 {
		t.Errorf("%v %v", got, err)
	}
	r.out = map[string]string{"from memory_units": "ERROR\n"}
	if _, err := (DB{Compose: Compose{Dir: t.TempDir(), Runner: r}}).Counts(context.Background(), []string{"memory_units"}); err == nil {
		t.Error("a count that is not a number was accepted")
	}
}
