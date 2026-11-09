//go:build !windows

package termstyle

import "io"

// EnableVT: terminals outside Windows read escape sequences as they are.
func EnableVT(io.Writer) (restore func(), ok bool) { return func() {}, true }
