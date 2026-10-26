// Package licenses holds the license texts and notices of the third-party
// code linked into backsight's binary, generated from go.mod by
// scripts/third-party-licenses.sh (make licenses). Release archives ship
// the same file; bare binaries print it with `backsight version --licenses`.
package licenses

import _ "embed"

// Text is THIRD_PARTY_LICENSES, whole.
//
//go:embed THIRD_PARTY_LICENSES
var Text string
