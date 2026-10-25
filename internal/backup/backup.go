// Package backup writes and reads backsight backups (SPEC §11.8, decision
// 0023): a pg_dump of the server's database with the files needed to
// rebuild the install, in one tar.gz, always age-encrypted. A sidecar
// <archive>.json carries the manifest (no secret) so that listing needs no
// key.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/agessh"

	"github.com/Barben360/backsight/internal/state"
)

// Format of the archive layout.
const Format = 1

// Manifest describes one backup.
type Manifest struct {
	Format         int              `json:"format"`
	CreatedAt      time.Time        `json:"created_at"`
	BinaryVersion  string           `json:"binary_version"`
	HindsightImage string           `json:"hindsight_image"`
	PostgresImage  string           `json:"postgres_image"`
	Alembic        string           `json:"alembic_revision"`
	PGVector       string           `json:"pgvector"`
	Embeddings     string           `json:"embeddings_preset"`
	Bank           string           `json:"bank"`
	Counts         map[string]int64 `json:"counts"` // table → rows
	DumpSHA256     string           `json:"dump_sha256"`
	DumpBytes      int64            `json:"dump_bytes"`
	Recipients     []string         `json:"recipients"` // public: who can decrypt
	Files          []string         `json:"files"`
}

// Tables are counted at backup time and compared by verify.
var Tables = []string{"memory_units", "documents", "mental_models"}

// DB is what a backup needs from the database.
type DB interface {
	// Query runs one SQL statement and returns its single value.
	Query(ctx context.Context, sql string) (string, error)
	// Dump writes a custom-format pg_dump to dst, checks it lists, and
	// returns the rows of tables counted in the dump itself.
	Dump(ctx context.Context, dst string, tables []string) (map[string]int64, error)
}

const prefix, suffix = "backsight-", ".tar.gz.age"

// Name is the archive file name for a backup taken at t.
func Name(t time.Time) string { return prefix + t.UTC().Format("20060102T150405Z") + suffix }

// Recipients parses age (age1…) and SSH (ssh-ed25519, ssh-rsa) recipients.
func Recipients(lines []string) ([]age.Recipient, error) {
	var out []age.Recipient
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		var r age.Recipient
		var err error
		if strings.HasPrefix(l, "ssh-") {
			r, err = agessh.ParseRecipient(l)
		} else {
			r, err = age.ParseX25519Recipient(l)
		}
		if err != nil {
			return nil, fmt.Errorf("backup recipient %q: %w", l, err)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errors.New("no backup recipient")
	}
	return out, nil
}

// Create dumps the database and writes the encrypted archive and its
// sidecar into dir. The clear dump is staged under staging (the state
// directory, 0700), never in dir: dir may be synced elsewhere, and only
// ciphertext lands there. afterDump runs once the database work is done,
// with the clear dump's path: the caller releases the maintenance lock
// (encryption needs no lock), or keeps a copy of the dump to roll back to
// (restore); its error fails the backup. m carries the fields the caller
// knows (versions, images, bank, recipients); Create fills the rest.
// extras are the files stored beside the dump.
func Create(ctx context.Context, db DB, dir, staging string, m Manifest, extras map[string][]byte, recipients []age.Recipient, afterDump func(dump string) error) (string, Manifest, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", m, err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", m, err
	}
	removeStale(dir, ".work-")      // left by older versions, which staged in dir
	removeStale(staging, "backup-") // left by a crash
	removeDeadVerifies(staging)
	work, err := os.MkdirTemp(staging, "backup-")
	if err != nil {
		return "", m, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	m.Format = Format
	if m.Alembic, err = db.Query(ctx, "select version_num from alembic_version"); err != nil {
		return "", m, fmt.Errorf("reading the schema revision: %w", err)
	}
	if m.PGVector, err = db.Query(ctx, "select extversion from pg_extension where extname = 'vector'"); err != nil {
		return "", m, fmt.Errorf("reading pgvector's version: %w", err)
	}
	dump := filepath.Join(work, "db.dump")
	if m.Counts, err = db.Dump(ctx, dump, Tables); err != nil {
		return "", m, fmt.Errorf("dumping the database: %w", err)
	}
	// The archive exists before the lock is released: from then on a purge
	// sees it (Before), though it holds the data as dumped.
	final := filepath.Join(dir, Name(m.CreatedAt))
	tmp := final + ".partial" // ciphertext only, renamed when complete
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", m, err
	}
	defer func() { _ = out.Close(); _ = os.Remove(tmp) }()
	if afterDump != nil {
		if err := afterDump(dump); err != nil {
			return "", m, err
		}
	}
	if m.DumpSHA256, m.DumpBytes, err = fileSum(dump); err != nil {
		return "", m, err
	}
	names := make([]string, 0, len(extras))
	for n := range extras {
		names = append(names, n)
	}
	sort.Strings(names)
	m.Files = append([]string{"manifest.json", "db.dump"}, names...)
	man, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", m, err
	}

	if err := writeArchive(out, recipients, man, dump, names, extras); err != nil {
		return "", m, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", m, err
	}
	if err := os.WriteFile(final+".json", man, 0o600); err != nil {
		return final, m, err
	}
	return final, m, nil
}

