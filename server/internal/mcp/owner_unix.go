//go:build unix

package mcp

import (
	"os"
	"syscall"
)

// dirOwnedPrivate reports whether the directory is owned by the current user with mode 0700.
func dirOwnedPrivate(fi os.FileInfo) bool {
	return fi.Mode().Perm() == 0o700 && ownedByMe(fi)
}

// ownedByMe reports whether the file belongs to the current user.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
