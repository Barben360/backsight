package termstyle

import (
	"io"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// The console mode calls; tests replace them.
var (
	getConsoleMode = windows.GetConsoleMode
	setConsoleMode = windows.SetConsoleMode
)

// EnableVT turns on virtual-terminal processing on w's console, so the
// console reads escape sequences instead of printing them. ok is false
// when w is not a console (a file, a pipe, mintty) or the mode cannot be
// set: that stream then gets no color. The mode is process-wide and
// outlives the process, so restore sets the saved mode back (at most once).
func EnableVT(w io.Writer) (restore func(), ok bool) {
	noop := func() {}
	f, isFile := w.(*os.File)
	if !isFile {
		return noop, false
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := getConsoleMode(h, &mode); err != nil {
		return noop, false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return noop, true
	}
	if err := setConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return noop, false
	}
	var once sync.Once
	return func() { once.Do(func() { _ = setConsoleMode(h, mode) }) }, true
}
