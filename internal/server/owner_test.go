package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// labelRunner answers docker ps with the working directories of the
// project's containers (one per line, as --format prints them) and
// records every call.
type labelRunner struct {
	calls [][]string
	dirs  []string
	psErr error
}

func (r *labelRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	if len(args) > 0 && args[0] == "ps" {
		if r.psErr != nil {
			return []byte("Cannot connect to the Docker daemon"), r.psErr
		}
		return []byte(strings.Join(r.dirs, "\n") + "\n"), nil
	}
	return nil, nil
}

// changed reports whether a call changed containers or volumes.
func (r *labelRunner) changed() []string {
	var out []string
	for _, c := range r.calls {
		joined := " " + strings.Join(c, " ") + " "
		for _, verb := range []string{" up ", " stop ", " down ", " rm ", " volume rm "} {
			if strings.Contains(joined, verb) && c[0] != "ps" {
				out = append(out, strings.Join(c, " "))
				break
			}
		}
	}
	return out
}

// ownerQuery is the docker ps that checks who owns project before a
// change to its containers.
func ownerQuery(project string) []string {
	return []string{"ps", "--all", "--filter", "label=com.docker.compose.project=" + project, "--format", `{{.Label "com.docker.compose.project.working_dir"}}`}
}

// install makes a backsight compose directory (one holding compose.yaml).
func install(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestComposeRefusesAProjectAnotherInstallOwns(t *testing.T) {
	ctx := context.Background()
	mine, other := install(t, filepath.Join(t.TempDir(), "mine")), install(t, filepath.Join(t.TempDir(), "other"))
	ops := map[string]func(Docker) error{
		"start":          func(d Docker) error { return d.Start(ctx) },
		"start database": func(d Docker) error { return d.StartDatabase(ctx) },
		"stop":           func(d Docker) error { return d.Stop(ctx) },
		"stop api":       func(d Docker) error { return d.StopAPI(ctx) },
		"delete":         func(d Docker) error { return d.Delete(ctx) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			r := &labelRunner{dirs: []string{other, other}}
			err := op(Docker{Compose: Compose{Dir: mine, Runner: r}})
			var fe *ForeignProjectError
			if !errors.As(err, &fe) {
				t.Fatalf("got %v, want a ForeignProjectError", err)
			}
			if fe.Other != other || fe.Project != "backsight" {
				t.Errorf("error names %q (project %q), want %q (backsight)", fe.Other, fe.Project, other)
			}
			for _, want := range []string{other, "server.project"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q does not name %q", err, want)
				}
			}
			if ch := r.changed(); len(ch) > 0 {
				t.Errorf("changed containers or volumes: %v", ch)
			}
			if !fileExists(filepath.Join(mine, "compose.yaml")) {
				t.Error("delete removed this install's compose.yaml after the refusal")
			}
		})
	}
}

