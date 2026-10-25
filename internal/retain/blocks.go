// Package retain sends settled sessions and notes to Hindsight (SPEC §9.1,
// decisions 0031, 0038, 0050).
package retain

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // UUIDv5 is defined over SHA-1
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/transcript"
)

var (
	nsDNS = [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	// nsBacksight = UUIDv5(DNS namespace, "retain.backsight.invalid").
	nsBacksight = mustParseUUID(uuid5(nsDNS, "retain.backsight.invalid"))
)

func uuid5(ns [16]byte, name string) string {
	h := sha1.New() //nolint:gosec
	h.Write(ns[:])
	h.Write([]byte(name))
	s := h.Sum(nil)[:16]
	s[6] = (s[6] & 0x0f) | 0x50
	s[8] = (s[8] & 0x3f) | 0x80
	x := hex.EncodeToString(s)
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

func mustParseUUID(s string) [16]byte {
	var u [16]byte
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != 16 {
		panic("bad uuid " + s)
	}
	copy(u[:], b)
	return u
}

// Segment is one retain item built from consecutive records.
type Segment struct {
	Item        hindsight.RetainItem
	OperationID string
	FirstUUID   string
	LastUUID    string
	Chars       int
}

type line struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp,omitempty"`
}

// BuildSegment turns records of one session into a retain item; segment 1
// creates the document, later ones append (decision 0050).
// sid is the transcript file's session id, used for the document whatever
// the records say (a resumed or forked file keeps one identity).
func BuildSegment(bank, sid string, recs []transcript.Record, segment int, project, machineID, version string) Segment {
	doc := "conversation:" + sid
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if segment == 1 {
		_ = enc.Encode(line{Role: "system", Content: "REF-ID: " + doc})
	}
	for _, r := range recs {
		for _, t := range r.Turns {
			text, _ := redact.Text(t.Content)
			_ = enc.Encode(line{Role: t.Role, Content: text, Timestamp: t.Timestamp.UTC().Format(time.RFC3339)})
		}
	}
	content := strings.TrimSuffix(buf.String(), "\n")
	first, last := recs[0], recs[len(recs)-1]
	ts := first.Timestamp
	item := hindsight.RetainItem{
		Content: content, Context: "coding agent session", DocumentID: doc, Strategy: "conversation",
		Tags:              []string{"project:" + project, "source:chat", "harness:claude-code"},
		ObservationScopes: [][]string{{"project:" + project}},
		Timestamp:         &ts,
		Metadata: map[string]string{"session_id": sid, "last_uuid": last.UUID, "records": strconv.Itoa(len(recs)),
			"segment": strconv.Itoa(segment), "machine_id": machineID, "binary_version": version, "source": "chat", "harness": "claude-code"},
	}
	if segment > 1 {
		item.UpdateMode = "append"
	}
	sum := sha256.Sum256([]byte(content))
	op := uuid5(nsBacksight, bank+"\n"+doc+"\n"+first.UUID+"\n"+last.UUID+"\n"+hex.EncodeToString(sum[:]))
	return Segment{Item: item, OperationID: op, FirstUUID: first.UUID, LastUUID: last.UUID, Chars: len(content)}
}

// Cursor is what this machine knows it sent of a session (a cache of the
// server's document metadata, decision 0038).
type Cursor struct {
	Transcript  string    `json:"transcript"`
	Offset      int64     `json:"offset"`
	Fingerprint string    `json:"fingerprint"`
	LastUUID    string    `json:"last_uuid"`
	Records     int       `json:"records"`
	Segment     int       `json:"segment"`
	Document    string    `json:"document"`
	Bank        string    `json:"bank"`
	LastTurnAt  time.Time `json:"last_turn_at"`
	SentAt      time.Time `json:"sent_at"`
	Quarantined bool      `json:"quarantined"`
	// Backfill: the session started before retain_since; live retain leaves
	// it to backfill without re-reading it every tick.
	Backfill bool `json:"backfill,omitempty"`
	// NotesReplayed: backfill replayed the session's explicit saves.
	NotesReplayed bool `json:"notes_replayed,omitempty"`
}

func cursorPath(dir, sid string) (string, error) {
	if sid == "" || strings.ContainsAny(sid, `/\`) || sid == "." || sid == ".." {
		return "", fmt.Errorf("invalid session id %q", sid)
	}
	return filepath.Join(dir, sid+".json"), nil
}

// LoadCursor reads dir/<sid>.json; ok is false when there is none.
func LoadCursor(dir, sid string) (Cursor, bool, error) {
	var c Cursor
	p, err := cursorPath(dir, sid)
	if err != nil {
		return c, false, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, false, fmt.Errorf("%s: %w", p, err)
	}
	return c, true, nil
}

// SaveCursor writes dir/<sid>.json atomically (0600).
func SaveCursor(dir, sid string, c Cursor) error {
	p, err := cursorPath(dir, sid)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(p, append(raw, '\n'), 0o600)
}

// fingerprintMax bounds the prefix a fingerprint covers.
const fingerprintMax = 4096

// Fingerprint is the SHA-256 of the first min(consumed, 4 KiB) bytes of the
// transcript: appends after the consumed part keep it, a rewritten file
// does not.
func Fingerprint(path string, consumed int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.CopyN(h, f, min(consumed, fingerprintMax)); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SentIndex holds 8-byte hashes of every record uuid sent from this
// machine, so a forked session skips its parent's records (decision 0038).
type SentIndex struct {
	path string
	set  map[uint64]bool
}

func uuidKey(u string) uint64 {
	s := sha256.Sum256([]byte(u))
	return binary.BigEndian.Uint64(s[:8])
}

// LoadSentIndex reads dir/index.bin.
func LoadSentIndex(dir string) (*SentIndex, error) {
	idx := &SentIndex{path: filepath.Join(dir, "index.bin"), set: map[uint64]bool{}}
	raw, err := os.ReadFile(idx.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for i := 0; i+8 <= len(raw); i += 8 { // a torn trailing entry is ignored
		idx.set[binary.BigEndian.Uint64(raw[i:])] = true
	}
	return idx, nil
}

func (s *SentIndex) Has(uuid string) bool { return s.set[uuidKey(uuid)] }

// sessionKey indexes a record as sent within one session of one bank, which
// tells a fork's copied records (sent under the parent) from this session's
// own, and a bank switch from a pending first segment.
func sessionKey(bank, sid, uuid string) string { return bank + "\x00" + sid + "\x00" + uuid }

// Add appends uuids (one write, fsynced).
func (s *SentIndex) Add(uuids ...string) error {
	var buf []byte
	for _, u := range uuids {
		k := uuidKey(u)
		if s.set[k] {
			continue
		}
		s.set[k] = true
		buf = binary.BigEndian.AppendUint64(buf, k)
	}
	if len(buf) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	// A torn entry from a crash would shift every later key: cut it first.
	if fi, err := f.Stat(); err == nil && fi.Size()%8 != 0 {
		if err := f.Truncate(fi.Size() - fi.Size()%8); err != nil {
			_ = f.Close()
			return err
		}
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Remove drops uuids from the index: a restore lost what they marked as
// sent (decision 0044). The file is rewritten from disk, so entries another
// flusher appended since this index was loaded are kept.
func (s *SentIndex) Remove(uuids ...string) error {
	drop := map[uint64]bool{}
	for _, u := range uuids {
		drop[uuidKey(u)] = true
	}
	if len(drop) == 0 {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	kept := make([]byte, 0, len(raw))
	for i := 0; i+8 <= len(raw); i += 8 {
		if k := binary.BigEndian.Uint64(raw[i:]); !drop[k] {
			kept = append(kept, raw[i:i+8]...)
			s.set[k] = true
		}
	}
	if err := fsutil.WriteAtomic(s.path, kept, 0o600); err != nil {
		return err
	}
	for k := range drop {
		delete(s.set, k)
	}
	return nil
}

// MachineID is a random id created once in dir/machine_id.
func MachineID(dir string) (string, error) {
	p := filepath.Join(dir, "machine_id")
	if raw, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(raw)) == 36 {
		return string(bytes.TrimSpace(raw)), nil
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b)
	id := x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
	if err := fsutil.ReplaceIfUnchanged(p, []byte(id+"\n"), 0o600, fsutil.Absent); err != nil {
		if raw, rerr := os.ReadFile(p); rerr == nil && len(bytes.TrimSpace(raw)) == 36 {
			return string(bytes.TrimSpace(raw)), nil // another process won the race
		}
		return "", err
	}
	return id, nil
}

// QuarantinedSessions lists the sessions whose cursor is quarantined.
func QuarantinedSessions(cursorDir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(cursorDir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		sid := strings.TrimSuffix(filepath.Base(f), ".json")
		if c, ok, err := LoadCursor(cursorDir, sid); err == nil && ok && c.Quarantined {
			out = append(out, sid)
		}
	}
	return out, nil
}

// ReleaseQuarantine drops the cursor of quarantined sessions (all, or only
// sid): the next flush resyncs them from the server's document.
func ReleaseQuarantine(cursorDir, sid string) ([]string, error) {
	held, err := QuarantinedSessions(cursorDir)
	if err != nil {
		return nil, err
	}
	var released []string
	for _, s := range held {
		if sid != "" && s != sid {
			continue
		}
		p, _ := cursorPath(cursorDir, s)
		if err := os.Remove(p); err != nil {
			return released, err
		}
		if _, err := os.Stat(pendingPath(cursorDir, s)); err == nil {
			// quarantined for a first segment never stored: resend it whole
			if err := fsutil.WriteAtomic(pendingPath(cursorDir, s), []byte(resendMark), 0o600); err != nil {
				return released, err
			}
		}
		released = append(released, s)
	}
	return released, nil
}

// MarkNotesReplayed records that a session's explicit saves were replayed.
func MarkNotesReplayed(cursorDir, sid string) error {
	c, ok, err := LoadCursor(cursorDir, sid)
	if err != nil || !ok {
		return err
	}
	c.NotesReplayed = true
	return SaveCursor(cursorDir, sid, c)
}
