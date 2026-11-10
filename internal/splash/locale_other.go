//go:build !windows

package splash

// UTF8 says whether the terminal takes UTF-8 (the locale, on Unix).
func UTF8(getenv func(string) string) bool { return localeUTF8(getenv) }
