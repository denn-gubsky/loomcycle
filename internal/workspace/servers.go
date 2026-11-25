package workspace

import "time"

// The server ops (RFC GW §5.3): a command that outlives its tool call, its
// end, and its output. The worker keeps a workspace's servers; they end with
// stop, with the workspace's removal and with the worker.
const (
	OpServerStart = "server_start"
	OpServerStop  = "server_stop"
	OpServerLogs  = "server_logs"
)

// Bounds on a workspace's servers.
const (
	// MaxServers is how many servers one workspace runs at once.
	MaxServers = 4
	// ServerLogBytes is how much of a server's latest output the worker
	// keeps.
	ServerLogBytes = 256 << 10
	// MaxServerCommand bounds a server's command line.
	MaxServerCommand = 16 << 10
)

// ClassServerLimit: the workspace runs MaxServers servers already.
const ClassServerLimit = "server_limit"

// ClassServerNotFound: no server of that id in the workspace.
const ClassServerNotFound = "server_not_found"

// ServerStartArgs are a server_start op's arguments. Port is the port the
// agent says the server listens on: recorded, not checked.
type ServerStartArgs struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
	Port    int    `json:"port,omitempty"`
}

// ServerStopArgs are a server_stop op's arguments.
type ServerStopArgs struct {
	Server string `json:"server"`
}

// ServerLogsArgs are a server_logs op's arguments. With no Server every
// server is listed without its output.
type ServerLogsArgs struct {
	Server    string `json:"server,omitempty"`
	TailBytes int    `json:"tail_bytes,omitempty"`
}

// ServerInfo is one server as start, stop and logs report it. Output is
// its latest output (logs, and start when it ended at once); Truncated says
// earlier output was dropped or the tail was cut.
type ServerInfo struct {
	Server    string    `json:"server"`
	Command   string    `json:"command"`
	Port      int       `json:"port,omitempty"`
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	Output    *string   `json:"output,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
}

// ServerStopAnswer is a server_stop op's output: Stopped is false for a
// server that had ended by itself.
type ServerStopAnswer struct {
	Server  string `json:"server"`
	Stopped bool   `json:"stopped"`
}

// ServerLogsAnswer is a server_logs op's output.
type ServerLogsAnswer struct {
	Servers []ServerInfo `json:"servers"`
}

// OpTunnel is a byte stream to a port a server of the workspace serves (RFC
// GW §5.3, P9 plan §3.2): the op's input is written to the port as it comes
// and what the port answers is the op's output, both at once. The worker
// admits it (an empty output chunk) once the port is reached; its input is
// of unknown length.
const OpTunnel = "tunnel"

// TunnelArgs are a tunnel op's arguments.
type TunnelArgs struct {
	Port int `json:"port"`
}

// ClassPortNotServed: no running server of the workspace declared the port
// (or, on the host engine, a server of another workspace on the worker
// declares it too).
const ClassPortNotServed = "port_not_served"

// ClassPortUnreachable: a server declared the port but nothing answers on
// it in the workspace.
const ClassPortUnreachable = "port_unreachable"

// MaxRequestBody bounds a request op's body each way: what is sent, and
// what of the answer's body is returned (RFC GW P9 plan §3.2).
const MaxRequestBody = 1 << 20

// OpAwait waits for the next update the workspace's application posted to
// the worker (RFC GW §5.3, P9 plan §3.4): the agent learns what a person
// did in its page. A server finds where to post in UpdateURLEnv: on the
// host engine a loopback address with a secret path, in a container
// session an address only that session reaches.
const OpAwait = "await"

// UpdateURLEnv names the variable a workspace's servers find the update
// address in.
const UpdateURLEnv = "LOOMCYCLE_UPDATE_URL"

// Bounds on a workspace's updates: a server cannot flood the run.
const (
	// MaxUpdates is how many updates wait for await at once; a post past
	// it is refused (429) and counted as dropped.
	MaxUpdates = 64
	// MaxUpdateBytes bounds one update's body; a larger post is refused
	// (413).
	MaxUpdateBytes = 64 << 10
)

// MaxAwaitSeconds bounds one await's wait: an op stream of the worker is
// held for it.
const MaxAwaitSeconds = 3600

// AwaitArgs are an await op's arguments: how long to wait for an update.
type AwaitArgs struct {
	TimeoutMS int64 `json:"timeout_ms"`
}

// AwaitAnswer is an await op's output: the next update, as text when it is
// UTF-8 and otherwise in base64, or TimedOut. Pending is how many more
// wait; Dropped how many were refused as over the bound since the last
// await that reported them.
type AwaitAnswer struct {
	Update       *string `json:"update,omitempty"`
	UpdateBase64 *string `json:"update_base64,omitempty"`
	TimedOut     bool    `json:"timed_out,omitempty"`
	Pending      int     `json:"pending"`
	Dropped      int     `json:"dropped,omitempty"`
}
