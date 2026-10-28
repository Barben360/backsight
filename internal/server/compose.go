package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Runner executes docker with args and returns combined output.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// DockerRunner runs the docker CLI (Compose v2 plugin). OwnGroup runs each
// docker command in its own process group: a Ctrl-C at the terminal then
// reaches backsight only, which cancels its context, instead of killing
// the docker compose a rollback is running.
type DockerRunner struct{ OwnGroup bool }

// dockerWaitDelay bounds the wait for docker's output once its context
// ended and docker was killed: an older CLI does not end the Compose plugin
// it started, which keeps the output open (with a hung daemon, until the
// daemon answers). A bounded look, as a hook's, then still ends in time.
const dockerWaitDelay = 500 * time.Millisecond

func (r DockerRunner) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = dockerWaitDelay
	if r.OwnGroup {
		ownGroup(cmd)
	}
	return cmd
}

func (r DockerRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return r.command(ctx, args...).CombinedOutput()
}

// Streamer runs a command with its output going to stdout and stderr as
// it comes (an image pull's progress at a terminal).
type Streamer interface {
	Stream(ctx context.Context, stdout, stderr io.Writer, args ...string) error
}

// Stream runs docker on stdout and stderr: a terminal as stdout (an
// *os.File) gets Docker's own progress bars.
func (r DockerRunner) Stream(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	cmd := r.command(ctx, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// StdoutRunner can return a command's stdout alone, stderr only in the
// error: a value read from output must not catch a warning (Compose prints
// them on stderr, e.g. an obsolete `version:` key in an override).
type StdoutRunner interface {
	RunStdout(ctx context.Context, args ...string) ([]byte, error)
}

func (r DockerRunner) RunStdout(ctx context.Context, args ...string) ([]byte, error) {
	out, err := r.command(ctx, args...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// stdout runs args and returns their stdout alone when the runner can.
func (c Compose) stdout(ctx context.Context, sub ...string) ([]byte, error) {
	if r, ok := c.Runner.(StdoutRunner); ok {
		out, err := r.RunStdout(ctx, c.args(sub...)...)
		if err != nil {
			return out, fmt.Errorf("docker compose %s: %w", strings.Join(sub, " "), err)
		}
		return out, nil
	}
	return c.run(ctx, sub...)
}

// Compose drives the backsight compose project in Dir.
type Compose struct {
	Dir     string
	Runner  Runner
	Project string // "" is the default project
}

func (c Compose) project() string {
	if c.Project == "" {
		return Project
	}
	return c.Project
}

func (c Compose) args(sub ...string) []string {
	a := []string{"compose", "-p", c.project(), "--project-directory", c.Dir, "-f", filepath.Join(c.Dir, "compose.yaml")}
	if override := filepath.Join(c.Dir, "compose.override.yaml"); fileExists(override) {
		a = append(a, "-f", override)
	}
	return append(a, sub...)
}

func (c Compose) run(ctx context.Context, sub ...string) ([]byte, error) {
	out, err := c.Runner.Run(ctx, c.args(sub...)...)
	if err != nil {
		return out, fmt.Errorf("docker compose %s: %w: %s", strings.Join(sub, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Pull downloads the pinned images.
func (c Compose) Pull(ctx context.Context) error { _, err := c.run(ctx, "pull"); return err }

// PullImage downloads one image, outside the project: nothing that runs
// changes.
func (c Compose) PullImage(ctx context.Context, image string) error {
	out, err := c.Runner.Run(ctx, "pull", image)
	if err != nil {
		return fmt.Errorf("docker pull %s: %w: %s", image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Up starts the project and waits until Compose sees it running and healthy.
func (c Compose) Up(ctx context.Context) error {
	_, err := c.run(ctx, "up", "-d", "--wait")
	return err
}

// Stop stops the containers of services (none: all) and keeps them and
// their volumes.
func (c Compose) Stop(ctx context.Context, services ...string) error {
	_, err := c.run(ctx, append([]string{"stop"}, services...)...)
	return err
}

// Container is one service's state as Compose reports it.
type Container struct {
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

// PS lists the project's containers; Compose prints JSON lines or, in
// older versions, one array.
func (c Compose) PS(ctx context.Context) ([]Container, error) {
	out, err := c.run(ctx, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	var list []Container
	if len(out) > 0 && out[0] == '[' {
		return list, json.Unmarshal(out, &list)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var ct Container
		if err := json.Unmarshal(sc.Bytes(), &ct); err != nil {
			return nil, fmt.Errorf("docker compose ps: %w", err)
		}
		list = append(list, ct)
	}
	return list, sc.Err()
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// DataVolume is the database volume Compose creates for the project.
func (c Compose) DataVolume() string { return c.project() + "_" + dataVolume }

// dataVolume is the database volume as compose.yaml declares it;
// modelsVolume holds the embedding models the server downloads at its
// first start.
const (
	dataVolume   = "pg_data"
	modelsVolume = "hf_cache"
)

// VolumeExists reports whether the project's database volume exists: its
// password was fixed when it was created.
func (c Compose) VolumeExists(ctx context.Context) (bool, error) {
	out, err := c.Runner.Run(ctx, "volume", "inspect", c.DataVolume())
	switch {
	case err == nil:
		return true, nil
	case strings.Contains(strings.ToLower(string(out)), "no such volume"):
		return false, nil
	}
	return false, fmt.Errorf("docker volume inspect %s: %w: %s", c.DataVolume(), err, strings.TrimSpace(string(out)))
}

// RemoveVolume deletes the project's volume name (as compose.yaml
// declares it); one already gone is not an error.
func (c Compose) RemoveVolume(ctx context.Context, name string) error {
	v := c.project() + "_" + name
	out, err := c.Runner.Run(ctx, "volume", "rm", v)
	if err != nil && !strings.Contains(strings.ToLower(string(out)), "no such volume") {
		return fmt.Errorf("docker volume rm %s: %w: %s", v, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DeclaredVolumes lists the volumes of a compose file's top-level volumes
// section, in order (a service's own volumes list declares none).
func DeclaredVolumes(compose []byte) []string {
	var out []string
	in := false
	sc := bufio.NewScanner(bytes.NewReader(compose))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "volumes:":
			in = true
		case line == "" || strings.HasPrefix(strings.TrimSpace(line), "#"):
		case !strings.HasPrefix(line, " "):
			in = false
		case in && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "):
			if name, _, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// Logs returns the last n lines of service's log.
func (c Compose) Logs(ctx context.Context, service string, n int) (string, error) {
	out, err := c.run(ctx, "logs", "--no-color", "--tail", strconv.Itoa(n), service)
	return string(out), err
}

// OwnGroup runs cmd in its own process group: a Ctrl-C at the terminal
// reaches backsight only, which cancels its context (update apply runs the
// binaries it installs and rolls back to this way).
func OwnGroup(cmd *exec.Cmd) { ownGroup(cmd) }

// TerminateGroup asks the process group of a child started with OwnGroup
// to stop (SIGTERM; CTRL_BREAK on Windows): a backsight child cancels its
// context and rolls back. A group already gone is os.ErrProcessDone.
func TerminateGroup(p *os.Process) error { return terminateGroup(p) }
