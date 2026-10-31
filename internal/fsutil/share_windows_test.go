//go:build windows

package fsutil

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shortRetry(t *testing.T, d time.Duration) {
	old := RetryFor
	RetryFor = d
	t.Cleanup(func() { RetryFor = old })
}

// A reader that opened the file through OpenShared does not stop a write,
// and keeps reading the bytes it opened.
func TestWriteAtomicWhileAReaderHoldsTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := WriteAtomic(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenShared(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	shortRetry(t, 300*time.Millisecond)
	if err := WriteAtomic(p, []byte("new"), 0o600); err != nil {
		t.Fatalf("write over a shared reader: %v", err)
	}
	if b, _ := io.ReadAll(r); string(b) != "old" {
		t.Errorf("the reader sees %q, want the bytes it opened", b)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Errorf("the file holds %q", b)
	}
}

// A plain os.Open has no FILE_SHARE_DELETE: the rename waits for it, within
// RetryFor, and fails naming the path when the reader stays.
func TestWriteAtomicWaitsForAPlainReader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := WriteAtomic(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	shortRetry(t, 3*time.Second)
	r, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(400 * time.Millisecond); _ = r.Close() }()
	start := time.Now()
	if err := WriteAtomic(p, []byte("new"), 0o600); err != nil {
		t.Fatalf("write after the reader closed: %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Error("the write did not wait for the reader")
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Errorf("the file holds %q", b)
	}

	shortRetry(t, 200*time.Millisecond)
	r, err = os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	err = WriteAtomic(p, []byte("newer"), 0o600)
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("WriteAtomic with a reader that stays = %v, want an error naming %s", err, p)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Errorf("a failed write changed the file to %q", b)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".state.json.tmp-*"))
	if len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestReplaceIfUnchangedRetriesToo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := WriteAtomic(p, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, h, _ := ReadHashed(p)
	shortRetry(t, 3*time.Second)
	r, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(300 * time.Millisecond); _ = r.Close() }()
	if err := ReplaceIfUnchanged(p, []byte("b"), 0o600, h); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveRetryWaitsForAPlainReader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.exe")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	shortRetry(t, 3*time.Second)
	r, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(300 * time.Millisecond); _ = r.Close() }()
	if err := RemoveRetry(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("the file is still there")
	}
	shortRetry(t, 150*time.Millisecond)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ = os.Open(p)
	defer func() { _ = r.Close() }()
	if err := RemoveRetry(p); err == nil {
		t.Error("RemoveRetry of a held file succeeded")
	}
}

// OpenShared of an absent file is a not-exist error, as os.Open's.
func TestOpenSharedAbsent(t *testing.T) {
	_, err := OpenShared(filepath.Join(t.TempDir(), "absent"))
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want not-exist", err)
	}
}
