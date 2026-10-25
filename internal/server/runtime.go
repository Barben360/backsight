package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Barben360/backsight/internal/config"
)

// Runtime runs the local Hindsight server (decision 0070). Docker Compose
// is the only implementation; a native one (Hindsight from PyPI on an
// embedded Postgres, under the OS's service manager, M7) comes behind the
// same interface. It holds what `server upgrade` and `restore` need; other
// commands still call Compose directly (listed in decision 0070's
// follow-up).
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
	// Fetch downloads Version (an image pull) without changing what runs.
	Fetch(ctx context.Context) error
	// Start starts the server as written and waits until it runs.
	Start(ctx context.Context) error
	// StopAPI stops the API server only; the database keeps running.
	StopAPI(ctx context.Context) error
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

func (d Docker) Fetch(ctx context.Context) error { return d.Compose.PullImage(ctx, d.Version()) }

func (d Docker) Start(ctx context.Context) error { return d.Compose.Up(ctx) }

func (d Docker) StopAPI(ctx context.Context) error { return d.Compose.Stop(ctx, "hindsight") }

func (d Docker) Database() Database { return DB{Compose: d.Compose} }

func (d Docker) VerifyDump(ctx context.Context, dump string, tables []string) (map[string]int64, error) {
	return RestoreCounts(ctx, d.Compose.Runner, dump, tables)
}
