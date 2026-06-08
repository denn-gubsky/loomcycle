//go:build !windows

package anthropic_oauth_dev

import (
	"testing"
	"time"
)

// acquireFileLock must serialize across open file descriptions: a second
// acquire BLOCKS while the first is held, then succeeds once it is released.
// (flock is per-open-file-description, so this holds even within one process.)
func TestAcquireFileLock_Serializes(t *testing.T) {
	lockPath := t.TempDir() + "/tok.lock"

	release1, err := acquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		r2, err := acquireFileLock(lockPath)
		if err != nil {
			return
		}
		close(acquired)
		r2()
	}()

	// While we hold the lock, the goroutine must not acquire it.
	select {
	case <-acquired:
		t.Fatal("second acquire succeeded while the first lock was held")
	case <-time.After(150 * time.Millisecond):
	}

	release1()

	// After release, the goroutine acquires promptly.
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire did not succeed after release")
	}
}
