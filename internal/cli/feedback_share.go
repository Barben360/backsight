package cli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// A machine's shared ledger is the control bank document
// sharedDocPrefix+<machine id>, its counts under the metadata key
// sharedMetaKey and nothing else there. It is sent again when it changed
// and the last send is shareUploadEvery old, at once when the set of
// hidden memories changed, and unchanged once the last send is
// shareResendEvery old (the other machines ignore a document a month
// old); the other machines' are fetched every shareRefreshEvery.
const (
	sharedDocPrefix   = "backsight:feedback:"
	sharedMetaKey     = "feedback"
	shareUploadEvery  = time.Hour
	shareResendEvery  = 7 * 24 * time.Hour
	shareRefreshEvery = 6 * time.Hour
	// shareMaxDocs: a refresh caches at most this many of the other
	// machines' documents, the newest.
	shareMaxDocs = 64
)

// shareTickBudget bounds the tick's sharing step, send and fetch.
var shareTickBudget = 10 * time.Second

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
	machines, skipped, rerr := refreshShared(ctx, cl, d, cfg.Server.Bank, machine, now, feedbackParams(cfg))
	return uploaded, machines, skipped, errors.Join(uerr, rerr)
}

// uploadShared sends this machine's document when it was never sent (or
// the record of it is unreadable or dated ahead of now: the clock stepped
// back), it was sent to another server or bank (shareTarget), its hidden
// ids changed, its entries changed and the last send is an hour old, or
// the last send is a week old.
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
	next := feedback.SharedUpload{At: now, Server: shareTarget(d, cfg), Hash: sha256Hex(entries), HiddenHash: sha256Hex(strings.Join(hidden, "\n"))}
	if last, ok := feedback.LoadSharedUpload(d.FeedbackDir()); ok && !last.At.After(now) && last.Server == next.Server && last.HiddenHash == next.HiddenHash {
		if age := now.Sub(last.At); age < shareUploadEvery || (last.Hash == next.Hash && age < shareResendEvery) {
			return false, nil
		}
	}
	content := fmt.Sprintf("feedback counts of machine %s: %d memories", machine, len(doc.Entries))
	if err := retain.WriteControlDocument(ctx, cl, cfg.Server.Bank, sharedDocPrefix+machine, content, map[string]string{sharedMetaKey: meta}); err != nil {
		return false, err
	}
	return true, feedback.SaveSharedUpload(d.FeedbackDir(), next)
}

// shareTarget names the server and bank this machine's document goes
// to: the id of the server this client joined (the one the foreign-server
// guard checks), else the configured address.
func shareTarget(d paths.Dirs, cfg config.Config) string {
	server := "url " + cfg.Server.URL
	if st, err := state.Load(d.StateFile()); err == nil && cfg.Server.Mode == "remote" && st.RemoteServer != nil && st.RemoteServer.ID != "" {
		server = "id " + st.RemoteServer.ID
	}
	return server + " bank " + cfg.Server.Bank
}

// refreshShared fetches the other machines' documents into shared.json,
// when the cache is shareRefreshEvery old (or dated ahead of now),
// missing or unreadable: those ParseSharedDoc reads, as the server holds
// them, under their own name (the machine they name is their id's), this
// machine's left out whatever their id, none a month old, at most
// shareMaxDocs of them, the newest. Then it combines them
// under p into shared-combined, which recall reads (combineShared),
// also when only that file is missing or unreadable. machines is counted
// from the cache as it stands after.
func refreshShared(ctx context.Context, cl *hindsight.Client, d paths.Dirs, bank, machine string, now time.Time, p feedback.Params) (machines, skipped int, err error) {
	dir := d.FeedbackDir()
	cache, cerr := feedback.LoadSharedCache(dir)
	count := func(c feedback.SharedCache) int {
		docs, _ := c.Parsed()
		return feedback.SharedMachines(docs, machine, now)
	}
	if age := now.Sub(cache.FetchedAt); cerr == nil && age >= 0 && age < shareRefreshEvery {
		if _, lerr := feedback.LoadSharedCombined(dir, now); lerr != nil {
			return count(cache), 0, combineShared(dir, cache, machine, now, p)
		}
		return count(cache), 0, nil
	}
	items, err := retain.ControlDocuments(ctx, cl, bank, sharedDocPrefix)
	if err != nil {
		return count(cache), 0, err
	}
	slices.SortFunc(items, func(a, b hindsight.DocumentItem) int { return cmp.Compare(a.ID, b.ID) })
	type fetched struct {
		id, raw string
		at      time.Time
	}
	var kept []fetched
	for _, it := range items {
		if it.ID == sharedDocPrefix+machine {
			continue
		}
		raw := it.Metadata[sharedMetaKey]
		doc, err := feedback.ParseSharedDoc(raw)
		switch {
		case err != nil, doc.Machine != strings.TrimPrefix(it.ID, sharedDocPrefix):
			skipped++
		case !doc.Expired(now):
			kept = append(kept, fetched{it.ID, raw, doc.At})
		}
	}
	slices.SortFunc(kept, func(a, b fetched) int { // newest first
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})
	kept = kept[:min(len(kept), shareMaxDocs)]
	slices.SortFunc(kept, func(a, b fetched) int { return cmp.Compare(a.id, b.id) })
	fresh := feedback.SharedCache{FetchedAt: now, Docs: []json.RawMessage{}}
	for _, f := range kept {
		fresh.Docs = append(fresh.Docs, json.RawMessage(f.raw))
	}
	if err := feedback.SaveSharedCache(dir, fresh); err != nil {
		return count(cache), skipped, err
	}
	return count(fresh), skipped, combineShared(dir, fresh, machine, now, p)
}

// combineShared writes c's documents combined at now (to the second)
// under p, this machine's left out: what recall adds to its own counts.
func combineShared(dir string, c feedback.SharedCache, machine string, now time.Time, p feedback.Params) error {
	docs, _ := c.Parsed()
	at := now.Truncate(time.Second)
	return feedback.SaveSharedCombined(dir, at, feedback.CombineRemote(docs, machine, at, p), feedback.BasisOf(docs, machine, at, p))
}

// recombineShared is the tick's local look at the combined view, with
// feedback and share_ledger on: combined again from shared.json, never
// the server, when SharedCombinedOutdated says so (a half-life changed,
// a machine it counts turned 30 days old, or an older binary wrote it),
// so that either counts within one tick, not at the next refresh. Without
// a readable shared.json it leaves the view as it is. The caller holds
// the tick's lock, which the refresh writes under too.
func recombineShared(d paths.Dirs, cfg config.Config, machine string, now time.Time) error {
	if !cfg.Feedback.Enabled || !cfg.Feedback.ShareLedger {
		return nil
	}
	dir, p := d.FeedbackDir(), feedbackParams(cfg)
	if !feedback.SharedCombinedOutdated(dir, now, p) {
		return nil
	}
	cache, err := feedback.LoadSharedCache(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return combineShared(dir, cache, machine, now, p)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
