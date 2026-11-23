package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// serverOp starts, stops or reads a workspace's server (RFC GW §5.3): a
// command the worker keeps running after the call. The worker keeps the
// servers; this side checks the arguments it can and passes them on.
func (w *Workspace) serverOp(ctx context.Context, row store.WorkspaceRow, in workspaceInput) tools.Result {
	var req dispatch.Request
	switch in.Op {
	case "start":
		if strings.TrimSpace(in.Command) == "" {
			return workspaceRefusal("invalid_argument", "start needs command")
		}
		if len(in.Command) > workspace.MaxServerCommand {
			return workspaceRefusal("invalid_argument", fmt.Sprintf("command is over %d bytes; write a script and start that", workspace.MaxServerCommand))
		}
		if in.Port < 0 || in.Port > 65535 {
			return workspaceRefusal("invalid_argument", "port must be from 1 to 65535")
		}
		args, _ := json.Marshal(workspace.ServerStartArgs{Command: in.Command, Cwd: in.Cwd, Port: in.Port})
		req = dispatch.Request{Op: workspace.OpServerStart, Args: args}
	case "stop":
		if in.Server == "" {
			return workspaceRefusal("invalid_argument", "stop needs server: the id start returned (logs lists them)")
		}
		args, _ := json.Marshal(workspace.ServerStopArgs{Server: in.Server})
		req = dispatch.Request{Op: workspace.OpServerStop, Args: args}
	case "logs":
		if in.TailBytes < 0 || in.TailBytes > workspace.ServerLogBytes {
			return workspaceRefusal("invalid_argument", fmt.Sprintf("tail_bytes must be from 1 to %d", workspace.ServerLogBytes))
		}
		if in.TailBytes > 0 && in.Server == "" {
			return workspaceRefusal("invalid_argument", "tail_bytes needs server: logs without server lists the servers, with no output")
		}
		args, _ := json.Marshal(workspace.ServerLogsArgs{Server: in.Server, TailBytes: in.TailBytes})
		req = dispatch.Request{Op: workspace.OpServerLogs, Args: args}
	}
	res, err := w.Manager.Do(ctx, row, req)
	if err != nil {
		return workspaceServerError(err, in)
	}
	var out any
	switch in.Op {
	case "start":
		var info workspace.ServerInfo
		if json.Unmarshal(res.Output, &info) != nil {
			return workspaceRefusal(workspace.ClassProtocol, "the worker's answer was not a server")
		}
		r := unescapedResult(info)
		// A server that ended at once failed to start, whatever it said.
		r.IsError = !info.Running
		return r
	case "stop":
		var sa workspace.ServerStopAnswer
		if json.Unmarshal(res.Output, &sa) != nil {
			return workspaceRefusal(workspace.ClassProtocol, "the worker's answer was not a stop result")
		}
		out = sa
	case "logs":
		var la workspace.ServerLogsAnswer
		if json.Unmarshal(res.Output, &la) != nil {
			return workspaceRefusal(workspace.ClassProtocol, "the worker's answer was not a server list")
		}
		r := unescapedResult(la)
		n := len(la.Servers)
		r.Count = &n
		return r
	}
	return workspaceOK(out)
}

// unescapedResult encodes v without HTML escaping, as a cell's result is:
// a server's output is text the model reads.
func unescapedResult(v any) tools.Result {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return workspaceRefusal("failed", "could not encode the result")
	}
	return tools.Result{Text: strings.TrimSuffix(buf.String(), "\n")}
}

// workspaceServerError is workspaceError with the server classes explained.
func workspaceServerError(err error, in workspaceInput) tools.Result {
	switch dispatch.ClassOf(err) {
	case workspace.ClassServerLimit:
		return workspaceRefusal(workspace.ClassServerLimit, fmt.Sprintf("this workspace runs %d servers, the most it may; stop one first (logs lists them)", workspace.MaxServers))
	case workspace.ClassServerNotFound:
		return workspaceRefusal(workspace.ClassServerNotFound, fmt.Sprintf("workspace %q has no server %q; logs lists them", in.Name, in.Server))
	}
	return workspaceError(err, in.Name)
}
