//go:build linux

package worker

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// cloneDir is APFS's whole-folder clone, which Linux has no call for.
func cloneDir(_, _ string) error { return errors.ErrUnsupported }

// cloneFile makes dst share src's blocks (FICLONE): xfs with reflink,
// btrfs, bcachefs. A file system without it refuses, and the caller copies.
func cloneFile(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}

func cloneDirSupported(string) bool { return false }

// reflinkSupported reports whether FICLONE works in dir's file system.
func reflinkSupported(dir string) bool {
	src, err := os.CreateTemp(dir, "probe-")
	if err != nil {
		return false
	}
	defer os.Remove(src.Name())
	defer src.Close()
	if _, err := src.WriteString("probe"); err != nil {
		return false
	}
	dst, err := os.CreateTemp(dir, "probe-")
	if err != nil {
		return false
	}
	defer os.Remove(dst.Name())
	defer dst.Close()
	return cloneFile(dst, src) == nil
}
