//go:build !unix

package builtin

import (
	"errors"

	"github.com/go-git/go-billy/v5"
)

// newVolumeBilly needs openat with O_NOFOLLOW, flock and futimes on a
// directory handle (git_billy.go), which only unix hosts give. loomcycle
// releases for darwin and linux; elsewhere the Git tool refuses rather than
// fall back to path strings.
func newVolumeBilly(*volumeFS, string) (billy.Filesystem, error) {
	return nil, errors.New("the Git tool needs a unix host")
}
