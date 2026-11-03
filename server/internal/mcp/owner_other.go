//go:build !unix

package mcp

import "os"

// dirOwnedPrivate: no uid or Unix mode here; the symlink and directory checks remain.
func dirOwnedPrivate(os.FileInfo) bool { return true }
