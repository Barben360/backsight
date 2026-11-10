//go:build windows

package splash

import "golang.org/x/sys/windows"

// consoleOutputCP is the console's output code page; tests replace it.
var consoleOutputCP = windows.GetConsoleOutputCP

// UTF8 says whether the console takes UTF-8: its output code page is
// 65001. The locale variables do not apply to a Windows console.
func UTF8(func(string) string) bool {
	cp, err := consoleOutputCP()
	return err == nil && cp == 65001
}
