//go:build !linux && !darwin

package engine

import (
	"context"
	"errors"
)

// ownListener cannot tell a port's owner here, so the host engine refuses
// every tunnel on this platform.
func ownListener(context.Context, int, int) error {
	return errors.New("this platform cannot tell who listens on a port")
}
