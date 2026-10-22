// Package procgroup starts host child processes in their own process group,
// so loomcycle can stop a child together with everything it started.
//
// Two kinds of child use it. A short-lived command (the Bash tool, a Bashbox
// host-fallback command) is bound with Bind: a context cancel or timeout
// kills its whole group, and WaitDelay stops a descendant holding the output
// from blocking Wait. A long-lived server (an MCP stdio server) is started
// with StartInOwnGroup and stopped with KillGroup; on Linux it also dies
// when loomcycle does.
//
// A descendant that calls setsid() leaves the group and survives a group
// kill. That is the documented "not a sandbox" residual; WaitDelay and the
// MCP client's bounded Close keep it from holding a call open.
package procgroup

import "time"

// WaitDelay bounds how long Wait keeps reading a host child's output after
// the child exits or its context is done. A descendant that inherits stdout
// (`server &`, or one that left the process group with setsid) would
// otherwise hold the pipe open and block the call until it exits.
const WaitDelay = 2 * time.Second
