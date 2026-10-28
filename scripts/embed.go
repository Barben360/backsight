// Package scripts holds the shell scripts shipped with backsight and their
// tests. Install is install.sh, embedded so that server install --ssh runs
// this version's script, with the key it carries, on the host.
package scripts

import _ "embed"

// Install is install.sh (SPEC §11.1).
//
//go:embed install.sh
var Install []byte
