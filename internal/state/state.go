// Package state holds backsight's runtime state: state.json and locks
// (SPEC Appendix A.3, §11.4).
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
)

// SchemaVersion of state.json this binary reads and writes.
const SchemaVersion = 1

type State struct {
	SchemaVersion           int       `json:"schema_version"`
	AppliedMigrations       []string  `json:"applied_migrations"`
	BankConfigHash          string    `json:"bank_config_hash"`
	PluginVersion           string    `json:"plugin_version"`
	ShimTarget              string    `json:"shim_target"`
	Backfill                *Backfill `json:"backfill"`
	ReplacedFrom            string    `json:"replaced_from"`
	FirstSessionNoticeShown bool      `json:"first_session_notice_shown"`
	// RetainSince: live retain only takes sessions started after it; older
	// ones belong to backfill (decision 0050).
	RetainSince *time.Time `json:"retain_since,omitempty"`
	Client      *Client    `json:"client,omitempty"`
	// Scheduler: the OS entries `jobs install` wrote (decision 0025).
	Scheduler *Scheduler `json:"scheduler,omitempty"`
	// RestoreEpoch: the bank's restore epoch the cursors were last
	// reconciled with (decision 0044).
	RestoreEpoch *RestoreEpoch `json:"restore_epoch,omitempty"`
	// UpdateCheck: the last release check of `jobs maintain` or `update
	// check` (SPEC §11.7), read by the SessionStart banner and doctor.
	UpdateCheck *UpdateCheck `json:"update_check,omitempty"`
	// SelfCheck: the running binary's check of itself against the signed
	// checksums.txt next to it (SPEC §11.1), made once per binary.
	SelfCheck *SelfCheck `json:"self_check,omitempty"`
	// AutoApply: the release jobs maintain failed to apply, skipped until
	// a newer one appears or update apply is run by hand.
	AutoApply *AutoApply `json:"auto_apply,omitempty"`
	// BackupVerify: the last `backup verify` that ran to its comparison
	// (0067 block 4: doctor's reference-server check reads it).
	BackupVerify *BackupVerify `json:"backup_verify,omitempty"`
	// Tombstones: the documents `forget --purge` deleted, never pruned,
	// mirrored into the bank's control bank (M5 review rules): what they
	// held is never sent again, after a restore or a reinstall either.
	Tombstones []Tombstone `json:"tombstones,omitempty"`
	// PurgeHold: the last reconcile could not purge again the tombstoned
	// documents the server holds; every send holds until one does
	// (issue 285 review LOW-4). Doctor shows it.
	PurgeHold *PurgeHold `json:"purge_hold,omitempty"`
	// PurgeGuard: the periodic look for tombstoned documents a client
	// unaware of tombstones wrote again (backsight issue 5).
	PurgeGuard *PurgeGuard `json:"purge_guard,omitempty"`

	// extra holds fields this binary does not know (written by a newer
	// minor version), saved back unchanged.
	extra map[string]json.RawMessage
}

type plain State // State without its JSON methods

func (s *State) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, (*plain)(s)); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, k := range knownKeys() {
		delete(all, k)
	}
	if len(all) > 0 {
		s.extra = all
	}
	return nil
}

