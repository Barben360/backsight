package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Barben360/backsight/internal/config"
)

// Runtime runs the local Hindsight server (decision 0070). Docker Compose
// is the only implementation; a native one (Hindsight from PyPI on an
// embedded Postgres, under the OS's service manager, M7) comes behind the
// same interface. Every command that installs, starts, stops, inspects or
// deletes the server goes through it.
type Runtime interface {
	// Kind names the runtime ("docker").
	Kind() string
	// Version is the server version Render deploys: for Docker, the
	// Hindsight image reference (the binary's pin, or config's digest),
	// unless WithVersion set another.
	Version() string
	// WithVersion is the same runtime deploying version v ("": the pin).
	WithVersion(v string) Runtime
	// Deployed is the version the server's files run now, "" when there
	// are none (the server was never installed).
	Deployed() (string, error)
	// ComparePins orders two versions; comparable is false when neither
	// can be placed (restore refuses an archive from a newer server).
	ComparePins(a, b string) (cmp int, comparable bool)
	// Render builds the server's files for Version; host names the worker.
	Render(sec config.Secrets, host string) (Files, error)
	// Write installs files, only where they changed.
	Write(f Files) (Changes, error)
	// Diff reports what Write would change, writing nothing.
	Diff(f Files) (Changes, error)
	// Fetch downloads Version (an image pull) without changing what runs.
	Fetch(ctx context.Context) error
	// FetchAll downloads everything the written files run (the API and
	// the database).
	FetchAll(ctx context.Context) error
	// Start starts the server as written and waits until it runs.
	Start(ctx context.Context) error
	// StopAPI stops the API server only; the database keeps running.
	StopAPI(ctx context.Context) error
	// Stop stops the whole server and keeps its data.
	Stop(ctx context.Context) error
	// StopCmd is how a user stops it by hand, once backsight is gone.
	StopCmd() string
	// Delete removes the server with its data (services, database) and
	// the files Write installed; a user's own files and volumes beside them
	// stay.
	// Backups are not the runtime's and are never touched.
	Delete(ctx context.Context) error
	// Status lists the server's services and their state; none when it
	// was never started.
	Status(ctx context.Context) ([]Container, error)
	// Logs returns the last n lines of service's log.
	Logs(ctx context.Context, service string, n int) (string, error)
	// DataExists reports whether the server's data exists (for Docker,
	// the database volume): its database password was fixed when it was
	// created.
	DataExists(ctx context.Context) (bool, error)
	// DataName names the server's data in messages; DataRemoveCmd is how a
	// user deletes it by hand.
	DataName() string
	DataRemoveCmd() string
	// Engine is what runs the server on this machine (Docker's daemon),
	// for doctor and setup's pre-flight. Not installed wraps
	// exec.ErrNotFound; not usable carries the engine's own output.
	Engine(ctx context.Context) (Engine, error)
	// Orchestrator is the version of what runs the server's services
	// (Docker Compose), for setup's pre-flight.
	Orchestrator(ctx context.Context) (string, error)
	// Database is the server's Postgres.
	Database() Database
	// VerifyDump restores a dump into a throwaway database and counts the
	// rows of tables (backup verify, the upgrade's safety backup).
	VerifyDump(ctx context.Context, dump string, tables []string) (map[string]int64, error)
}

// Database is what backups, restores and upgrades need from the server's
// Postgres.
type Database interface {
	Query(ctx context.Context, sql string) (string, error)
	Dump(ctx context.Context, dst string, tables []string) (map[string]int64, error)
	// Restore replaces the database with a dump; dropped reports that the
	// old one was dropped (a rollback must then restore one).
	Restore(ctx context.Context, src string) (dropped bool, err error)
	Counts(ctx context.Context, tables []string) (map[string]int64, error)
	RemoveStaleDumps(ctx context.Context) error
	DocumentMetadata(ctx context.Context, bank, id string) (map[string]string, error)
}

// Engine is a runtime's engine: its version, and whether it runs without
// root privileges.
type Engine struct {
	Version  string
	Rootless bool
}

// Docker is the Compose runtime: the project in Compose.Dir.
type Docker struct {
	Compose Compose
	Cfg     config.Config
	Image   string // "" = ImageFor(Cfg)
}

var _ Runtime = Docker{}

func (Docker) Kind() string { return "docker" }

func (d Docker) Version() string {
	if d.Image != "" {
		return d.Image
	}
	return ImageFor(d.Cfg)
}

