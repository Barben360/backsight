package supervise

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/Barben360/backsight/internal/redact"
)

// maxLog is where a log rotates to <name>.1 (0080's layout: 10 MiB).
const maxLog = 10 << 20

// rotating is a log file shared by the writers of one log (a child's
// successive runs), rotated to <path>.1 before a write would take it past
// maxLog.
type rotating struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	// retryAt is the size at which a rotation that failed (Windows: the
	// file held by a reader without FILE_SHARE_DELETE) is tried again.
	retryAt int64
}

func openRotating(path string) (*rotating, error) {
	r := &rotating{path: path}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotating) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

func (r *rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > maxLog && r.size >= r.retryAt {
		r.rotate()
		if r.f == nil {
			return 0, os.ErrClosed
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate moves the log to <path>.1 (the older .1 dropped) and opens a new
// one; on failure it keeps appending and tries again 1 MiB later.
func (r *rotating) rotate() {
	_ = r.f.Close()
	r.f = nil
	old := r.path + ".1"
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.retryAt = r.size + 1<<20
	} else if err := os.Rename(r.path, old); err != nil {
		r.retryAt = r.size + 1<<20
	} else {
		r.retryAt = 0
	}
	if err := r.open(); err != nil {
		r.f = nil
	}
}

func (r *rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// lineRedactor writes a child's output to its log line by line, each line
// with the known secrets' forms replaced (redact.Forms, as redact.Values):
// a secret is never cut in two by a write boundary. A line longer than
// maxLine is redacted whole, then written but for its last keep bytes,
// which wait for what follows: a form cut by the end of the buffer is
// shorter than keep, so it is held back whole, and a form wholly inside
// the buffer is already replaced. No search for a cut, so no walk-back a
// periodic secret could drive.
type lineRedactor struct {
	dst   io.Writer
	forms [][]byte
	keep  int // keepTail, or the longest form if longer
	buf   []byte
}

const (
	maxLine  = 64 << 10
	keepTail = 1 << 10
)

func newLineRedactor(dst io.Writer, secrets []string) *lineRedactor {
	l := &lineRedactor{dst: dst, keep: keepTail}
	for _, f := range redact.Forms(secrets) {
		l.forms = append(l.forms, []byte(f))
		l.keep = max(l.keep, len(f))
	}
	return l
}

func (l *lineRedactor) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.emit(l.buf[:i+1])
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > maxLine {
		red := l.redacted(l.buf)
		cut := len(red) - l.keep
		_, _ = l.dst.Write(red[:cut])
		l.buf = append([]byte{}, red[cut:]...)
	}
	return len(p), nil
}

func (l *lineRedactor) redacted(b []byte) []byte {
	for _, f := range l.forms {
		b = bytes.ReplaceAll(b, f, []byte("[redacted]"))
	}
	return b
}

func (l *lineRedactor) emit(b []byte) { _, _ = l.dst.Write(l.redacted(b)) }

// Close writes what is left (a last line without a newline).
func (l *lineRedactor) Close() error {
	if len(l.buf) > 0 {
		l.emit(l.buf)
		l.buf = nil
	}
	return nil
}
