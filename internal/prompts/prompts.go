// Package prompts holds the default prompt files. On first run they are
// copied to <data>/prompts, where the dashboard edits them.
package prompts

import "embed"

//go:embed *.md
var Defaults embed.FS
