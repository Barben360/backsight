package server

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Compose labels each container with the directory it was created from
// (backsight passes compose_dir as --project-directory).
const (
	projectLabel    = "com.docker.compose.project"
	workingDirLabel = "com.docker.compose.project.working_dir"
)

// ForeignProjectError: the Compose project's containers were created from
// another backsight install's compose directory (issue #144). Driving the
// project from Dir would recreate or remove that install's server.
type ForeignProjectError struct {
	Project string // the Compose project name
	Dir     string // this install's compose_dir
	Other   string // the compose directory its containers come from
}

func (e *ForeignProjectError) Error() string {
	return fmt.Sprintf("the Docker Compose project %q belongs to another backsight install, in %s (this one is in %s). "+
		"Set server.project in this install's config.toml to a name of its own, or run the command from the install in %s. "+
		"If %s is an old copy of this install that nothing runs any more, delete %s and run the command again: this install then takes the project over",
		e.Project, e.Other, e.Dir, e.Other, e.Other, filepath.Join(e.Other, "compose.yaml"))
}

// changes reports whether a compose subcommand creates, changes or
// removes containers: those are refused on a project another install owns.
func changes(sub []string) bool {
	return len(sub) > 0 && slices.Contains([]string{"up", "down", "stop", "start", "restart", "rm", "create", "kill", "pause", "unpause", "run"}, sub[0])
}

// CheckOwner returns a *ForeignProjectError when the project's containers
// come from another install's compose directory, one that still holds a
// compose.yaml. No containers, containers from Dir (compared after
// resolving symlinks) or without the label, and containers whose
// directory no longer holds a compose.yaml (this install, moved or
// renamed: its next up takes them over) are this install's to drive.
func (c Compose) CheckOwner(ctx context.Context) error {
	args := []string{"ps", "--all", "--filter", "label=" + projectLabel + "=" + c.project(), "--format", `{{.Label "` + workingDirLabel + `"}}`}
	var out []byte
	var err error
	if r, ok := c.Runner.(StdoutRunner); ok { // a warning on stderr is no directory
		out, err = r.RunStdout(ctx, args...)
	} else {
		out, err = c.Runner.Run(ctx, args...)
	}
	if err != nil {
		return fmt.Errorf("cannot check which install owns the Docker Compose project %q: docker ps: %w: %s", c.project(), err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		dir := strings.TrimSpace(line)
		if dir == "" || sameDir(dir, c.Dir) || !fileExists(filepath.Join(dir, "compose.yaml")) {
			continue
		}
		return &ForeignProjectError{Project: c.project(), Dir: c.Dir, Other: filepath.Clean(dir)}
	}
	return nil
}

// sameDir compares two directories after resolving symlinks; Windows
// paths ignore case.
func sameDir(a, b string) bool {
	a, b = resolved(a), resolved(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

// CheckOwner is the compose project's owner check (Compose.CheckOwner).
func (d Docker) CheckOwner(ctx context.Context) error { return d.Compose.CheckOwner(ctx) }