// Without compose.yaml, Delete removes the project's containers by their
// label: it must not take another install's.
func TestDeleteWithoutComposeFileRefusesAnotherInstallsContainers(t *testing.T) {
	mine, other := filepath.Join(t.TempDir(), "mine"), install(t, filepath.Join(t.TempDir(), "other"))
	if err := os.MkdirAll(mine, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &labelRunner{dirs: []string{other}}
	err := Docker{Compose: Compose{Dir: mine, Runner: r}}.Delete(context.Background())
	var fe *ForeignProjectError
	if !errors.As(err, &fe) {
		t.Fatalf("got %v, want a ForeignProjectError", err)
	}
	if ch := r.changed(); len(ch) > 0 {
		t.Errorf("changed containers or volumes: %v", ch)
	}
}

func TestComposeDrivesItsOwnProject(t *testing.T) {
	base := t.TempDir()
	mine := install(t, filepath.Join(base, "mine"))
	link := filepath.Join(base, "link")
	if err := os.Symlink(mine, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	cases := map[string]struct {
		dir  string   // this install's compose_dir
		dirs []string // the containers' working dirs
	}{
		"no containers":            {mine, nil},
		"same directory":           {mine, []string{mine, mine}},
		"same directory, trailing": {mine, []string{mine + string(filepath.Separator)}},
		"through a symlink":        {link, []string{mine}},
		"labelled through a link":  {mine, []string{link}},
		"no working-dir label":     {mine, []string{""}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := &labelRunner{dirs: c.dirs}
			if err := (Compose{Dir: c.dir, Runner: r}).Up(context.Background()); err != nil {
				t.Fatalf("refused its own project: %v", err)
			}
			if !slices.ContainsFunc(r.calls, func(a []string) bool { return slices.Contains(a, "up") }) {
				t.Errorf("no compose up issued: %v", r.calls)
			}
		})
	}
}

// A home moved or renamed leaves containers labelled with its old path.
// Decision: when that path no longer holds a compose.yaml, no install
// drives the project any more, and the moved install adopts it (its next
// up relabels the containers). When it still does (a copy), it is another
// install: refused, the message saying how to adopt.
func TestComposeAdoptsTheProjectOfAMovedInstall(t *testing.T) {
	base := t.TempDir()
	mine := install(t, filepath.Join(base, "new-home"))
	for name, old := range map[string]string{
		"old directory gone":            filepath.Join(base, "old-home"),
		"old directory without compose": func() string { d := filepath.Join(base, "emptied"); _ = os.MkdirAll(d, 0o755); return d }(),
	} {
		t.Run(name, func(t *testing.T) {
			r := &labelRunner{dirs: []string{old}}
			if err := (Compose{Dir: mine, Runner: r}).Up(context.Background()); err != nil {
				t.Fatalf("a moved install was locked out: %v", err)
			}
		})
	}
	t.Run("copy still in place", func(t *testing.T) {
		old := install(t, filepath.Join(base, "copy"))
		r := &labelRunner{dirs: []string{old}}
		err := (Compose{Dir: mine, Runner: r}).Up(context.Background())
		var fe *ForeignProjectError
		if !errors.As(err, &fe) {
			t.Fatalf("got %v, want a ForeignProjectError", err)
		}
		if !strings.Contains(err.Error(), filepath.Join(old, "compose.yaml")) {
			t.Errorf("message %q does not say how to adopt (remove %s)", err, filepath.Join(old, "compose.yaml"))
		}
	})
}

func TestComposeOwnerCheckQueriesTheProjectLabel(t *testing.T) {
	r := &labelRunner{}
	c := Compose{Dir: install(t, t.TempDir()), Runner: r, Project: "bs-test"}
	if err := c.CheckOwner(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.calls[0], " ")
	for _, want := range []string{"ps --all", "label=com.docker.compose.project=bs-test", `{{.Label "com.docker.compose.project.working_dir"}}`} {
		if !strings.Contains(got, want) {
			t.Errorf("query %q lacks %q", got, want)
		}
	}
}

// A check that cannot run refuses: changing a project nobody could look
// at is what this guard prevents.
func TestComposeOwnerCheckFailureChangesNothing(t *testing.T) {
	r := &labelRunner{psErr: errors.New("exit status 1")}
	err := (Compose{Dir: install(t, t.TempDir()), Runner: r}).Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Fatalf("got %v", err)
	}
	if ch := r.changed(); len(ch) > 0 {
		t.Errorf("changed containers: %v", ch)
	}
}

// Read-only commands are never refused: status still lists another
// install's containers.
func TestComposeReadsAreNotGuarded(t *testing.T) {
	other := install(t, filepath.Join(t.TempDir(), "other"))
	r := &labelRunner{dirs: []string{other}}
	c := Compose{Dir: install(t, filepath.Join(t.TempDir(), "mine")), Runner: r}
	if _, err := c.PS(context.Background()); err != nil {
		t.Errorf("ps refused: %v", err)
	}
	if _, err := c.Logs(context.Background(), "hindsight", 5); err != nil {
		t.Errorf("logs refused: %v", err)
	}
}

func TestSameDirIgnoresCaseOnWindowsOnly(t *testing.T) {
	a, b := `C:\Users\Me\backsight`, `c:\users\me\backsight`
	if !sameDir(a, a) {
		t.Errorf("sameDir(%q, itself) = false", a)
	}
	if got := sameDir(a, b); got != (runtime.GOOS == "windows") {
		t.Errorf("sameDir(%q, %q) = %v on %s", a, b, got, runtime.GOOS)
	}
}
