package builtin

import "time"

// hostChildWaitDelay bounds how long Wait keeps reading a host child's output
// after the child exits or its context is done. A descendant that inherits
// stdout (`server &`, or one that left the process group with setsid) would
// otherwise hold the pipe open and block the tool call until it exits.
const hostChildWaitDelay = 2 * time.Second
