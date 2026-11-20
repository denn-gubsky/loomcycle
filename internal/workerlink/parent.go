package workerlink

import (
	"context"
	"os"
	"time"
)

// parentEvery is how often WatchParent looks at the parent pid.
const parentEvery = time.Second

// WatchParent calls cancel once this process's parent is no longer pid (the
// parent died and the reaper adopted this process), and returns; it returns
// without calling cancel when ctx ends first. A worker the core started as
// its child (`--parent-pid`) runs it, so a core that is killed outright does
// not leave its workers behind.
func WatchParent(ctx context.Context, pid int, cancel context.CancelFunc) {
	watchParent(ctx, pid, cancel, os.Getppid, parentEvery)
}

func watchParent(ctx context.Context, pid int, cancel context.CancelFunc, getppid func() int, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if getppid() != pid {
				cancel()
				return
			}
		}
	}
}
