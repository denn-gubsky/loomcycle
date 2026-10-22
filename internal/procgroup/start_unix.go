//go:build unix && !linux

package procgroup

import (
	"os/exec"
	"syscall"
)

// StartInOwnGroup starts a long-lived child in its own process group. Only
// Linux can tie the child's life to loomcycle's (Pdeathsig); elsewhere a
// loomcycle that dies without stopping the child leaves it running, and a
// well-behaved MCP server still exits when its stdin closes.
func StartInOwnGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Start()
}
