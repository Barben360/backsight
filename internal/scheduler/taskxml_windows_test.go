//go:build windows

package scheduler

import (
	"slices"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var argSamples = []string{"", "plain", `C:\x\current.exe`, `C:\Jé & Co\a.exe`, "a\tb", `say "hi"`, `C:\dir with sp\`,
	`back\\"quote`, `trailing\`, `two words\\`, `q"`, `with"space and\\"`, `\\server\share\x y\`, `"`, `\`, `\"`}

// EscapeArg is syscall.EscapeArg, and CommandLineToArgv splits the
// arguments of a task back into exactly what was given.
func TestEscapeArgIsWindowsOwn(t *testing.T) {
	for _, s := range argSamples {
		if got, want := EscapeArg(s), syscall.EscapeArg(s); got != want {
			t.Errorf("EscapeArg(%q) = %s, syscall.EscapeArg = %s", s, got, want)
		}
	}
	// A task carries no control character: the tab stays out.
	args := slices.DeleteFunc(slices.Clone(argSamples), func(s string) bool { return s == "a\tb" })
	raw, err := TaskXML(serverSpec(`C:\Users\Jé & Co\bin\current.exe`, args...))
	if err != nil {
		t.Fatal(err)
	}
	got := splitArgs(t, "conhost.exe "+parseTask(t, raw).Arguments)[1:]
	want := HeadlessArgs(`C:\Users\Jé & Co\bin\current.exe`, args...)
	if !slices.Equal(got, want) {
		t.Errorf("the arguments split back as\n%q\nwant\n%q", got, want)
	}
}

// splitArgs is CommandLineToArgv: how a Windows program reads its command
// line.
func splitArgs(t *testing.T, line string) []string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(line)
	if err != nil {
		t.Fatal(err)
	}
	var n int32
	argv, err := windows.CommandLineToArgv(p, &n)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(argv)))) }()
	out := make([]string, n)
	for i := range out {
		out[i] = windows.UTF16PtrToString(&argv[i][0])
	}
	return out
}
