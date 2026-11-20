//go:build unix

package fsops

import "syscall"

// nonBlock opens without waiting: a FIFO opened for reading would otherwise
// wait for a writer. It changes nothing for a regular file or a directory.
const nonBlock = syscall.O_NONBLOCK
