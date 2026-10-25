//go:build !testkey

package signing

// linkedTestKey: a build without the testkey tag trusts no test key.
func linkedTestKey() string { return "" }
