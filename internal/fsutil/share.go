package fsutil

import (
	"io"
	"time"
)

// RetryFor bounds how long a Windows write, rename or remove retries while
// another process holds the file; tests shorten it. Unix never retries.
var RetryFor = 2 * time.Second

// RetryHook runs each time a Windows retry found the file held, before it
// waits to try again; tests (of the callers too) release the file there.
var RetryHook = func() {}

// ReadFileShared reads path through OpenShared, so a reader never stops a
// writer's rename over it on Windows.
func ReadFileShared(path string) ([]byte, error) {
	f, err := OpenShared(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}
