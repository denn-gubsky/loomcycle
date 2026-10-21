package builtin

import "golang.org/x/sys/unix"

// exchangePaths atomically swaps two directory entries in one syscall, so a
// path component flips between a directory and a symlink with no gap.
func exchangePaths(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
