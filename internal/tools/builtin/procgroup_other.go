//go:build !unix

package builtin

import "os/exec"

// bindProcessGroup has no process groups to use here; it still bounds Wait so
// a descendant holding the output cannot block the call. Releases are built
// for darwin and linux only.
func bindProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = hostChildWaitDelay
}
