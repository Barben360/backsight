// Package signing embeds the release public keys (decision 0027): the
// current key verifies releases, the next one lets a key rotation happen
// without a trust reset. Verification itself arrives with self-update (M5).
package signing

import _ "embed"

var (
	//go:embed keys/current.pub
	Current string
	//go:embed keys/next.pub
	Next string
)
