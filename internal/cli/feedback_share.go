package cli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
)

// A machine's shared ledger is the control bank document
// sharedDocPrefix+<machine id>, its counts under the metadata key
// sharedMetaKey and nothing else there. It is sent again when it changed
// and the last send is shareUploadEvery old, at once when the set of
// hidden memories changed; the other machines' are fetched every
// shareRefreshEvery.
const (
	sharedDocPrefix    = "backsight:feedback:"
	sharedMetaKey      = "feedback"
	shareUploadEvery   = time.Hour
	shareRefreshEvery  = 6 * time.Hour
	shareUploadRecName = "shared-upload.json"
)

// shareTickBudget bounds the tick's sharing step, send and fetch.
var shareTickBudget = 10 * time.Second

// shareUpload is <state>/feedback/shared-upload.json: when this machine
// last sent its document, the hash of its entries (its time left out, so
// that the same counts hash the same) and of its hidden ids.
type shareUpload struct {
	At         time.Time `json:"at"`
	Hash       string    `json:"hash"`
	HiddenHash string    `json:"hidden_hash"`
}

// shareLedger is the tick's sharing step, with feedback and share_ledger
// on (the tick checks first that its server answers and is the one this
// PC joined): this machine's document is sent when the upload rule says
// so, and the other machines' are fetched into shared.json when the cache
// is 6 hours old, missing or unreadable. machines is how many other
// machines the cache speaks for, skipped how many fetched documents did
// not parse. A failure keeps the cache and the upload record as they
// were; the caller never fails for it.
func shareLedger(ctx context.Context, cl *hindsight.Client, d paths.Dirs, cfg config.Config, machine string, now time.Time) (uploaded bool, machines, skipped int, err error) {
	if !cfg.Feedback.Enabled || !cfg.Feedback.ShareLedger || machine == "" {
		return false, 0, 0, nil
	}
	uploaded, uerr := uploadShared(ctx, cl, d, cfg, machine, now)
	machines, skipped, rerr := refreshShared(ctx, cl, d, cfg.Server.Bank, machine, now)
	return uploaded, machines, skipped, errors.Join(uerr, rerr)
}

// uploadShared sends this machine's document when it was never sent (or
// the record of it is unreadable), its hidden ids changed, or its entries
// changed and the last send is an hour old.
func uploadShared(ctx context.Context, cl *hindsight.Client, d paths.Dirs, cfg config.Config, machine string, now time.Time) (bool, error) {
	l, err := feedback.Current(d.FeedbackDir())
	if err != nil {
		return false, err
	}
	doc := l.Export(machine, now, feedbackParams(cfg))
	meta, err := doc.MarshalMeta()
	if err != nil {
		return false, err
	}
	timeless := doc
	timeless.At = time.Time{}
	entries, err := timeless.MarshalMeta()
	if err != nil {
		return false, err
	}
	var hidden []string
	for id, e := range doc.Entries {
		if !e.HiddenSince.IsZero() {
			hidden = append(hidden, id)
		}
	}
	slices.Sort(hidden)
	next := shareUpload{At: now, Hash: sha256Hex(entries), HiddenHash: sha256Hex(strings.Join(hidden, "\n"))}
	file := filepath.Join(d.FeedbackDir(), shareUploadRecName)
	var last shareUpload
	if raw, err := os.ReadFile(file); err == nil && json.Unmarshal(raw, &last) == nil && !last.At.IsZero() &&
		last.HiddenHash == next.HiddenHash && (last.Hash == next.Hash || now.Sub(last.At) < shareUploadEvery) {
		return false, nil
	}
	content := fmt.Sprintf("feedback counts of machine %s: %d memories", machine, len(doc.Entries))
	if err := retain.WriteControlDocument(ctx, cl, cfg.Server.Bank, sharedDocPrefix+machine, content, map[string]string{sharedMetaKey: meta}); err != nil {
		return false, err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return true, err
	}
	return true, fsutil.WriteAtomic(file, raw, 0o600)
}

// refreshShared fetches the other machines' documents into shared.json,
// when the cache is shareRefreshEvery old (or dated ahead of now),
// missing or unreadable: those ParseSharedDoc reads, as the server holds
// them, this machine's left out whatever their id. machines is counted
// from the cache as it stands after.
func refreshShared(ctx context.Context, cl *hindsight.Client, d paths.Dirs, bank, machine string, now time.Time) (machines, skipped int, err error) {
	dir := d.FeedbackDir()
	cache, cerr := feedback.LoadSharedCache(dir)
	count := func(c feedback.SharedCache) int {
		docs, _ := c.Parsed()
		return feedback.SharedMachines(docs, machine, now)
	}
	if age := now.Sub(cache.FetchedAt); cerr == nil && age >= 0 && age < shareRefreshEvery {
		return count(cache), 0, nil
	}
	items, err := retain.ControlDocuments(ctx, cl, bank, sharedDocPrefix)
	if err != nil {
		return count(cache), 0, err
	}
	slices.SortFunc(items, func(a, b hindsight.DocumentItem) int { return cmp.Compare(a.ID, b.ID) })
	fresh := feedback.SharedCache{FetchedAt: now, Docs: []json.RawMessage{}}
	for _, it := range items {
		if it.ID == sharedDocPrefix+machine {
			continue
		}
		raw := it.Metadata[sharedMetaKey]
		doc, err := feedback.ParseSharedDoc(raw)
		switch {
		case err != nil:
			skipped++
		case doc.Machine != machine:
			fresh.Docs = append(fresh.Docs, json.RawMessage(raw))
		}
	}
	if err := feedback.SaveSharedCache(dir, fresh); err != nil {
		return count(cache), skipped, err
	}
	return count(fresh), skipped, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
