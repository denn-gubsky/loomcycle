//go:build unix

package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Bind puts cmd in its own process group and makes a context cancel or
// timeout SIGKILL the whole group, so the command's background descendants
// die with it. A normal exit kills nothing: a `server &` the command started
// keeps running, and WaitDelay stops it from holding the call open.
func Bind(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Cancel can run just after exec reaps the child, when the context
		// ends between the reap and Wait taking the watcher's result. While
		// any member lives the id still names this group: the kernel does
		// not reuse a pid while a group of that id exists. Signalling a
		// stranger needs the group to have emptied and the pid to be
		// reused as a new group's id inside that microsecond window; the
		// usual outcome is ESRCH, reported as done so a command that exited
		// cleanly just before the deadline keeps its success status.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = WaitDelay
}

// KillGroup sends SIGKILL to cmd's process group. With Setpgid the group id
// is the child's pid. A group that is already gone is not an error.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
