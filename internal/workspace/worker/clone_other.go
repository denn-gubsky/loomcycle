//go:build !darwin && !linux

package worker

import (
	"errors"
	"os"
)

func cloneDir(_, _ string) error    { return errors.ErrUnsupported }
func cloneFile(_, _ *os.File) error { return errors.ErrUnsupported }
func cloneDirSupported(string) bool { return false }
func reflinkSupported(string) bool  { return false }
