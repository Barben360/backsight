package termstyle

import (
	"io"
	"regexp"
	"sync"
)

const reset = "\x1b[0m"

// Writer is an output stream that knows its color level. It passes every
// byte through unchanged; the level is resolved once, on first use, so
// that cobra has parsed --json and --color by then.
type Writer struct {
	w       io.Writer
	resolve func() Level
	once    sync.Once
	level   Level
}

// NewWriter wraps w; resolve is called at most once, lazily.
func NewWriter(w io.Writer, resolve func() Level) *Writer {
	return &Writer{w: w, resolve: resolve}
}

func (w *Writer) Write(p []byte) (int, error) { return w.w.Write(p) }

// Level is the stream's color level, resolved on the first call.
func (w *Writer) Level() Level {
	w.once.Do(func() { w.level = w.resolve() })
	return w.level
}

// Unwrap is the wrapped writer.
func (w *Writer) Unwrap() io.Writer { return w.w }

// LevelOf is w's color level: None for any writer that is not a *Writer.
func LevelOf(w io.Writer) Level {
	if sw, ok := w.(*Writer); ok {
		return sw.Level()
	}
	return None
}

// Raw is the writer under any *Writer wrapping, for a child process:
// os/exec turns a non-*os.File writer into a pipe, and a pager on a pipe
// stops paging.
func Raw(w io.Writer) io.Writer {
	for {
		sw, ok := w.(*Writer)
		if !ok {
			return w
		}
		w = sw.w
	}
}

// Paint wraps s in role r's color for w's level, or returns s unchanged
// when w has no color or s is empty. Only SGR sequences are added, so
// Strip(Paint(w, r, s)) == s.
func Paint(w io.Writer, r Role, s string) string {
	if s == "" {
		return s
	}
	start := sgr(r, LevelOf(w))
	if start == "" {
		return s
	}
	return start + s + reset
}

var sgrRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Strip removes SGR sequences: what the text is without color.
func Strip(s string) string { return sgrRe.ReplaceAllString(s, "") }
