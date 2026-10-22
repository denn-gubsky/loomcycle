//go:build !unix

package procgroup

import "os/exec"

// Releases are built for darwin and linux only. These keep the package
// building elsewhere: no process groups, so only the direct child is
// stopped, and WaitDelay still bounds Wait.

// Bind bounds Wait so a descendant holding the output cannot block the call.
func Bind(cmd *exec.Cmd) { cmd.WaitDelay = WaitDelay }

// StartInOwnGroup starts cmd.
func StartInOwnGroup(cmd *exec.Cmd) error { return cmd.Start() }

// KillGroup kills the direct child.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
