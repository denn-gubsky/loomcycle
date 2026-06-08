//go:build !windows

package anthropic_oauth_dev

import (
	"fmt"
	"os"
	"syscall"
)

// acquireFileLock takes an exclusive advisory lock (flock LOCK_EX) on lockPath,
// creating it if absent, and BLOCKS until the lock is available. Refresh is rare
// and brief, so serializing it across processes (the whole point — F7) costs at
// most the holder's bounded HTTP timeout. The lock auto-releases when the fd is
// closed or the process dies, so a crash never strands it (unlike an
// O_EXCL-lockfile scheme, which needs stale-lock reaping).
//
// The fd is kept open inside the returned release closure — closing it earlier
// would drop the lock.
func acquireFileLock(lockPath string) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
