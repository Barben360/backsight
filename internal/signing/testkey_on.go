//go:build testkey

package signing

// LinkedTestKey is the test key a testkey build trusts, set with -X (make
// build TEST_KEY=…). A release has no testkey tag, so no such variable.
var LinkedTestKey string

func linkedTestKey() string { return LinkedTestKey }
