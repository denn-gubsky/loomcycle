package codejs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

// bindFunc registers the agent's permitted tool surface onto rt, using emit
// as the tool-call primitive. Supplied by the provider (it knows req.Tools).
type bindFunc func(rt *goja.Runtime, emit toolEmitter)

// toolEmitter is the host-side primitive a bound JS tool function calls. In
// the replay model (replay.go) it either fast-forwards a recorded result or
// stops at the frontier; the signature is uniform either way. err != nil is a
// host/ctx failure the binding throws into JS; isError is a tool error the
// binding throws as a catchable JS error; otherwise text is the result.
type toolEmitter func(name string, input json.RawMessage) (text string, isError bool, err error)

// buildBindFunc returns the bindFunc that registers ONLY the agent's
// permitted tool surface (RFC J Decision 7: default-deny by construction). It
// is driven by the tool names the loop computed from the agent's allowed_tools
// (req.Tools); a tool absent from that list gets NO JS binding — operator code
// referencing it sees `ReferenceError: <name> is not defined`, not a
// permission error.
//
// The JS surface mirrors the loomcycle MCP meta-tool API (Decision 6):
//
//	memory.get/set/delete/search(obj)   → tool "Memory" with {op, ...obj}
//	channel.publish/subscribe(obj)      → tool "Channel" with {op, ...obj}
//	agent.spawn(obj)                    → tool "Agent" with obj
//	mcp__<server>__<tool>(obj)          → that tool name, obj verbatim
//
// Each call becomes an EventToolCall the LOOP dispatches (schema validation,
// hooks, OTEL, ${run.credentials} substitution are the loop's existing path);
// the binding only translates + suspends. A tool the loop returns as IsError
// surfaces as a catchable JS throw.
func buildBindFunc(toolNames []string) bindFunc {
	allowed := make(map[string]bool, len(toolNames))
	var mcpTools []string
	for _, n := range toolNames {
		allowed[n] = true
		if strings.HasPrefix(n, "mcp__") {
			mcpTools = append(mcpTools, n)
		}
	}

	return func(rt *goja.Runtime, emit toolEmitter) {
		if allowed["Memory"] {
			mem := rt.NewObject()
			for _, op := range []string{"get", "set", "delete", "search"} {
				_ = mem.Set(op, opCallable(rt, emit, "Memory", op))
			}
			_ = rt.Set("memory", mem)
		}
		if allowed["Channel"] {
			ch := rt.NewObject()
			for _, op := range []string{"publish", "subscribe"} {
				_ = ch.Set(op, opCallable(rt, emit, "Channel", op))
			}
			_ = rt.Set("channel", ch)
		}
		if allowed["Agent"] {
			ag := rt.NewObject()
			// agent.spawn maps to the Agent tool's default invocation; the
			// Agent tool's own input schema validates name/prompt at dispatch.
			_ = ag.Set("spawn", rawCallable(rt, emit, "Agent"))
			_ = rt.Set("agent", ag)
		}
		// MCP tools bind as flat global callables named exactly as the loop
		// knows them (mcp__server__tool is a valid JS identifier). The arg
		// object passes through verbatim — the MCP tool's own schema is the
		// contract, validated at the loop's dispatch.
		for _, name := range mcpTools {
			_ = rt.Set(name, rawCallable(rt, emit, name))
		}
	}
}

// opCallable builds a JS function that injects {"op": op} into its object
// argument and dispatches it to toolName. Used for the built-in multi-op
// tools (Memory, Channel) whose JS methods are the op names.
func opCallable(rt *goja.Runtime, emit toolEmitter, toolName, op string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		input, err := opInput(rt, call, op)
		if err != nil {
			panic(rt.NewTypeError(fmt.Sprintf("%s.%s: %s", strings.ToLower(toolName), op, err)))
		}
		return invoke(rt, emit, toolName, input)
	}
}

// rawCallable builds a JS function that dispatches its object argument
// verbatim to toolName (no op injection). Used for Agent.spawn and MCP tools.
func rawCallable(rt *goja.Runtime, emit toolEmitter, toolName string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		input, err := marshalArg(call)
		if err != nil {
			panic(rt.NewTypeError(fmt.Sprintf("%s: %s", toolName, err)))
		}
		return invoke(rt, emit, toolName, input)
	}
}

// invoke is the shared dispatch tail: suspend via emit, then map the result
// back into JS. err (ctx cancel/timeout) and IsError both become JS throws;
// the difference is that an IsError throw is an ordinary catchable Error
// (operator code may try/catch a failed tool), whereas a ctx-cancel throw
// will typically propagate to run failure as the run is already over.
func invoke(rt *goja.Runtime, emit toolEmitter, name string, input json.RawMessage) goja.Value {
	text, isError, err := emit(name, input)
	if err != nil {
		panic(rt.NewGoError(err))
	}
	if isError {
		panic(rt.NewGoError(errors.New(text)))
	}
	return resultToValue(rt, text)
}

// opInput marshals call.Argument(0) (which must be an object or absent) and
// injects the op. Absent/undefined → just {"op": op}.
func opInput(rt *goja.Runtime, call goja.FunctionCall, op string) (json.RawMessage, error) {
	m := map[string]interface{}{}
	arg := call.Argument(0)
	if !goja.IsUndefined(arg) && !goja.IsNull(arg) {
		ex := arg.Export()
		mm, ok := ex.(map[string]interface{})
		if !ok {
			return nil, errors.New("argument must be an object")
		}
		for k, v := range mm {
			m[k] = v
		}
	}
	m["op"] = op
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding arguments: %w", err)
	}
	return b, nil
}

// marshalArg marshals call.Argument(0) verbatim. Absent/undefined → {}.
func marshalArg(call goja.FunctionCall) (json.RawMessage, error) {
	arg := call.Argument(0)
	if goja.IsUndefined(arg) || goja.IsNull(arg) {
		return json.RawMessage("{}"), nil
	}
	ex := arg.Export()
	if _, ok := ex.(map[string]interface{}); !ok {
		return nil, errors.New("argument must be an object")
	}
	b, err := json.Marshal(ex)
	if err != nil {
		return nil, fmt.Errorf("encoding arguments: %w", err)
	}
	return b, nil
}

// resultToValue turns a tool's text result into a JS value: parsed JSON when
// it is JSON (the common case — built-in tools return JSON objects), else the
// raw string.
func resultToValue(rt *goja.Runtime, text string) goja.Value {
	if text == "" {
		return goja.Undefined()
	}
	var v interface{}
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return rt.ToValue(text)
	}
	return rt.ToValue(v)
}
