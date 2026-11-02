package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
)

// Extraction is <state>/extraction.json: whether this PC holds its sends
// because the server's extraction LLM fails (issue 149). An async retain
// is accepted before its extraction runs: with a key the provider
// rejects, every session sent is accepted, then fails on the server and
// leaves no document. jobs tick writes it after its provider probe; the
// flushes, store, the session banner and doctor read it, with no call of
// their own. While held, sessions stay in Claude Code's transcripts and
// notes in this PC's queue: nothing is sent, so nothing is lost.
type Extraction struct {
	Bank string `json:"bank"`
	// HeldSince: when the hold began; zero while sends go out.
	HeldSince time.Time `json:"held_since,omitzero"`
	// KeyRejected: the provider refused llm_api_key (otherwise the
	// extraction LLM failed for another reason).
	KeyRejected bool `json:"key_rejected,omitempty"`
	// CheckedAt: the last probe that kept the hold.
	CheckedAt time.Time `json:"checked_at,omitzero"`
	// ProbeOKAt: the last probe that passed. A failed retain older than it
	// asks no probe again.
	ProbeOKAt time.Time `json:"probe_ok_at,omitzero"`
}

func extractionPath(d paths.Dirs) string { return filepath.Join(d.State, "extraction.json") }

// HeldFor says whether e holds the sends to bank.
func (e Extraction) HeldFor(bank string) bool {
	return !e.HeldSince.IsZero() && SameBank(e.Bank, bank)
}

// LoadExtraction reads extraction.json; absent or unreadable: no hold.
func LoadExtraction(d paths.Dirs) Extraction {
	var e Extraction
	if raw, err := fsutil.ReadFileShared(extractionPath(d)); err == nil && json.Unmarshal(raw, &e) != nil {
		return Extraction{}
	}
	return e
}

// SaveExtraction writes extraction.json. Only jobs tick writes it (under
// its lock).
func SaveExtraction(d paths.Dirs, e Extraction) error {
	if err := os.MkdirAll(d.State, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(extractionPath(d), raw, 0o600)
}

// Text says what holds the sends, as the flushes, store, jobs tick and
// doctor report it.
func (e Extraction) Text() string {
	since := e.HeldSince.UTC().Format("2006-01-02 15:04") + " UTC"
	if e.KeyRejected {
		return "sending held since " + since + ": the extraction LLM's provider rejects llm_api_key, so sessions and notes wait on this PC"
	}
	return "sending held since " + since + ": the server's extraction LLM fails, so sessions and notes wait on this PC"
}

// Fix says what lifts the hold.
func (e Extraction) Fix() string {
	if e.KeyRejected {
		return "on the server's host: backsight secret set llm_api_key, then backsight server render and backsight server start; the next backsight jobs tick checks the provider again and sends them"
	}
	return "on the server's host, backsight server llm check names the cause (a model, a base URL, the account's credit); once fixed, the next backsight jobs tick checks the provider again and sends them"
}
