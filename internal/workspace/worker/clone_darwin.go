//go:build darwin

package worker

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// cloneDir clones the folder src whole to dst (APFS clonefile): every file
// shares its blocks with the source until either side is written. Links
// inside are cloned as links; CLONE_NOFOLLOW keeps src itself from being
// one followed.
func cloneDir(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}

// cloneFile is FICLONE's place on Linux; on macOS a tree is cloned whole.
func cloneFile(_, _ *os.File) error { return errors.ErrUnsupported }

// cloneDirSupported reports whether cloneDir works in dir's file system.
func cloneDirSupported(dir string) bool {
	src, err := os.MkdirTemp(dir, "probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(src)
	if err := os.WriteFile(src+"/f", []byte("probe"), 0o600); err != nil {
		return false
	}
	dst := src + "-clone"
	defer os.RemoveAll(dst)
	return cloneDir(src, dst) == nil
}

// reflinkSupported is Linux's.
func reflinkSupported(string) bool { return false }
