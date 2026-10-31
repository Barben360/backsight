//go:build !windows

package report

// spellings: a Unix path has one.
func spellings(string) []string { return nil }
