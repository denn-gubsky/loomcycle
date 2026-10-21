//go:build unix

package builtin

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// bindProcessGroup puts cmd in its own process group and makes a context
// cancel or timeout SIGKILL the whole group, so the command's background
// descendants die with it. A normal exit kills nothing: a `server &` the
// command started keeps running, and hostChildWaitDelay stops it from holding
// the call open. A descendant that calls setsid() leaves the group and
// survives a cancel too; that is the documented "not a sandbox" residual.
func bindProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// With Setpgid the group id is the child's pid. ESRCH means the whole
		// group is gone already; report it as done so a command that exited
		// cleanly just before the deadline keeps its success status.
		//
		// Cancel can run just after exec reaps the child, when the context
		// ends between the reap and Wait taking the watcher's result. While
		// any member lives the id still names this group: the kernel does
		// not reuse a pid while a group of that id exists. Signalling a
		// stranger needs the group to have emptied and the pid to be
		// reused as a new group's id inside that microsecond window; the
		// usual outcome is ESRCH.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = hostChildWaitDelay
}
