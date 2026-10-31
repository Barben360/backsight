//go:build !windows

package cli

import "errors"

// pointShimWindows is only reached on Windows; the symlink swap is the
// other systems' shim.
func pointShimWindows(target, shim string) error {
	return errors.New("the copied shim is a Windows form")
}
