package machines

import (
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// ErrCacheTooLarge is returned by LoadCache for a file over MaxCacheBytes, refused from its size.
var ErrCacheTooLarge = errors.New("machines: cache file over its cap")

// CachedDoc is a metadata value as fetched, under its document id.
type CachedDoc struct {
	Name string `json:"name"`
	Raw  string `json:"raw"`
}

// Ignored is a fetched document that did not parse; Machine is the short id or empty.
type Ignored struct {
	Machine string `json:"machine"`
	Reason  Reason `json:"reason"`
}

// Cache is the content of machines.json.
type Cache struct {
	FetchedAt time.Time   `json:"fetched_at"`
	Docs      []CachedDoc `json:"docs"`
	Ignored   []Ignored   `json:"ignored"`
}

// CacheFile is the path of the cache in metricsDir.
func CacheFile(metricsDir string) string { return filepath.Join(metricsDir, "machines.json") }

// LoadCache reads the cache. A missing file is fs.ErrNotExist; a file over
// MaxCacheBytes is refused from its size without being read.
func LoadCache(metricsDir string) (Cache, error) {
	f, err := fsutil.OpenShared(CacheFile(metricsDir))
	if err != nil {
		return Cache{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return Cache{}, err
	}
	if fi.Size() > MaxCacheBytes {
		return Cache{}, ErrCacheTooLarge
	}
	b, err := readCapped(f, MaxCacheBytes)
	if err != nil {
		return Cache{}, err
	}
	var c Cache
	if err := json.Unmarshal(b, &c); err != nil {
		return Cache{}, err
	}
	return c, nil
}

// readCapped reads r to its end, refusing more than max bytes (a file that
// grew after its size was checked).
func readCapped(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrCacheTooLarge
	}
	return b, nil
}

// SaveCache writes the cache atomically (0600).
func SaveCache(metricsDir string, c Cache) error {
	c.FetchedAt = c.FetchedAt.UTC()
	if c.Docs == nil {
		c.Docs = []CachedDoc{}
	}
	if c.Ignored == nil {
		c.Ignored = []Ignored{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(CacheFile(metricsDir), b, 0o600)
}

// Published is the publish record (published.json), written by the publish step only.
type Published struct {
	At          time.Time `json:"at,omitzero"`
	Server      string    `json:"server,omitempty"`
	Until       string    `json:"until,omitempty"`
	FirstSeen   string    `json:"first_seen,omitempty"`
	Bytes       int       `json:"bytes"`
	FailedAt    time.Time `json:"failed_at,omitzero"`
	FailedSince time.Time `json:"failed_since,omitzero"`
	Failure     string    `json:"failure,omitempty"`
}

// PublishedFile is the path of the publish record in metricsDir.
func PublishedFile(metricsDir string) string { return filepath.Join(metricsDir, "published.json") }

// LoadPublished reads the record; a missing or corrupt file reads as none.
func LoadPublished(metricsDir string) (Published, bool) {
	b, err := fsutil.ReadFileShared(PublishedFile(metricsDir))
	if err != nil {
		return Published{}, false
	}
	var p Published
	if err := json.Unmarshal(b, &p); err != nil {
		return Published{}, false
	}
	return p, true
}

// SavePublished writes the record atomically (0600), times in UTC.
func SavePublished(metricsDir string, p Published) error {
	p.At, p.FailedAt, p.FailedSince = p.At.UTC(), p.FailedAt.UTC(), p.FailedSince.UTC()
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(PublishedFile(metricsDir), b, 0o600)
}