// removeStale removes entries of dir starting with prefix: work left by a
// crash.
func removeStale(dir, prefix string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix) && e.IsDir() {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

const verifyPrefix = "verify-"

// VerifyDir makes the work directory of a verify under staging (0700), to
// remove with done. Verify holds no maintenance lock, so each holds its own
// lock beside its directory, taken first: a directory with no live lock is
// a killed verify's, clear dump included, and removed here and by Create.
func VerifyDir(staging string) (string, func(), error) {
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", nil, err
	}
	removeDeadVerifies(staging)
	work := filepath.Join(staging, fmt.Sprintf("%s%d-%d", verifyPrefix, os.Getpid(), time.Now().UnixNano()))
	l, err := state.TryAcquire(work+".lock", "backup verify")
	if err != nil {
		return "", nil, err
	}
	if err := os.Mkdir(work, 0o700); err != nil {
		_ = l.Release()
		return "", nil, err
	}
	return work, func() { _ = os.RemoveAll(work); _ = l.Release() }, nil
}

// removeDeadVerifies removes the verify directories under staging whose
// lock is gone or stale, and those locks.
func removeDeadVerifies(staging string) {
	ents, _ := os.ReadDir(staging)
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), verifyPrefix) {
			continue
		}
		work := filepath.Join(staging, strings.TrimSuffix(e.Name(), ".lock"))
		held := state.Inspect(work + ".lock")
		if held != nil && !held.Stale {
			continue // running
		}
		_ = os.RemoveAll(work)
		if held != nil {
			_ = state.BreakStale(held)
		}
	}
}

