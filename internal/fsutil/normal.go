package fsutil

// Normal is p in the one spelling paths are compared in: cleaned, and on
// Windows with long names for the parts that exist (C:\Users\RUNNER~1 is
// C:\Users\runneradmin), forward slashes as backslashes (git's C:/x is
// C:\x) and an upper-case drive letter. On Unix it is filepath.Clean. An
// empty path stays empty.
func Normal(p string) string {
	if p == "" {
		return ""
	}
	return normal(p)
}

// SamePath says whether a and b name one path: equal strings on Unix; on
// Windows equal once Normal, whatever their case.
func SamePath(a, b string) bool {
	if a == b {
		return true
	}
	return samePath(a, b)
}
