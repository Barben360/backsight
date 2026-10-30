package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
)

// Runtime runs the local Hindsight server (decision 0070). Docker Compose
// is one implementation; the native one (Hindsight from PyPI on pg0's
// Postgres, under systemd user units, decision 0078) comes behind the same
// interface. Every command that installs, starts, stops, inspects or
// deletes the server, or reaches its database or its files, goes through
// it.
type Runtime interface {
	// Kind names the runtime ("docker").
	Kind() string
	// Version is the server version Render deploys: for Docker, the
	// Hindsight image reference (the binary's pin, or config's digest),
	// unless WithVersion set another.
	Version() string
	// HindsightVersion is the Hindsight release Version runs ("1.1.0"),
	// what runtimes compare across; "" when Version names none (an image
	// by digest alone).
	HindsightVersion() string
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
	// StartDatabase starts the database alone and waits until it runs, the
	// API not started (a restore into a server switched to).
	StartDatabase(ctx context.Context) error
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
	// RenameBank runs Hindsight's own rename (hindsight-admin rename-bank)
	// with the server's own installation; dryRun rolls it back.
	RenameBank(ctx context.Context, from, to string, dryRun bool) ([]byte, error)
	// SecretFiles are the files Write installs that hold the server's keys:
	// what report and redaction scrub.
	SecretFiles() []string
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
	// PendingOperations counts the operations of banks the server has not
	// finished: a bank rename waits for them.
	PendingOperations(ctx context.Context, banks []string) (int, error)
}

// Pruner is a runtime that keeps several versions installed side by side
// (native: one virtual environment per version) and can remove all but
// keep.
type Pruner interface {
	PruneVersions(keep ...string) error
}

// Sizer is a runtime that can tell how much disk its server's data takes.
type Sizer interface {
	DataSize(ctx context.Context) (int64, error)
}

// Cleaner is what server runtime clean shows of a runtime switched from
// (decision 0078, ruling R43): everything its Delete removes, and how
// much disk that is.
type Cleaner interface {
	// Leftover reports whether anything of the runtime is on this machine.
	Leftover(ctx context.Context) (bool, error)
	// CleanWhat names everything Delete removes.
	CleanWhat() string
	// CleanSize is the disk all of it takes.
	CleanSize(ctx context.Context) (int64, error)
}

