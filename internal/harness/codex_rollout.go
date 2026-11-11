package harness

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/Barben360/backsight/internal/attribution"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/transcript"
)

// Codex's conversation store, its rollouts: block 6
// (https://github.com/Barben360/backsight/issues/281), 0085 D7.
//
// A thread is one file, $CODEX_HOME/sessions/YYYY/MM/DD/rollout-<time>-<thread
// id>.jsonl, which Codex may compress in place after a week
// (rollout-….jsonl.zst, zstd, the same bytes once decompressed). Its
// format is Codex's own, with no stability guarantee: every line is read
// as untrusted input. A line that is not JSON, too long, or of a type this
// reader does not know is counted and skipped, never fatal; a compressed
// file that decompresses to more than rolloutMax is an error for that file
// alone.
//
// What a rollout line becomes (Codex 0.167.0, the recorded fixtures):
//   - response_item/message, role user: the user's text, only its items
//     Codex marks as typed by the user (provenance "user"); the
//     environment context, subagent notifications and the like Codex
//     injects as user messages are stripped. A Codex writing no
//     provenance falls back to stripping its known blocks by their tag.
//   - response_item/message, role assistant: the reply's text, except the
//     summary a compaction writes (metadata.compaction_output).
//   - response_item/function_call, custom_tool_call, local_shell_call,
//     web_search_call: one action each, with its input for attribution
//     (a shell command array is joined into one string).
//   - developer messages (instructions, hook context), reasoning, tool
//     outputs, event_msg (which repeats the items), turn_context (whose cwd
//     the next records take), session_meta (the thread's identity and its
//     first cwd), compacted (a copy of earlier items), world_state,
//     token_usage_record: no turn.
//
// A record's UUID is a hash of its line: stable across reads, resumes
// (Codex appends to the same file) and compression, unique within a
// thread, and the same in a copy of the line (a fork that copies its
// parent's history keeps the parent's ids, which the sent index holds).

// rolloutMax bounds a rollout's content stream: a compressed file that
// decompresses past it is refused (a decompression bomb never fills the
// disk or blocks a flush). A variable so tests can shrink it.
var rolloutMax int64 = 1 << 30

// rolloutLineMax bounds one rollout line; a longer one is skipped (and
// counted) without being buffered. A variable so tests can shrink it.
var rolloutLineMax = 16 << 20

// rolloutScanMax bounds how much of a rollout's head is read for its
// session_meta (its first line).
const rolloutScanMax = 4 << 20

// codexRolloutRe is a rollout's file name: rollout-<time>-<thread id>,
// the id a UUID (Codex's are UUIDv7).
var codexRolloutRe = regexp.MustCompile(`^rollout-.+-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl(\.zst)?$`)

// codexSessionsDir is where env's Codex keeps its rollouts: its home's
// sessions/ (the home: env.Profile, else ~/.codex).
func (c Codex) codexSessionsDir(env Env) string {
	if l := c.Locate(env).Conversations; l != "" {
		return l
	}
	home := env.Profile
	if home == "" {
		if env.Home == "" {
			return ""
		}
		home = filepath.Join(env.Home, ".codex")
	}
	return filepath.Join(home, "sessions")
}

