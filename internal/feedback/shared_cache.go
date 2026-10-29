package feedback

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// SharedCache is <state>/feedback/shared.json: the other machines'
// shared documents as jobs tick last fetched them from the server, each
// the metadata value its machine wrote (ParseSharedDoc reads it), and
// when. Only the tick writes it; the hot path reads it, never the server.
type SharedCache struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Docs      []json.RawMessage `json:"docs"`
}

// SharedCacheFile is where dir (the feedback directory) keeps the cache.
func SharedCacheFile(dir string) string { return filepath.Join(dir, "shared.json") }

// LoadSharedCache reads dir's cache: an error when it is missing
// (fs.ErrNotExist) or does not parse.
func LoadSharedCache(dir string) (SharedCache, error) {
	var c SharedCache
	raw, err := os.ReadFile(SharedCacheFile(dir))
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return SharedCache{}, err
	}
	return c, nil
}

// SaveSharedCache replaces dir's cache with c in one rename: a reader
// sees the old file or the new one, never a part.
func SaveSharedCache(dir string, c SharedCache) error {
	if c.Docs == nil {
		c.Docs = []json.RawMessage{}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(SharedCacheFile(dir), raw, 0o600)
}

// Parsed is c's documents that ParseSharedDoc reads, and how many it
// does not (corrupt, or of another version).
func (c SharedCache) Parsed() (docs []SharedDoc, skipped int) {
	for _, raw := range c.Docs {
		d, err := ParseSharedDoc(string(raw))
		if err != nil {
			skipped++
			continue
		}
		docs = append(docs, d)
	}
	return docs, skipped
}

// SharedMachines is how many machines docs speak for at now, self left
// out: those CombineRemote counts.
func SharedMachines(docs []SharedDoc, self string, now time.Time) int {
	seen := map[string]bool{}
	for _, d := range docs {
		if d.Machine != self && d.current(now) {
			seen[d.Machine] = true
		}
	}
	return len(seen)
}