// Creator is a runtime that creates its database at its first start only
// when told (native, ruling R35): an install or a switch to it.
type Creator interface {
	AllowCreate() Runtime
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

// DownloadMB is about how much images (a native runtime's items) and
// models download.
func DownloadMB(images []string, models bool) int {
	mb := 0
	for _, im := range images {
		switch im {
		case PostgresImage:
			mb += PostgresImageMB
		case NativeUV:
			mb += NativeUVMB
		case NativePython:
			mb += NativePythonMB
		case NativeWheels:
			mb += NativeWheelsMB
		case NativeModels:
			mb += ModelsMB
		default:
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

// PullAttempts and PullBackoff: a pull that fails is tried again, since
// Docker keeps the layers it already has and a second pull often passes
// (a busy containerd mount, a dropped connection: issue 60).
var (
	PullAttempts = 3
	PullBackoff  = 3 * time.Second
)

// PullError is an image pull that failed every attempt, with Docker's
// last error line.
type PullError struct {
	Image    string
	Last     string
	Attempts int
	Err      error
}

func (e *PullError) Error() string {
	return fmt.Sprintf("docker pull %s: %v: %s", e.Image, e.Err, e.Last)
}

func (e *PullError) Unwrap() error { return e.Err }

// Pull failure causes, from Docker's last line.
const (
	PullNetwork = "network" // DNS, TLS, timeouts, refused or reset connections, registry errors
	PullUnpack  = "unpack"  // the layers arrived, containerd could not unpack them locally
	PullDisk    = "disk"    // no space left
)

var (
	unpackRE  = regexp.MustCompile(`(?i)failed to extract layer|failed to unmount|device or resource busy|failed to register layer|error creating overlay mount`)
	diskRE    = regexp.MustCompile(`(?i)no space left on device`)
	networkRE = regexp.MustCompile(`(?i)no such host|lookup .*: |dial tcp|i/o timeout|timeout|tls|x509|connection (refused|reset)|network is unreachable|EOF|toomanyrequests|50[0-9] |unexpected status|too many requests|bad gateway|service unavailable`)
)

// Cause classifies the failure; "" when Docker's line names none of them.
func (e *PullError) Cause() string {
	switch {
	case unpackRE.MatchString(e.Last):
		return PullUnpack
	case diskRE.MatchString(e.Last):
		return PullDisk
	case networkRE.MatchString(e.Last):
		return PullNetwork
	}
	return ""
}

// Download pulls each image outside the project (nothing that runs
// changes), Docker's progress going to w: given as is, a terminal gets
// Docker's progress bars. Each pull is tried PullAttempts times; a failure
// is a *PullError carrying Docker's last error line, which w may not show.
func (d Docker) Download(ctx context.Context, images []string, w io.Writer) error {
	for _, im := range images {
		var err error
		last := &lastLine{}
		for attempt := 1; attempt <= PullAttempts; attempt++ {
			last = &lastLine{}
			if st, ok := d.Compose.Runner.(Streamer); ok {
				err = st.Stream(ctx, w, io.MultiWriter(w, last), "pull", im)
			} else {
				var out []byte
				out, err = d.Compose.Runner.Run(ctx, "pull", im)
				_, _ = w.Write(out)
				_, _ = last.Write(out)
			}
			if err == nil || ctx.Err() != nil || attempt == PullAttempts {
				break
			}
			fmt.Fprintf(w, "docker pull %s failed (%s): trying again (%d/%d)\n", im, last.String(), attempt+1, PullAttempts)
			select {
			case <-ctx.Done():
			case <-time.After(PullBackoff):
			}
		}
		if err != nil {
			return &PullError{Image: im, Last: last.String(), Attempts: PullAttempts, Err: err}
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

// HindsightVersion is the tag of the image Version names.
func (d Docker) HindsightVersion() string { return HindsightVersionOf(d.Version()) }

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

// StartDatabase is compose up of the db service alone.
func (d Docker) StartDatabase(ctx context.Context) error { return d.Compose.Up(ctx, "db") }

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
// does the override (decision 0067 block 3 review). Without compose.yaml
// (a switch's leftover volumes, which Leftover reports), Compose cannot
// run: the project's containers go by their compose label, and the
// volumes backsight's compose.yaml declares by their names.
func (d Docker) Delete(ctx context.Context) error {
	raw, err := os.ReadFile(filepath.Join(d.Compose.Dir, "compose.yaml"))
	noFile := errors.Is(err, fs.ErrNotExist)
	if err != nil && !noFile {
		return err
	}
	// The data goes whatever the file says.
	volumes := []string{dataVolume}
	declared := DeclaredVolumes(raw)
	if noFile {
		declared = []string{modelsVolume}
	}
	for _, v := range declared {
		if !slices.Contains(volumes, v) {
			volumes = append(volumes, v)
		}
	}
	if noFile {
		if err := d.removeContainers(ctx); err != nil {
			return err
		}
	} else {
		if _, err := d.Compose.run(ctx, "rm", "--stop", "--force", "-v"); err != nil {
			return err
		}
		if _, err := d.Compose.run(ctx, "down", "--remove-orphans"); err != nil {
			return err
		}
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

// removeContainers removes the project's containers, found by the label
// Compose gives them, with their anonymous volumes: Delete's way without
// compose.yaml.
func (d Docker) removeContainers(ctx context.Context) error {
	args := []string{"ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project=" + d.Compose.project()}
	var out []byte
	var err error
	if r, ok := d.Compose.Runner.(StdoutRunner); ok { // a warning on stderr is no container id
		out, err = r.RunStdout(ctx, args...)
	} else {
		out, err = d.Compose.Runner.Run(ctx, args...)
	}
	if err != nil {
		return fmt.Errorf("docker ps: %w: %s", err, strings.TrimSpace(string(out)))
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	if out, err := d.Compose.Runner.Run(ctx, append([]string{"rm", "--force", "--volumes"}, ids...)...); err != nil {
		return fmt.Errorf("docker rm: %w: %s", err, strings.TrimSpace(string(out)))
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

// RenameBank runs the rename in the hindsight container.
func (d Docker) RenameBank(ctx context.Context, from, to string, dryRun bool) ([]byte, error) {
	return d.Compose.RenameBank(ctx, from, to, dryRun)
}

// SecretFiles is the project's .env.
func (d Docker) SecretFiles() []string { return []string{filepath.Join(d.Compose.Dir, ".env")} }

var _ Cleaner = Docker{}

// Leftover: compose.yaml, or the database volume. A machine without
// Docker has none of it.
func (d Docker) Leftover(ctx context.Context) (bool, error) {
	if fileExists(filepath.Join(d.Compose.Dir, "compose.yaml")) {
		return true, nil
	}
	ok, err := d.DataExists(ctx)
	if errors.Is(err, exec.ErrNotFound) {
		return false, nil
	}
	return ok, err
}

// CleanWhat names what Delete removes: both volumes, the containers and
// backsight's compose files (a user's override stays).
func (d Docker) CleanWhat() string {
	p := d.Compose.project()
	return "the Docker server of project " + p + ": the database volume " + p + "_" + dataVolume + ", the model cache " + p + "_" + modelsVolume +
		", the containers, and compose.yaml and .env in " + d.Compose.Dir
}

// CleanSize is both volumes' size.
func (d Docker) CleanSize(ctx context.Context) (int64, error) { return d.DataSize(ctx) }

var _ Sizer = Docker{}

// DataSize adds up the sizes docker system df reports for the project's
// two volumes (the database, the embedding models); a volume not created
// yet holds nothing.
func (d Docker) DataSize(ctx context.Context) (int64, error) {
	args := []string{"system", "df", "-v", "--format", "{{json .Volumes}}"}
	var out []byte
	var err error
	if r, ok := d.Compose.Runner.(StdoutRunner); ok { // a warning on stderr is no JSON
		out, err = r.RunStdout(ctx, args...)
	} else {
		out, err = d.Compose.Runner.Run(ctx, args...)
	}
	if err != nil {
		return 0, fmt.Errorf("docker system df: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var vols []struct{ Name, Size string }
	if err := json.Unmarshal(bytes.TrimSpace(out), &vols); err != nil {
		return 0, fmt.Errorf("docker system df: %w: %q", err, out)
	}
	ours := []string{d.Compose.project() + "_" + dataVolume, d.Compose.project() + "_" + modelsVolume}
	var total int64
	for _, v := range vols {
		if !slices.Contains(ours, v.Name) {
			continue
		}
		n, ok := decimalSize(v.Size)
		if !ok {
			return 0, fmt.Errorf("docker cannot tell the size of volume %s (%q)", v.Name, v.Size)
		}
		total += n
	}
	return total, nil
}

// dockerSize is a size as Docker prints it: decimal units, 4 significant
// digits ("48.25MB", "3.057GB", "0B").
var dockerSize = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([kKMGTPE]?)B$`)

// decimalSize reads a size Docker printed, in bytes.
func decimalSize(s string) (int64, bool) {
	m := dockerSize.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	exp := 0 // bytes
	if m[2] != "" {
		exp = strings.Index("kMGTPE", strings.ReplaceAll(m[2], "K", "k")) + 1
	}
	return int64(math.Round(f * math.Pow(1000, float64(exp)))), true
}

func (d Docker) VerifyDump(ctx context.Context, dump string, tables []string) (map[string]int64, error) {
	return RestoreCounts(ctx, d.Compose.Runner, dump, tables)
}