func fileSum(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func writeArchive(f *os.File, recipients []age.Recipient, manifest []byte, dump string, names []string, extras map[string][]byte) (err error) {
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	enc, err := age.Encrypt(f, recipients...)
	if err != nil {
		return err
	}
	gz, _ := gzip.NewWriterLevel(enc, gzip.BestSpeed) // a -Fc dump is compressed already
	tw := tar.NewWriter(gz)
	add := func(name string, size int64, r io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := io.Copy(tw, r)
		return err
	}
	if err := add("manifest.json", int64(len(manifest)), strings.NewReader(string(manifest))); err != nil {
		return err
	}
	df, err := os.Open(dump)
	if err != nil {
		return err
	}
	fi, _ := df.Stat()
	err = add("db.dump", fi.Size(), df)
	_ = df.Close()
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := add(n, int64(len(extras[n])), strings.NewReader(string(extras[n]))); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// Open decrypts the archive at path into dir, keeping the entries want
// accepts (nil: all; the manifest and the dump always), checks the format
// and the dump against the manifest, and returns the manifest. An archive
// is only as trusted as whoever holds the public recipient: age gives
// secrecy, not origin (restore, M5, treats its config as untrusted).
func Open(path string, identities []age.Identity, dir string, want func(name string) bool) (Manifest, error) {
	var m Manifest
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer func() { _ = f.Close() }()
	dec, err := age.Decrypt(f, identities...)
	if err != nil {
		return m, fmt.Errorf("decrypting %s: %w (the backup identity is in secrets.toml, or import it with backsight backup identity import)", filepath.Base(path), err)
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		return m, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, err
		}
		name := filepath.Base(h.Name) // flat archive: no paths
		if name != h.Name || name == "." || name == ".." || h.Typeflag != tar.TypeReg {
			return m, fmt.Errorf("unexpected entry %q", h.Name)
		}
		if name != "manifest.json" && name != "db.dump" && want != nil && !want(name) {
			continue // read through, not written
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return m, err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return m, err
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, fmt.Errorf("no manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.Format != Format {
		return m, fmt.Errorf("archive format %d: this binary reads format %d", m.Format, Format)
	}
	sum, _, err := fileSum(filepath.Join(dir, "db.dump"))
	if err != nil {
		return m, err
	}
	if sum != m.DumpSHA256 {
		return m, fmt.Errorf("the dump does not match its manifest (sha256 %s, want %s)", sum, m.DumpSHA256)
	}
	return m, nil
}

// Entry is one backup found on disk.
type Entry struct {
	Path     string   `json:"path"`
	Bytes    int64    `json:"bytes"`
	Manifest Manifest `json:"manifest"`
	// Ours: written here, with its sidecar. Prune touches only these; a
	// copy brought from elsewhere is the user's.
	Ours bool `json:"-"`
}

// List reads the backups in dir from their sidecars, newest first.
func List(dir string) ([]Entry, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		en := Entry{Path: p, Bytes: fi.Size()}
		if raw, err := os.ReadFile(p + ".json"); err == nil {
			_ = json.Unmarshal(raw, &en.Manifest)
		}
		en.Ours = en.Manifest.Format > 0
		if en.Manifest.CreatedAt.IsZero() {
			if t, err := time.Parse("20060102T150405Z", strings.TrimSuffix(strings.TrimPrefix(e.Name(), prefix), suffix)); err == nil {
				en.Manifest.CreatedAt = t
			}
		}
		out = append(out, en)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.CreatedAt.After(out[j].Manifest.CreatedAt) })
	return out, nil
}

// MinAge is how long an archive is kept whatever the retention: one taken
// before a destructive step (forget, restore, upgrade) must outlive the
// next backup of the same day.
const MinAge = 24 * time.Hour

// Prune deletes the backups (and sidecars) beyond the newest of each of the
// last keepDaily days and of each of the last keepWeekly ISO weeks
// (decision 0023: 7 daily + 4 weekly). An archive younger than MinAge at
// now is never deleted. It returns the deleted paths.
func Prune(dir string, now time.Time, keepDaily, keepWeekly int) ([]string, error) {
	list, err := List(dir)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	days, weeks := map[string]bool{}, map[string]bool{}
	for _, e := range list { // newest first
		if !e.Ours {
			continue
		}
		t := e.Manifest.CreatedAt.UTC()
		if now.Sub(t) < MinAge {
			keep[e.Path] = true
		}
		day := t.Format("2006-01-02")
		y, w := t.ISOWeek()
		week := fmt.Sprintf("%d-%02d", y, w)
		if !days[day] && len(days) < keepDaily {
			days[day], keep[e.Path] = true, true
		}
		if !weeks[week] && len(weeks) < keepWeekly {
			weeks[week], keep[e.Path] = true, true
		}
	}
	var removed []string
	for _, e := range list {
		if keep[e.Path] || !e.Ours {
			continue
		}
		if err := os.Remove(e.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		_ = os.Remove(e.Path + ".json")
		removed = append(removed, e.Path)
	}
	return removed, nil
}

// Before lists the backups taken before t: the ones a purge at t cannot
// reach, those still being encrypted included (by the path they will
// have). The archives in progress are read first: one renamed meanwhile is
// then found by List.
func Before(dir string, t time.Time) []Entry {
	var partial []Entry
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), suffix+".partial")
		if !ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		if at, err := time.Parse("20060102T150405Z", strings.TrimPrefix(name, prefix)); err == nil {
			partial = append(partial, Entry{Path: filepath.Join(dir, name+suffix), Manifest: Manifest{CreatedAt: at}})
		}
	}
	list, _ := List(dir)
	var out []Entry
	seen := map[string]bool{}
	for _, e := range append(list, partial...) {
		if e.Manifest.CreatedAt.Before(t) && !seen[e.Path] {
			seen[e.Path] = true
			out = append(out, e)
		}
	}
	return out
}
