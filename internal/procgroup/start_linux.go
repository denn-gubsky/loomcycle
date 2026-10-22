//go:build linux

package procgroup

import (
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

// StartInOwnGroup starts a long-lived child in its own process group and asks
// the kernel to SIGKILL it when loomcycle dies (Pdeathsig), so a crashed
// loomcycle does not leave the child running. Pdeathsig reaches the child
// only, not what it started; KillGroup on a clean stop reaches those.
//
// Pdeathsig fires when the THREAD that started the child exits, not the
// process. Go may end an OS thread (one a goroutine locked and exited
// without unlocking), which would kill a healthy child. So every such child
// is started from one goroutine locked to its thread for the life of the
// process: that thread exits only when loomcycle does.
func StartInOwnGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	spawner.once.Do(func() {
		spawner.reqs = make(chan spawnRequest)
		go func() {
			runtime.LockOSThread() // never unlocked, on purpose: see above
			for r := range spawner.reqs {
				r.done <- r.cmd.Start()
			}
		}()
	})
	done := make(chan error, 1)
	spawner.reqs <- spawnRequest{cmd: cmd, done: done}
	return <-done
}

type spawnRequest struct {
	cmd  *exec.Cmd
	done chan error
}

var spawner struct {
	once sync.Once
	reqs chan spawnRequest
}
