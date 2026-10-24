package doctor

import "errors"

// FreeBytes is not implemented on Windows yet (validated in M7).
func FreeBytes(string) (uint64, error) {
	return 0, errors.New("free space check not implemented on Windows yet")
}