// Conversations are the root threads' rollouts of env's Codex,
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread id>.jsonl or
// .jsonl.zst, sorted. A subagent's thread (its session_meta names a
// parent) is not one: it belongs to its session, as a report (Subagents).
// A thread found both plain and compressed (Codex compressing it) is
// listed once, plain.
func (c Codex) Conversations(env Env) []string {
	dir := c.codexSessionsDir(env)
	if dir == "" {
		return nil
	}
	var files []string
	for _, pat := range []string{"rollout-*.jsonl", "rollout-*.jsonl.zst"} {
		m, _ := filepath.Glob(filepath.Join(dir, "*", "*", "*", pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	var out []string
	for _, f := range files {
		if c.SessionOf(f) == "" || isRolloutDuplicate(f) {
			continue
		}
		// A rollout is a file Codex wrote: a symbolic link (or anything
		// else) in its place is not followed.
		if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if m, ok := ReadRolloutMeta(f); ok && m.Subagent() {
			continue
		}
		out = append(out, f)
	}
	return out
}

// isRolloutDuplicate: f is the compressed copy of a plain rollout that is
// still there (Codex writes the copy, then removes the plain file).
func isRolloutDuplicate(f string) bool {
	plain, ok := strings.CutSuffix(f, ".zst")
	if !ok {
		return false
	}
	_, err := os.Stat(plain)
	return err == nil
}

// SessionOf is the thread id ending a rollout's name; "" for a file that
// is not a rollout.
func (Codex) SessionOf(path string) string {
	m := codexRolloutRe.FindStringSubmatch(filepath.Base(path))
	if m == nil || !session.ValidID(m[1]) {
		return ""
	}
	return m[1]
}

// Current is where the rollout path names is now: path, or its
// compressed twin once Codex compressed it in place (retain compares a
// cursor's or a ledger's path with it).
func (Codex) Current(path string) string { return rolloutPath(path) }

// rolloutPath is where path's rollout is now: path, or its compressed or
// plain twin once Codex compressed it (a cursor names the file it read).
func rolloutPath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	twin := path + ".zst"
	if p, ok := strings.CutSuffix(path, ".zst"); ok {
		twin = p
	}
	if _, err := os.Stat(twin); err == nil {
		return twin
	}
	return path
}

func compressed(path string) bool { return strings.HasSuffix(path, ".zst") }

// Size is the rollout's content length: the file's size, or for
// .jsonl.zst the sum of its frames' declared sizes, else what it
// decompresses to (at most rolloutMax).
func (Codex) Size(path string) (int64, error) {
	path = rolloutPath(path)
	if !compressed(path) {
		fi, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		return fi.Size(), nil
	}
	if n, ok := knownContentSize(path); ok {
		return n, nil
	}
	rc, err := openRolloutContent(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()
	sizeDecompressions.Add(1)
	n, err := io.Copy(io.Discard, rc)
	if err == nil {
		rememberContentSize(path, n)
	}
	return n, err
}

// sizeDecompressions counts the compressed rollouts Size decompressed to
// learn their length (a test seam: a second Size of an unchanged file
// decompresses nothing).
var sizeDecompressions atomic.Int64

// contentSizes are the content lengths Size learnt by decompressing a
// .zst that declares none, by file, for as long as the file's size and
// modification time stay the same (a process runs Size and Open on the
// same file several times per flush).
var contentSizes sync.Map // path -> sizedFile

type sizedFile struct {
	size    int64
	mtime   time.Time
	content int64
}

// knownContentSize is a .zst rollout's content length without
// decompressing it: what its frames declare, else what Size learnt of the
// same unchanged file.
func knownContentSize(path string) (int64, bool) {
	if n, ok := zstdDeclaredSize(path); ok {
		return n, true
	}
	v, ok := contentSizes.Load(path)
	if !ok {
		return 0, false
	}
	fi, err := os.Stat(path)
	if c := v.(sizedFile); err == nil && fi.Size() == c.size && fi.ModTime().Equal(c.mtime) {
		return c.content, true
	}
	contentSizes.Delete(path)
	return 0, false
}

func rememberContentSize(path string, n int64) {
	if fi, err := os.Stat(path); err == nil {
		contentSizes.Store(path, sizedFile{size: fi.Size(), mtime: fi.ModTime(), content: n})
	}
}

// Open is the rollout's content from offset from on, decompressed for
// .jsonl.zst (what it skips is decompressed and discarded). The reader
// knows its rollout, so ReadConversation can read the context a read
// that starts mid-file needs (the thread's identity, the cwd in force).
func (c Codex) Open(path string, from int64) (io.ReadCloser, error) {
	path = rolloutPath(path)
	if from < 0 {
		return nil, fmt.Errorf("%s: negative offset %d", path, from)
	}
	if compressed(path) && from > 0 {
		// A compressed rollout read up to its end (the usual case: Codex
		// compresses only week-old threads) decompresses nothing.
		if n, ok := knownContentSize(path); ok && from >= n {
			return &rolloutStream{Reader: bytes.NewReader(nil), closer: io.NopCloser(nil), path: path, from: from}, nil
		}
	}
	rc, err := openRolloutContent(path)
	if err != nil {
		return nil, err
	}
	if from > 0 {
		if s, ok := rc.(io.Seeker); ok {
			if _, err := s.Seek(from, io.SeekStart); err != nil {
				_ = rc.Close()
				return nil, err
			}
		} else if _, err := io.CopyN(io.Discard, rc, from); err != nil && !errors.Is(err, io.EOF) {
			_ = rc.Close()
			return nil, err
		}
	}
	return &rolloutStream{Reader: rc, closer: rc, path: path, from: from}, nil
}

// rolloutStream is a rollout's content stream from offset from.
type rolloutStream struct {
	io.Reader
	closer io.Closer
	path   string
	from   int64
}

func (s *rolloutStream) Close() error { return s.closer.Close() }

// openRolloutContent is the rollout's content stream from its start: the
// file (a Seeker), or its zstd decompression capped at rolloutMax.
func openRolloutContent(path string) (io.ReadCloser, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !compressed(path) {
		return fh, nil
	}
	dec, err := zstd.NewReader(bufio.NewReaderSize(fh, 1<<16), zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(128<<20), zstd.WithDecoderMaxMemory(uint64(rolloutMax)))
	if err != nil {
		_ = fh.Close()
		return nil, err
	}
	return &zstdContent{dec: dec, fh: fh, left: rolloutMax, path: path}, nil
}

// zstdContent is a decompressed rollout, refused past rolloutMax.
type zstdContent struct {
	dec  *zstd.Decoder
	fh   *os.File
	left int64
	path string
}

// errRolloutTooLarge: a compressed rollout decompresses past rolloutMax.
var errRolloutTooLarge = errors.New("decompresses to more than the size backsight reads")

func (z *zstdContent) Read(p []byte) (int, error) {
	if z.left <= 0 {
		// One byte more tells a stream of exactly rolloutMax bytes from a
		// longer one.
		var b [1]byte
		if n, _ := z.dec.Read(b[:]); n > 0 {
			return 0, fmt.Errorf("%s: %w", filepath.Base(z.path), errRolloutTooLarge)
		}
		return 0, io.EOF
	}
	if int64(len(p)) > z.left {
		p = p[:z.left]
	}
	n, err := z.dec.Read(p)
	z.left -= int64(n)
	if errors.Is(err, zstd.ErrDecoderSizeExceeded) {
		err = fmt.Errorf("%s: %w", filepath.Base(z.path), errRolloutTooLarge) // a frame declaring more
	}
	return n, err
}

func (z *zstdContent) Close() error {
	z.dec.Close()
	return z.fh.Close()
}

// zstdDeclaredSize is the content size a .zst file's frames declare, when
// every frame declares one (ok false otherwise, or on a malformed frame):
// read from the frame and block headers, nothing decompressed.
func zstdDeclaredSize(path string) (int64, bool) {
	fh, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = fh.Close() }()
	br := bufio.NewReaderSize(fh, 1<<16)
	var total int64
	frames := 0
	for {
		var magic [4]byte
		if _, err := io.ReadFull(br, magic[:]); err != nil {
			return total, errors.Is(err, io.EOF) && frames > 0
		}
		m := binary.LittleEndian.Uint32(magic[:])
		if m&0xFFFFFFF0 == 0x184D2A50 { // a skippable frame: no content
			var size [4]byte
			if _, err := io.ReadFull(br, size[:]); err != nil {
				return 0, false
			}
			if _, err := br.Discard(int(binary.LittleEndian.Uint32(size[:]))); err != nil {
				return 0, false
			}
			continue
		}
		if m != 0xFD2FB528 {
			return 0, false
		}
		n, ok := zstdFrameSize(br)
		if !ok || n < 0 || total+n > rolloutMax {
			return 0, false
		}
		total += n
		frames++
	}
}

// zstdFrameSize reads one frame after its magic: its declared content size,
// its blocks skipped (RFC 8878 §3.1.1).
func zstdFrameSize(br *bufio.Reader) (int64, bool) {
	fhd, err := br.ReadByte()
	if err != nil {
		return 0, false
	}
	fcsFlag, single, checksum, dictFlag := fhd>>6, fhd&0x20 != 0, fhd&0x04 != 0, fhd&0x03
	if !single {
		if _, err := br.Discard(1); err != nil { // window descriptor
			return 0, false
		}
	}
	if _, err := br.Discard([]int{0, 1, 2, 4}[dictFlag]); err != nil {
		return 0, false
	}
	fcsSize := []int{0, 2, 4, 8}[fcsFlag]
	if fcsFlag == 0 && single {
		fcsSize = 1
	}
	if fcsSize == 0 {
		return 0, false // no declared size
	}
	var raw [8]byte
	if _, err := io.ReadFull(br, raw[:fcsSize]); err != nil {
		return 0, false
	}
	fcs := int64(binary.LittleEndian.Uint64(raw[:]))
	if fcsSize == 2 {
		fcs += 256
	}
	for {
		var h [3]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return 0, false
		}
		bh := uint32(h[0]) | uint32(h[1])<<8 | uint32(h[2])<<16
		last, typ, size := bh&1 != 0, (bh>>1)&3, int(bh>>3)
		switch typ {
		case 1: // RLE: one byte repeated size times
			size = 1
		case 3: // reserved
			return 0, false
		}
		if _, err := br.Discard(size); err != nil {
			return 0, false
		}
		if last {
			break
		}
	}
	if checksum {
		if _, err := br.Discard(4); err != nil {
			return 0, false
		}
	}
	return fcs, true
}

// RolloutMeta is a thread's identity, from its rollout's session_meta.
type RolloutMeta struct {
	// ID is the thread's id; SessionID the root session's (a subagent's
	// thread names its root session).
	ID, SessionID string
	// ParentThreadID is the thread that spawned this one (a subagent);
	// ForkedFromID the thread this one was forked from (a new session).
	ParentThreadID, ForkedFromID string
	// Cwd is where the thread started; Originator the Codex front end that
	// started it (codex-tui, codex_exec…), backsight's entrypoint.
	Cwd, Originator string
	// AgentType is a subagent's role, "default" when it has none.
	AgentType string
	subagent  bool
}

// Subagent: the thread was spawned by another one (its report goes to its
// session, it is no conversation of its own).
func (m RolloutMeta) Subagent() bool { return m.subagent || m.ParentThreadID != "" }

type rawMeta struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"session_id"`
	ParentThreadID string          `json:"parent_thread_id"`
	ForkedFromID   string          `json:"forked_from_id"`
	Cwd            string          `json:"cwd"`
	Originator     string          `json:"originator"`
	Source         json.RawMessage `json:"source"`
	ThreadSource   string          `json:"thread_source"`
}

func (r rawMeta) meta() RolloutMeta {
	m := RolloutMeta{ID: r.ID, SessionID: r.SessionID, ParentThreadID: r.ParentThreadID, ForkedFromID: r.ForkedFromID, Cwd: r.Cwd, Originator: r.Originator}
	var src struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	if json.Unmarshal(r.Source, &src) == nil && len(src.Subagent) > 0 && string(src.Subagent) != "null" {
		m.subagent = true
		var spawn struct {
			ThreadSpawn struct {
				ParentThreadID string  `json:"parent_thread_id"`
				AgentRole      *string `json:"agent_role"`
			} `json:"thread_spawn"`
		}
		if json.Unmarshal(src.Subagent, &spawn) == nil {
			if m.ParentThreadID == "" {
				m.ParentThreadID = spawn.ThreadSpawn.ParentThreadID
			}
			if spawn.ThreadSpawn.AgentRole != nil {
				m.AgentType = *spawn.ThreadSpawn.AgentRole
			}
		}
	}
	if r.ThreadSource == "subagent" {
		m.subagent = true
	}
	if m.Subagent() && m.AgentType == "" {
		m.AgentType = "default" // what Codex's hooks name a role-less subagent
	}
	if m.SessionID == "" {
		m.SessionID = m.ID
	}
	return m
}

// ReadRolloutMeta is the session_meta of the rollout at path (its first
// line); ok false when it has none.
func ReadRolloutMeta(path string) (RolloutMeta, bool) {
	rc, err := openRolloutContent(rolloutPath(path))
	if err != nil {
		return RolloutMeta{}, false
	}
	defer func() { _ = rc.Close() }()
	br := bufio.NewReaderSize(io.LimitReader(rc, rolloutScanMax), 1<<16)
	for i := 0; i < 8; i++ { // session_meta is the first line; a few tolerated before it
		line, _, tooLong, err := readRolloutLine(br)
		if !tooLong {
			var l rolloutLine
			if json.Unmarshal(bytes.TrimSpace(line), &l) == nil && l.Type == "session_meta" {
				var r rawMeta
				if json.Unmarshal(l.Payload, &r) == nil {
					return r.meta(), true
				}
			}
		}
		if err != nil {
			break
		}
	}
	return RolloutMeta{}, false
}

// Subagents are the rollouts of the subagent threads of the session whose
// rollout is path (its descendants, all naming it as their session), found
// in the same sessions/ tree; sorted.
func (c Codex) Subagents(path string) []string {
	root := c.SessionOf(path)
	if root == "" {
		return nil
	}
	dir := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))) // sessions/YYYY/MM/DD/file
	var files []string
	for _, pat := range []string{"rollout-*.jsonl", "rollout-*.jsonl.zst"} {
		m, _ := filepath.Glob(filepath.Join(dir, "*", "*", "*", pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	var out []string
	for _, f := range files {
		id := c.SessionOf(f)
		if id == "" || id == root || isRolloutDuplicate(f) {
			continue
		}
		if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if m, ok := ReadRolloutMeta(f); ok && m.Subagent() && strings.EqualFold(m.SessionID, root) {
			out = append(out, f)
		}
	}
	return out
}

// Entrypoint is how the thread of the rollout at path was started: its
// session_meta's originator (codex-tui, codex_exec…), read from its first
// line alone; "" when unknown. A hook asks it within its budget.
func (Codex) Entrypoint(path string) string {
	m, _ := ReadRolloutMeta(path)
	return m.Originator
}

// AgentType is a subagent rollout's role ("default" when it has none), ""
// for a rollout that is no subagent's.
func (Codex) AgentType(path string) string {
	m, ok := ReadRolloutMeta(path)
	if !ok || !m.Subagent() {
		return ""
	}
	return m.AgentType
}

// rolloutLine is one line of a rollout.
type rolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Metadata  struct {
		CompactionOutput bool `json:"compaction_output"`
	} `json:"metadata"`
}

// codexKnown are the line types Codex 0.167.0 writes; any other is
// counted as unknown (a format change to look at, SPEC §14).
var codexKnown = map[string]bool{
	"session_meta": true, "response_item": true, "event_msg": true, "turn_context": true,
	"compacted": true, "world_state": true, "token_usage_record": true,
}

// codexKnownItems are the response_item payload types this reader knows
// (most carry no turn); another one is counted as unknown, as
// "response_item/<type>".
var codexKnownItems = map[string]bool{
	"message": true, "function_call": true, "function_call_output": true, "custom_tool_call": true,
	"custom_tool_call_output": true, "local_shell_call": true, "web_search_call": true, "reasoning": true,
	"image_generation_call": true, "ghost_snapshot": true, "compaction": true, "compaction_summary": true,
	"tool_search_call": true, "tool_search_output": true, "other": true,
}

type codexItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	CallID    string          `json:"call_id"`
	ID        string          `json:"id"`
	Action    json.RawMessage `json:"action"`
	Meta      struct {
		Kinds    []string `json:"content_item_kinds"`
		Metadata []struct {
			Provenance struct {
				Type string `json:"type"`
			} `json:"provenance"`
		} `json:"content_item_metadata"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// rolloutContext is what a read needs from before its start: the
// thread's identity and the cwd in force.
type rolloutContext struct {
	meta    RolloutMeta
	hasMeta bool
	cwd     string
}

// load reads, from the stream's start up to its offset (at most the first
// line when only the identity is needed), the context a read starting there
// needs.
func (s *rolloutStream) load(withCwd bool) rolloutContext {
	var ctx rolloutContext
	rc, err := openRolloutContent(s.path)
	if err != nil {
		return ctx
	}
	defer func() { _ = rc.Close() }()
	br := bufio.NewReaderSize(io.LimitReader(rc, s.from), 1<<16)
	for {
		line, _, tooLong, err := readRolloutLine(br)
		if !tooLong && (bytes.Contains(line, []byte(`"session_meta"`)) || bytes.Contains(line, []byte(`"turn_context"`))) {
			var l rolloutLine
			if json.Unmarshal(bytes.TrimSpace(line), &l) == nil {
				ctx.apply(l)
			}
		}
		if err != nil || (!withCwd && ctx.hasMeta) {
			return ctx
		}
	}
}

// apply takes l's identity or cwd.
func (c *rolloutContext) apply(l rolloutLine) {
	switch l.Type {
	case "session_meta":
		var r rawMeta
		if json.Unmarshal(l.Payload, &r) == nil {
			c.meta, c.hasMeta = r.meta(), true
			if r.Cwd != "" {
				c.cwd = r.Cwd
			}
		}
	case "turn_context":
		var t struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(l.Payload, &t) == nil && t.Cwd != "" {
			c.cwd = t.Cwd
		}
	}
}

// ReadConversation normalises the complete lines of r, which starts at
// offset from of a rollout's content stream (Open's): the user's and the
// assistant's text, and the tool calls as actions. A read that starts
// mid-file takes the thread's identity and the cwd in force from the
// rollout's earlier lines when r is Open's; another reader's records
// before any turn_context have no cwd (the flush keeps them out).
func (Codex) ReadConversation(r io.Reader, from int64) ([]transcript.Record, transcript.Stats, error) {
	st := transcript.Stats{Consumed: from, Unknown: map[string]int{}}
	stream, _ := r.(*rolloutStream)
	var ctx rolloutContext
	metaLoaded, cwdLoaded := from == 0, from == 0
	// before is the context from before from, read once and only when a
	// record needs it.
	before := func(withCwd bool) {
		if stream == nil || stream.from != from || (metaLoaded && (cwdLoaded || !withCwd)) {
			return
		}
		got := stream.load(withCwd)
		if !ctx.hasMeta && got.hasMeta {
			ctx.meta, ctx.hasMeta = got.meta, true
		}
		if withCwd && ctx.cwd == "" {
			ctx.cwd = got.cwd
		}
		metaLoaded, cwdLoaded = true, cwdLoaded || withCwd
	}
	br := bufio.NewReaderSize(r, 1<<16)
	var recs []transcript.Record
	off := from
	var prevTS time.Time
	for {
		raw, n, tooLong, err := readRolloutLine(br)
		if errors.Is(err, io.EOF) {
			return recs, st, nil // partial or empty last line: not consumed
		}
		if err != nil {
			return recs, st, err
		}
		start := off
		off += n
		st.Consumed = off
		if tooLong {
			st.BadLines++
			continue
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var l rolloutLine
		if json.Unmarshal(raw, &l) != nil || l.Type == "" {
			st.BadLines++
			continue
		}
		if !codexKnown[l.Type] {
			st.Unknown[l.Type]++
			st.UnknownLines = append(st.UnknownLines, transcript.UnknownLine{Type: l.Type, End: off})
			continue
		}
		if l.Type != "response_item" {
			st.Unknown[l.Type]++
			ctx.apply(l)
			if l.Type == "turn_context" {
				cwdLoaded = true // what came before no longer matters
			}
			continue
		}
		var it codexItem
		if json.Unmarshal(l.Payload, &it) != nil {
			st.BadLines++
			continue
		}
		if !codexKnownItems[it.Type] {
			t := "response_item/" + it.Type
			st.Unknown[t]++
			st.UnknownLines = append(st.UnknownLines, transcript.UnknownLine{Type: t, End: off})
			continue
		}
		ts := parseCodexTime(l.Timestamp)
		if ts.IsZero() {
			ts = prevTS // missing or malformed: the previous record's time
		}
		prevTS = ts
		turns, tools := codexTurns(it, l, ts)
		if len(turns) == 0 {
			continue
		}
		before(ctx.cwd == "")
		rec := transcript.Record{UUID: lineID(raw), CWD: ctx.cwd, Timestamp: ts, Start: start, End: off, Turns: turns, Tools: tools}
		if ctx.hasMeta {
			rec.SessionID, rec.Entrypoint = ctx.meta.ID, ctx.meta.Originator
		}
		recs = append(recs, rec)
	}
}

// lineID is a record's id: a UUID-shaped hash of its line.
func lineID(line []byte) string {
	s := sha256.Sum256(line)
	x := hex.EncodeToString(s[:16])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

func parseCodexTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// codexTurns are the turns and tool calls of one response item.
func codexTurns(it codexItem, l rolloutLine, ts time.Time) ([]transcript.Turn, []transcript.ToolUse) {
	switch it.Type {
	case "message":
		var text string
		switch it.Role {
		case "user":
			text = userText(it)
		case "assistant":
			if l.Metadata.CompactionOutput {
				return nil, nil // the summary a compaction wrote, not a reply
			}
			text = joinText(it.Content, nil)
		default:
			return nil, nil // developer and system messages: instructions, hook context
		}
		if text = strings.TrimSpace(text); text == "" {
			return nil, nil
		}
		return []transcript.Turn{{Role: it.Role, Content: text, Timestamp: ts}}, nil
	case "function_call", "custom_tool_call", "local_shell_call", "web_search_call":
		tu, ok := codexTool(it)
		if !ok {
			return nil, nil
		}
		return []transcript.Turn{{Role: "action", Content: codexAction(tu), Timestamp: ts}}, []transcript.ToolUse{tu}
	}
	return nil, nil
}

// userText is a user message's text: its items Codex marks as the user's
// (provenance "user") when it marks them, else those that are not one of
// Codex's own blocks.
func userText(it codexItem) string {
	var items []contentItem
	if json.Unmarshal(it.Content, &items) != nil {
		var s string
		if json.Unmarshal(it.Content, &s) == nil && !codexBlock(s) {
			return s
		}
		return ""
	}
	marked := len(it.Meta.Metadata) == len(items) && len(items) > 0
	return joinText(it.Content, func(i int, text string) bool {
		if marked {
			return it.Meta.Metadata[i].Provenance.Type == "user"
		}
		return !codexBlock(text)
	})
}

// joinText joins the text items of content kept by keep (nil keeps all).
func joinText(content json.RawMessage, keep func(i int, text string) bool) string {
	var items []contentItem
	if json.Unmarshal(content, &items) != nil {
		return ""
	}
	var texts []string
	for i, c := range items {
		switch c.Type {
		case "input_text", "output_text", "text":
		default:
			continue
		}
		if keep != nil && !keep(i, c.Text) {
			continue
		}
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, "\n")
}

// codexBlocks are the blocks Codex injects as user messages, by their
// opening (what a rollout without provenance still tells apart).
var codexBlocks = []string{"<environment_context>", "<user_instructions>", "<subagent_notification>", "<turn_aborted>",
	"<user_shell_command>", "<INSTRUCTIONS>", "<skill>", "<permissions instructions>", "<collaboration_mode>",
	"# AGENTS.md instructions for "}

func codexBlock(text string) bool {
	t := strings.TrimSpace(text)
	for _, b := range codexBlocks {
		if strings.HasPrefix(t, b) {
			return true
		}
	}
	return false
}

// codexTool is a tool call as backsight reads it: its name (an MCP tool's
// with its namespace, mcp__<server>__<tool>), its id, and its input as an
// object, a shell command array joined into one string.
func codexTool(it codexItem) (transcript.ToolUse, bool) {
	tu := transcript.ToolUse{ID: it.CallID, Name: it.Name, Input: map[string]json.RawMessage{}}
	if tu.ID == "" {
		tu.ID = it.ID
	}
	if strings.HasPrefix(it.Namespace, "mcp__") && it.Name != "" {
		tu.Name = it.Namespace + "__" + it.Name
	}
	switch it.Type {
	case "function_call":
		var args string
		if json.Unmarshal(it.Arguments, &args) == nil {
			if json.Unmarshal([]byte(args), &tu.Input) != nil || tu.Input == nil {
				tu.Input = map[string]json.RawMessage{"input": mustJSON(args)}
			}
		} else if json.Unmarshal(it.Arguments, &tu.Input) != nil || tu.Input == nil {
			tu.Input = map[string]json.RawMessage{}
		}
	case "custom_tool_call":
		var in string
		_ = json.Unmarshal(it.Input, &in)
		tu.Input["input"] = mustJSON(in)
	case "local_shell_call":
		tu.Name = "local_shell"
		var a struct {
			Command          json.RawMessage `json:"command"`
			WorkingDirectory string          `json:"working_directory"`
		}
		if json.Unmarshal(it.Action, &a) != nil || len(a.Command) == 0 || string(a.Command) == "null" {
			return tu, false
		}
		tu.Input["command"] = a.Command
		if a.WorkingDirectory != "" {
			tu.Input["workdir"] = mustJSON(a.WorkingDirectory)
		}
	case "web_search_call":
		tu.Name = "web_search"
		var a struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(it.Action, &a)
		if a.Query != "" {
			tu.Input["query"] = mustJSON(a.Query)
		}
	}
	if tu.Name == "" {
		return tu, false
	}
	if cmd, ok := commandArray(tu.Input["command"]); ok {
		tu.Input["command"] = mustJSON(cmd)
	}
	return tu, true
}

// commandArray joins a command given as an argument array (["bash", "-lc",
// "<script>"] is its script).
func commandArray(raw json.RawMessage) (string, bool) {
	var argv []string
	if len(raw) == 0 || json.Unmarshal(raw, &argv) != nil || len(argv) == 0 {
		return "", false
	}
	if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
		return argv[2], true
	}
	return strings.Join(argv, " "), true
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

// codexTargetKeys: the input that names what a call acts on, first one
// set wins.
var codexTargetKeys = []string{"cmd", "command", "file_path", "path", "query", "queries", "url", "input", "message", "name", "id"}

// codexAction is "<tool> <target>": its target the first string among
// codexTargetKeys (a patch: its first file), first line only, redacted,
// at most 100 characters.
func codexAction(tu transcript.ToolUse) string {
	for _, k := range codexTargetKeys {
		v := ""
		if json.Unmarshal(tu.Input[k], &v) != nil {
			var list []string
			if json.Unmarshal(tu.Input[k], &list) != nil || len(list) == 0 {
				continue
			}
			v = list[0]
		}
		if k == "input" {
			if files := attribution.PatchFiles(v); len(files) > 0 {
				v = files[0]
			}
		}
		v, _, _ = strings.Cut(strings.TrimSpace(v), "\n")
		if v == "" {
			continue
		}
		// Redact before cutting, as Claude Code's actions are.
		v, _ = redact.Text(v)
		if r := []rune(v); len(r) > 100 {
			v = string(r[:100])
		}
		return tu.Name + " " + v
	}
	return tu.Name
}

// readRolloutLine is the next complete line of br; a line longer than
// rolloutLineMax comes back as tooLong with its length, its bytes
// discarded.
func readRolloutLine(br *bufio.Reader) (line []byte, n int64, tooLong bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		if !tooLong {
			if int64(len(line))+int64(len(chunk)) > int64(rolloutLineMax) {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, n, tooLong, err
	}
}
