package retain

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gzStore keeps Claude Code-format records gzipped, as rollout-<sid>.gz:
// a harness whose files are neither named after their session nor their
// own content stream.
type gzStore struct{ claudeCode }

func (gzStore) SessionOf(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "rollout-"), ".gz")
}

func gunzip(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

func (gzStore) Size(path string) (int64, error) {
	raw, err := gunzip(path)
	return int64(len(raw)), err
}

func (gzStore) Open(path string, from int64) (io.ReadCloser, error) {
	raw, err := gunzip(path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(raw[min(from, int64(len(raw))):])), nil
}

// archive gzips sid's transcript into the store's file, which it returns.
func (e *env) archive(sid string) string {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.dir, sid+".jsonl"))
	if err != nil {
		e.t.Fatal(err)
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	p := filepath.Join(e.dir, "rollout-"+sid+".gz")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// The flusher keys its cursor by the harness's session id and counts in
// the content stream: its offset, its fingerprint (the stream's first
// bytes) and the size it compares the offset with. A compressed file
// smaller than its content never reads as shrunk: an unchanged session is
// not read again from the server.
func TestTheFlusherReadsThroughItsHarness(t *testing.T) {
	e := newEnv(t)
	e.f.Harness = gzStore{}
	long := strings.Repeat("The export pages by 1000 rows. ", 40) // compresses well below its size
	e.record("s1", "u1", "user", 0, long)
	path := e.archive("s1")
	expect(t, e.f.FlushFile(context.Background(), path, Options{}), Sent, 1, 1)
	content, _ := gunzip(path)
	cur, ok, err := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	if err != nil || !ok || cur.Offset != int64(len(content)) {
		t.Fatalf("cursor %+v %v %v, want offset %d", cur, ok, err, len(content))
	}
	sum := sha256.Sum256(content[:min(len(content), fingerprintMax)])
	if cur.Fingerprint != hex.EncodeToString(sum[:]) {
		t.Errorf("fingerprint %s is not the content's first bytes'", cur.Fingerprint)
	}
	if fi, _ := os.Stat(path); fi.Size() >= cur.Offset {
		t.Fatalf("the test needs a file smaller than its content: %d >= %d", fi.Size(), cur.Offset)
	}
	if !e.f.CaughtUp(path) {
		t.Error("CaughtUp does not find the session's cursor")
	}
	gets := e.hs.gets
	if r := e.f.FlushFile(context.Background(), path, Options{}); r.Status != Nothing || e.hs.gets != gets {
		t.Errorf("an unchanged session: %s (%s), %d document reads", r.Status, r.Detail, e.hs.gets-gets)
	}
	e.record("s1", "u2", "user", 60, "second")
	path = e.archive("s1")
	expect(t, e.f.FlushFile(context.Background(), path, Options{}), Sent, 1, 2)
	if it := e.hs.retains[len(e.hs.retains)-1]; strings.Contains(it.Content, "pages by 1000") || it.UpdateMode != "append" {
		t.Errorf("the append resent the start: %+v", it)
	}
}
