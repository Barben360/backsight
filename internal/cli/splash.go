package cli

import (
	"bytes"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/splash"
	"github.com/Barben360/backsight/internal/termstyle"
)

// splashMinWidth: a narrower terminal gets no splash (decision 0084 D3).
const splashMinWidth = 72

// termWidth is a terminal's width in columns; tests replace it.
var termWidth = func(fd uintptr) (int, error) {
	w, _, err := term.GetSize(int(fd))
	return w, err
}

// buildPanel builds the splash's panel; tests replace it.
var buildPanel = splash.Build

// consoleUTF8 says whether the terminal takes UTF-8 (the locale on Unix,
// the console code page on Windows); tests replace it.
var consoleUTF8 = splash.UTF8

// splashWidth is the width to draw the splash at, or 0 for none: stdout
// is colored, a terminal itself (even --color=always prints no splash
// into a pipe) at least splashMinWidth wide, and not opted out.
func (e *env) splashWidth(stdout io.Writer) int {
	if e.noSplash || os.Getenv("BACKSIGHT_SPLASH") == "0" || termstyle.LevelOf(e.stdout) == termstyle.None {
		return 0
	}
	f, ok := stdout.(*os.File)
	if !ok || !isTerminalFd(f.Fd()) {
		return 0
	}
	w, err := termWidth(f.Fd())
	if err != nil || w < splashMinWidth {
		return 0
	}
	return w
}

// printSplash writes the splash to raw, ahead of a full command's first
// stdout byte. It never fails the command: a panic or an error drops it,
// and it is written whole or not at all.
func (e *env) printSplash(raw, stdout io.Writer) {
	defer func() { _ = recover() }()
	width := e.splashWidth(stdout)
	if width == 0 {
		return
	}
	// paths.Default, never machine(): the splash creates nothing.
	d, err := paths.Default()
	if err != nil {
		return
	}
	utf8 := consoleUTF8(os.Getenv)
	panel, kind := buildPanel(splash.Sources{Dirs: d, Now: now(), Version: buildinfo.Version, Hindsight: server.PinnedVersion(),
		ASCII: !utf8, Mask: maskTokenText})
	var b bytes.Buffer
	splash.Render(&b, termstyle.LevelOf(e.stdout), width, !utf8, splash.Logo(kind, utf8), panel)
	_, _ = raw.Write(b.Bytes())
}
