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
	// Dump writes a custom-format pg_dump to dst and checks it lists.
	Dump(ctx context.Context, dst string) error
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
// sidecar into dir. m carries the fields the caller knows (versions,
// images, bank, recipients); Create fills the rest. extras are the files
// stored beside the dump (config, secrets, state…), by archive name.
func Create(ctx context.Context, db DB, dir string, m Manifest, extras map[string][]byte, recipients []age.Recipient) (string, Manifest, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", m, err
	}
	work, err := os.MkdirTemp(dir, ".work-")
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
	m.Counts = map[string]int64{}
	for _, t := range Tables {
		v, err := db.Query(ctx, "select count(*) from "+t)
		if err != nil {
			return "", m, fmt.Errorf("counting %s: %w", t, err)
		}
		var n int64
		if _, err := fmt.Sscan(v, &n); err != nil {
			return "", m, fmt.Errorf("counting %s: %q", t, v)
		}
		m.Counts[t] = n
	}
	dump := filepath.Join(work, "db.dump")
	if err := db.Dump(ctx, dump); err != nil {
		return "", m, fmt.Errorf("dumping the database: %w", err)
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

	final := filepath.Join(dir, Name(m.CreatedAt))
	tmp := filepath.Join(work, "archive")
	if err := writeArchive(tmp, recipients, man, dump, names, extras); err != nil {
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

func writeArchive(path string, recipients []age.Recipient, manifest []byte, dump string, names []string, extras map[string][]byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	enc, err := age.Encrypt(f, recipients...)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(enc)
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

// Open decrypts the archive at path into dir and checks its dump against
// the manifest; it returns the manifest.
func Open(path string, identities []age.Identity, dir string) (Manifest, error) {
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
		if name != h.Name || name == "." || name == ".." {
			return m, fmt.Errorf("unexpected entry %q", h.Name)
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

// Prune deletes the backups (and sidecars) beyond the newest of each of the
// last keepDaily days and of each of the last keepWeekly ISO weeks
// (decision 0023: 7 daily + 4 weekly). It returns the deleted paths.
func Prune(dir string, keepDaily, keepWeekly int) ([]string, error) {
	list, err := List(dir)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	days, weeks := map[string]bool{}, map[string]bool{}
	for _, e := range list { // newest first
		t := e.Manifest.CreatedAt.UTC()
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
		if keep[e.Path] {
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
// reach.
func Before(dir string, t time.Time) []Entry {
	list, _ := List(dir)
	var out []Entry
	for _, e := range list {
		if e.Manifest.CreatedAt.Before(t) {
			out = append(out, e)
		}
	}
	return out
}
