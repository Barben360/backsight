package splash

import (
	"bufio"
	"errors"
	"io"

	"github.com/Barben360/backsight/internal/fsutil"
)

// Size caps (decision 0084 D4): a file past its cap is not read.
const (
	fileCap     = 64 << 10
	snapshotCap = 1 << 20
	outboxMax   = 1000
	outboxBytes = 8 << 20
)

var errTooBig = errors.New("over its size cap")

// readFile reads path, refusing more than limit bytes. No lock, no write;
// opened shared so a writer on Windows is never blocked. Tests replace it.
var readFile = func(path string, limit int64) ([]byte, error) {
	f, err := fsutil.OpenShared(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errTooBig
	}
	return b, nil
}

// countLines counts path's lines without decoding them, stopping at max
// lines or outboxBytes bytes (more: there may be others). Absent: 0.
func countLines(path string, max int) (n int, more bool, err error) {
	f, err := fsutil.OpenShared(path)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(io.LimitReader(f, outboxBytes))
	read := 0
	for {
		chunk, err := r.ReadSlice('\n')
		read += len(chunk)
		if err == nil {
			if n++; n >= max {
				_, perr := r.Peek(1)
				return n, perr == nil, nil
			}
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return n, false, err
		}
		if len(chunk) > 0 {
			n++
		}
		return n, read >= outboxBytes, nil
	}
}
