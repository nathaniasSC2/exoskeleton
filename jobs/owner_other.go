//go:build !unix

package jobs

import "io/fs"

// fileOwner can't tell off Unix, so scratch directories are never removed there.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
