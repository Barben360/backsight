package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// ImagesRemoveCmd is how a user frees the disk space of what the
	// server ran (for Docker, its images), once it is deleted.
	ImagesRemoveCmd() string
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
	BankIDs(ctx context.Context) ([]string, error)
}

// Downloader is a runtime that can say what its first start still has
// to download and show the download as it goes (setup's server step).
type Downloader interface {
	// Downloads lists the images not on this machine yet, and whether the
	// embedding models are still to download (at the server's first
	// start, inside it).
	Downloads(ctx context.Context) (images []string, models bool, err error)
	// Download fetches images, the progress going to w.
	Download(ctx context.Context, images []string, w io.Writer) error
}

// Approximate download sizes, in MB, for the messages that announce them.
const (
	HindsightImageMB = 1000
	PostgresImageMB  = 160
	ModelsMB         = 1200
)

// DownloadMB is about how much images and models download.
func DownloadMB(images []string, models bool) int {
	mb := 0
	for _, im := range images {
		if im == PostgresImage {
			mb += PostgresImageMB
		} else {
			mb += HindsightImageMB
		}
	}
	if models {
		mb += ModelsMB
	}
	return mb
}

var _ Downloader = Docker{}

// Downloads asks Docker for each image the server runs and for the model
// cache volume.
func (d Docker) Downloads(ctx context.Context) ([]string, bool, error) {
	var missing []string
	for _, im := range []string{d.Version(), PostgresImage} {
		out, err := d.Compose.Runner.Run(ctx, "image", "inspect", "--format", "{{.Id}}", im)
		switch {
		case err == nil:
		case strings.Contains(strings.ToLower(string(out)), "no such image"):
			missing = append(missing, im)
		default:
			return nil, false, fmt.Errorf("docker image inspect %s: %w: %s", im, err, strings.TrimSpace(string(out)))
		}
	}
	vol := d.Compose.project() + "_" + modelsVolume
	out, err := d.Compose.Runner.Run(ctx, "volume", "inspect", vol)
	switch {
	case err == nil:
		return missing, false, nil
	case strings.Contains(strings.ToLower(string(out)), "no such volume"):
		return missing, true, nil
	}
	return nil, false, fmt.Errorf("docker volume inspect %s: %w: %s", vol, err, strings.TrimSpace(string(out)))
}

// Download pulls each image outside the project (nothing that runs
// changes), Docker's progress going to w: given as is, a terminal gets
// Docker's progress bars. A failure carries Docker's last error line,
// which w may not show.
func (d Docker) Download(ctx context.Context, images []string, w io.Writer) error {
	for _, im := range images {
		var err error
		last := &lastLine{}
		if st, ok := d.Compose.Runner.(Streamer); ok {
			err = st.Stream(ctx, w, io.MultiWriter(w, last), "pull", im)
		} else {
			var out []byte
			out, err = d.Compose.Runner.Run(ctx, "pull", im)
			_, _ = w.Write(out)
			_, _ = last.Write(out)
		}
		if err != nil {
			return fmt.Errorf("docker pull %s: %w: %s", im, err, last.String())
		}
	}
	return nil
}

// lastLine keeps the last non-empty line written to it.
type lastLine struct{ buf []byte }

func (l *lastLine) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	if len(l.buf) > 4096 {
		l.buf = l.buf[len(l.buf)-4096:]
	}
	return len(p), nil
}

func (l *lastLine) String() string {
	lines := strings.FieldsFunc(string(l.buf), func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
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

// Delete removes the containers with their anonymous volumes (the
// database image declares one at /var/lib/postgresql), runs compose down
// without the named volumes, removes each volume backsight's compose.yaml
// declares (the database, the model cache), then compose.yaml and .env.
// A volume only the user's compose.override.yaml declares stays, and so
// does the override (decision 0067 block 3 review).
func (d Docker) Delete(ctx context.Context) error {
	raw, err := os.ReadFile(filepath.Join(d.Compose.Dir, "compose.yaml"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// The data goes whatever the file says.
	volumes := []string{dataVolume}
	for _, v := range DeclaredVolumes(raw) {
		if !slices.Contains(volumes, v) {
			volumes = append(volumes, v)
		}
	}
	if _, err := d.Compose.run(ctx, "rm", "--stop", "--force", "-v"); err != nil {
		return err
	}
	if _, err := d.Compose.run(ctx, "down", "--remove-orphans"); err != nil {
		return err
	}
	for _, v := range volumes {
		if err := d.Compose.RemoveVolume(ctx, v); err != nil {
			return err
		}
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

// ImagesRemoveCmd names the images by digest: pulled by digest, they
// carry no tag ("<none>" in docker image ls). The Hindsight image is the
// one deployed, else the one this runtime deploys.
func (d Docker) ImagesRemoveCmd() string {
	hs, err := d.Deployed()
	if err != nil || hs == "" {
		hs = d.Version()
	}
	return "docker image rm " + byDigest(hs) + " " + byDigest(PostgresImage)
}

// byDigest drops the tag of repo:tag@sha256:… (Docker accepts either, not
// both, to remove an untagged image).
func byDigest(ref string) string {
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ref
	}
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo + "@" + digest
}

// Engine asks the Docker daemon for its version and security options.
func (d Docker) Engine(ctx context.Context) (Engine, error) {
	out, err := d.Compose.Runner.Run(ctx, "info", "--format", "{{.ServerVersion}} {{range .SecurityOptions}}{{.}} {{end}}")
	if err != nil {
		if text := strings.TrimSpace(string(out)); text != "" {
			err = fmt.Errorf("%w: %s", err, text)
		}
		return Engine{}, err
	}
	// Docker CLI 26 exits 0 when it cannot reach the daemon and prints the
	// error in place of the version: only a version is one.
	f := strings.Fields(string(out))
	if len(f) == 0 || !engineVersion.MatchString(f[0]) {
		text := strings.TrimSpace(string(out))
		if text == "" {
			text = "docker info printed no version"
		}
		return Engine{}, fmt.Errorf("docker info: %s", text)
	}
	return Engine{Version: f[0], Rootless: strings.Contains(string(out), "rootless")}, nil
}

// engineVersion is how a Docker server version starts (26.1.5, 28.1.1-rd).
var engineVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+`)

// Orchestrator is `docker compose version --short`.
func (d Docker) Orchestrator(ctx context.Context) (string, error) {
	out, err := d.Compose.Runner.Run(ctx, "compose", "version", "--short")
	return strings.TrimSpace(string(out)), err
}

func (d Docker) Database() Database { return DB{Compose: d.Compose} }

func (d Docker) VerifyDump(ctx context.Context, dump string, tables []string) (map[string]int64, error) {
	return RestoreCounts(ctx, d.Compose.Runner, dump, tables)
}
