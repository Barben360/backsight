// Package docs embeds backsight's user documentation, the Markdown files
// of this directory (decision 0069): the binary's `guide` shows and
// searches the same files GitHub publishes.
package docs

import "embed"

// FS holds every docs/*.md file at its base name.
//
//go:embed *.md
var FS embed.FS
