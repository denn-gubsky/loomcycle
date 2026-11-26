//go:build !unix

package rootbilly

import (
	"errors"

	"github.com/go-git/go-billy/v5"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
)

// New needs openat with O_NOFOLLOW, flock and futimes on a directory handle
// (rootbilly.go), which only unix hosts give. loomcycle releases for darwin
// and linux; elsewhere a caller refuses rather than fall back to path strings.
func New(*fsops.VolumeFS, string) (billy.Filesystem, error) {
	return nil, errors.New("git over a volume needs a unix host")
}