func (s State) MarshalJSON() ([]byte, error) {
	if s.AppliedMigrations == nil {
		s.AppliedMigrations = []string{}
	}
	known, err := json.Marshal(plain(s))
	if err != nil || len(s.extra) == 0 {
		return known, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(known, &all); err != nil {
		return nil, err
	}
	for k, v := range s.extra {
		if _, ok := all[k]; !ok {
			all[k] = v
		}
	}
	return json.Marshal(all)
}

func knownKeys() []string {
	t := reflect.TypeOf(plain{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	return keys
}

// Scheduler is what `jobs install` set up: the scheduler kind and the files
// it wrote, so that `jobs uninstall` and doctor know them.
type Scheduler struct {
	Kind        string    `json:"kind"` // systemd | launchd | schtasks
	Files       []string  `json:"files,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
	// Digest identifies the commands that created Task Scheduler tasks,
	// which have no files to compare (scheduler.Plan.Digest).
	Digest string `json:"commands_sha256,omitempty"`
}

// UpdateCheck is the outcome of the last release check. A failed check
// sets Error and keeps the newest release the last good one found.
type UpdateCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Source    string    `json:"source"`
	Installed string    `json:"installed"`         // the binary that checked
	Latest    string    `json:"latest,omitempty"`  // the newest release newer than Installed
	Summary   string    `json:"summary,omitempty"` // Latest's one-line notes
	Newer     int       `json:"newer,omitempty"`   // releases newer than Installed
	Error     string    `json:"error,omitempty"`   // the check failed
}

// BackupVerify is the outcome of one `backup verify`: the archive (an
// absolute path), its manifest's creation time, and whether it was the
// newest archive in backup.dir when it was verified.
type BackupVerify struct {
	Archive    string    `json:"archive"`
	CreatedAt  time.Time `json:"created_at"`
	Newest     bool      `json:"newest"`
	OK         bool      `json:"ok"`
	VerifiedAt time.Time `json:"verified_at"`
}

// AutoApply is the last failed auto-apply of jobs maintain.
type AutoApply struct {
	Failed string    `json:"failed"` // the release version
	Code   int       `json:"code"`   // update apply's exit code
	Error  string    `json:"error"`
	At     time.Time `json:"at"`
}

// SelfCheck is one binary's check of itself: the binary is named by its
// path, version, size and modification time, so a replaced binary is
// checked again.
type SelfCheck struct {
	Version   string    `json:"version"`
	Exe       string    `json:"exe"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	Status    string    `json:"status"` // verified | unsigned | failed
	Detail    string    `json:"detail,omitempty"`
	KeyID     string    `json:"key_id,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// PurgeHold is what a failed purge again left on the server.
type PurgeHold struct {
	Bank      string `json:"bank"`
	Documents int    `json:"documents"` // tombstoned documents left to purge again
	// Unchecked: a read failed, so whether the server holds them is not
	// known yet.
	Unchecked bool      `json:"unchecked,omitempty"`
	Error     string    `json:"error"`
	At        time.Time `json:"at"`
}

// PurgeGuard is where the look for recreated purged documents stands:
// Hindsight 1.1.0 recreates a deleted document on append, so an older
// binary, or any client unaware of tombstones, can bring a purged id back.
type PurgeGuard struct {
	Bank string `json:"bank"`
	// Newest: the creation time, in the server's clock, of the newest
	// document seen; the next look lists the documents created since, less
	// an overlap.
	Newest time.Time `json:"newest,omitempty"`
	// FullAt: the last look at every tombstone; Tombstones: how many
	// tombstoned documents there were then (more now: look at all again).
	FullAt     time.Time `json:"full_at,omitempty"`
	Tombstones int       `json:"tombstones,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
	// Waiting: tombstoned documents the server holds again, not purged
	// again yet, and why. Each look reads them again.
	Waiting []Recreated `json:"waiting,omitempty"`
	// Purged: the documents the last purge again removed, and when.
	Purged   []string  `json:"purged,omitempty"`
	PurgedAt time.Time `json:"purged_at,omitempty"`
}

// Recreated is a tombstoned document the server holds again.
type Recreated struct {
	Document string `json:"document"`
	Session  string `json:"session,omitempty"`
	Project  string `json:"project,omitempty"`
	Why      string `json:"why,omitempty"`
}

// RestoreEpoch is the bank restore epoch this client reconciled with.
type RestoreEpoch struct {
	Bank         string    `json:"bank"`
	Epoch        int       `json:"epoch"`
	ReconciledAt time.Time `json:"reconciled_at"`
}

// Tombstone is one document a purge deleted, read just before the delete.
// A conversation document's records up to LastUUID (and, for Legacy, every
// project's records up to it) count as sent: no reconcile, resync,
// retry-quarantine or backfill sends them again. Later records of the
// session still go.
type Tombstone struct {
	Bank     string `json:"bank"`
	Document string `json:"document"`
	Session  string `json:"session,omitempty"`
	// Project: the project the document's last segment was sent for.
	Project  string `json:"project,omitempty"`
	LastUUID string `json:"last_uuid,omitempty"`
	// Legacy: the record up to which the document held every project's
	// records (a build from before per-project documents wrote it).
	Legacy string `json:"legacy_last_uuid,omitempty"`
	// ContentSHA256: a note's text, hashed: a backfill replays a save
	// under another document id than the one stored live.
	ContentSHA256 string    `json:"content_sha256,omitempty"`
	PurgedAt      time.Time `json:"purged_at"`
}

// same: one purge of one document, whenever it was recorded.
func (t Tombstone) same(o Tombstone) bool {
	return t.Bank == o.Bank && t.Document == o.Document && t.LastUUID == o.LastUUID && t.Legacy == o.Legacy
}

// AddTombstones adds the tombstones s does not hold yet and returns how
// many it added.
func (s *State) AddTombstones(ts ...Tombstone) int {
	n := 0
	for _, t := range ts {
		if !slices.ContainsFunc(s.Tombstones, t.same) {
			s.Tombstones = append(s.Tombstones, t)
			n++
		}
	}
	return n
}

// TombstonesOf are the tombstones of bank.
func (s State) TombstonesOf(bank string) []Tombstone {
	var out []Tombstone
	for _, t := range s.Tombstones {
		if t.Bank == bank {
			out = append(out, t)
		}
	}
	return out
}

// EpochOf is the restore epoch recorded for bank: 0 when none was (every
// install before M5, or another bank).
func (s State) EpochOf(bank string) int {
	if s.RestoreEpoch == nil || s.RestoreEpoch.Bank != bank {
		return 0
	}
	return s.RestoreEpoch.Epoch
}

// Client remembers Claude Code settings as they were before backsight first
// changed them, so uninstall can offer them back (decision 0052).
type Client struct {
	// RetentionBefore: absent = not recorded yet; null = the key was absent.
	RetentionBefore json.RawMessage `json:"retention_before,omitempty"`
	// Profile is the Claude Code directory client install wired ("" = the
	// default ~/.claude); ShimPrevious the binary the shim ran before, for
	// a rollback.
	Profile      string `json:"profile,omitempty"`
	ShimPrevious string `json:"shim_previous,omitempty"`
}

// Backfill tracks the replay of past transcripts (decision 0037).
type Backfill struct {
	Done      int       `json:"done"`
	Total     int       `json:"total"`
	StartedAt time.Time `json:"started_at"`
	// Budget (decision 0051): a cap in currency or in tokens and the spend
	// measured so far.
	MaxCost     float64 `json:"max_cost,omitempty"`
	MaxTokens   int64   `json:"max_tokens,omitempty"`
	Currency    string  `json:"currency,omitempty"`
	SpentCost   float64 `json:"spent_cost"`
	SpentTokens int64   `json:"spent_tokens"`
	// StartDay (UTC) and its token sums when the backfill started: spend is
	// the start day's increase plus every later day.
	StartDay  string   `json:"start_day"`
	StartBase [4]int64 `json:"start_base"` // input, cached, output, thoughts
	// Paused: budget | server | measure | interrupted | "" (running).
	Paused            string   `json:"paused,omitempty"`
	AllowSubscription bool     `json:"allow_subscription,omitempty"`
	Filters           string   `json:"filters,omitempty"`  // since|project|projects-dir of the queue
	Sessions          []string `json:"sessions,omitempty"` // still to send, oldest first
}

// Load reads path; a missing file is an empty state of the current schema.
// Load then Save is not safe against concurrent writers: use Update.
func Load(path string) (State, error) {
	s := State{SchemaVersion: SchemaVersion, AppliedMigrations: []string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if s.SchemaVersion > SchemaVersion {
		return s, fmt.Errorf("%s: schema_version %d was written by a newer backsight (this one reads %d)", path, s.SchemaVersion, SchemaVersion)
	}
	return s, nil
}

// Save writes s atomically with mode 0600.
func Save(path string, s State) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(raw, '\n'), 0o600)
}

// Update loads state.json, applies fn and saves it, under the state lock.
// It never waits: a held lock returns the *HeldError (a stale one is
// taken over). fn's error aborts
// without writing.
func Update(d paths.Dirs, command string, fn func(*State) error) error {
	l, _, err := Acquire(filepath.Join(d.LocksDir(), "state.lock"), command)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()
	s, err := Load(d.StateFile())
	if err != nil {
		return err
	}
	if err := fn(&s); err != nil {
		return err
	}
	return Save(d.StateFile(), s)
}

// UpdateWithin is Update that waits for a held state lock, up to wait:
// update apply's record and rollback must not fail on a tick or a hook
// writing state.json at that moment. fn's own error is not retried.
func UpdateWithin(d paths.Dirs, command string, wait time.Duration, fn func(*State) error) error {
	deadline := time.Now().Add(wait)
	for {
		err := Update(d, command, fn)
		var held *HeldError
		if !errors.As(err, &held) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// SaveWithin writes raw as state.json under the state lock, waiting for it
// up to wait (a rollback putting back the state as it was); nil removes it.
func SaveWithin(d paths.Dirs, command string, wait time.Duration, raw []byte) error {
	deadline := time.Now().Add(wait)
	for {
		l, _, err := Acquire(filepath.Join(d.LocksDir(), "state.lock"), command)
		var held *HeldError
		if errors.As(err, &held) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
		if raw == nil {
			err = os.Remove(d.StateFile())
			if errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
		} else {
			err = fsutil.WriteAtomic(d.StateFile(), raw, 0o600)
		}
		_ = l.Release()
		return err
	}
}