func (d Docker) WithVersion(v string) Runtime { d.Image = v; return d }

// Deployed reads the hindsight service's image from compose.yaml.
func (d Docker) Deployed() (string, error) {
	raw, err := os.ReadFile(filepath.Join(d.Compose.Dir, "compose.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ServiceImage(raw, "hindsight"), nil
}

// ServiceImage is the image of service in a compose file backsight
// rendered ("" when absent).
func ServiceImage(compose []byte, service string) string {
	sc := bufio.NewScanner(bytes.NewReader(compose))
	in := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "  "+service+":":
			in = true
		case in && strings.HasPrefix(line, "    image: "):
			return strings.TrimSpace(strings.TrimPrefix(line, "    image: "))
		case in && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    "):
			in = false
		case in && line != "" && !strings.HasPrefix(line, " "):
			in = false
		}
	}
	return ""
}

func (Docker) ComparePins(a, b string) (int, bool) { return CompareImages(a, b) }

func (d Docker) Render(sec config.Secrets, host string) (Files, error) {
	return RenderImage(d.Cfg, sec, host, d.Version())
}

func (d Docker) Write(f Files) (Changes, error) { return Write(d.Compose.Dir, f) }

func (d Docker) Diff(f Files) (Changes, error) { return Diff(d.Compose.Dir, f) }

func (d Docker) Fetch(ctx context.Context) error { return d.Compose.PullImage(ctx, d.Version()) }

func (d Docker) FetchAll(ctx context.Context) error { return d.Compose.Pull(ctx) }

func (d Docker) Start(ctx context.Context) error { return d.Compose.Up(ctx) }

func (d Docker) StopAPI(ctx context.Context) error { return d.Compose.Stop(ctx, "hindsight") }

func (d Docker) Stop(ctx context.Context) error { return d.Compose.Stop(ctx) }

func (d Docker) StopCmd() string {
	words := []string{"docker"}
	for _, a := range d.Compose.args("stop") {
		if strings.ContainsAny(a, " '\"$\\") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// Delete runs compose down without the volumes, removes the database
// volume alone, then compose.yaml and .env. A volume the user's
// compose.override.yaml declares stays, and so does the override; the
// model cache (hf_cache) is downloaded data, like the images (decision
// 0067 block 3 review).
func (d Docker) Delete(ctx context.Context) error {
	if _, err := d.Compose.run(ctx, "down", "--remove-orphans"); err != nil {
		return err
	}
	if err := d.Compose.RemoveDataVolume(ctx); err != nil {
		return err
	}
	for _, f := range []string{"compose.yaml", ".env"} {
		if err := os.Remove(filepath.Join(d.Compose.Dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (d Docker) Status(ctx context.Context) ([]Container, error) { return d.Compose.PS(ctx) }

func (d Docker) Logs(ctx context.Context, service string, n int) (string, error) {
	return d.Compose.Logs(ctx, service, n)
}

func (d Docker) DataExists(ctx context.Context) (bool, error) { return d.Compose.VolumeExists(ctx) }

func (d Docker) DataName() string { return "the database volume " + d.Compose.DataVolume() }

func (d Docker) DataRemoveCmd() string { return "docker volume rm " + d.Compose.DataVolume() }

// Engine asks the Docker daemon for its version and security options.
func (d Docker) Engine(ctx context.Context) (Engine, error) {
	out, err := d.Compose.Runner.Run(ctx, "info", "--format", "{{.ServerVersion}} {{range .SecurityOptions}}{{.}} {{end}}")
	if err != nil {
		if text := strings.TrimSpace(string(out)); text != "" {
			err = fmt.Errorf("%w: %s", err, text)
		}
		return Engine{}, err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return Engine{}, nil
	}
	return Engine{Version: f[0], Rootless: strings.Contains(string(out), "rootless")}, nil
}

// Orchestrator is `docker compose version --short`.
func (d Docker) Orchestrator(ctx context.Context) (string, error) {
	out, err := d.Compose.Runner.Run(ctx, "compose", "version", "--short")
	return strings.TrimSpace(string(out)), err
}

func (d Docker) Database() Database { return DB{Compose: d.Compose} }

func (d Docker) VerifyDump(ctx context.Context, dump string, tables []string) (map[string]int64, error) {
	return RestoreCounts(ctx, d.Compose.Runner, dump, tables)
}
