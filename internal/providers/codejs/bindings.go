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
// permission error. `allowed_tools` is the floor: ANY allowed tool — built-in
// or MCP — is callable, never just a hardcoded subset.
//
// Tools are referenced in JS by their EXACT canonical name — the same string
// as in `allowed_tools` and as every other agent uses (CamelCase: Memory,
// WebFetch, …). No casing translation. Two binding shapes:
//   - The three multi-op META-tools are objects whose methods are the ops:
//     Memory.get/set/delete/search(obj) → "Memory" with {op, ...obj}
//     Channel.publish/subscribe(obj)    → "Channel" with {op, ...obj}
//     Agent.spawn(obj)                  → "Agent" with obj
//   - EVERY OTHER allowed tool — built-ins (WebFetch, Read, HTTP, WebSearch,
//     Grep, Glob, …) and mcp__<server>__<tool> — binds as a FLAT callable by
//     its name (all valid JS identifiers), args verbatim:
//     WebFetch({url}),  mcp__jobs__ingestJobs({...}).
//
// Each call becomes an EventToolCall the LOOP dispatches (schema validation,
// hooks, OTEL, ${run.credentials} substitution, and WebFetch/HTTP host-
// allowlist enforcement are the loop's existing path — no second trust model);
// the binding only translates. A tool the loop returns as IsError surfaces as
// a catchable JS throw.
func buildBindFunc(toolNames []string) bindFunc {
	return func(rt *goja.Runtime, emit toolEmitter) {
		for _, name := range toolNames {
			switch name {
			case "Memory":
				mem := rt.NewObject()
				for _, op := range []string{"get", "set", "delete", "search"} {
					_ = mem.Set(op, opCallable(rt, emit, "Memory", op))
				}
				_ = rt.Set(name, mem) // JS: Memory.get(...) etc.
			case "Channel":
				ch := rt.NewObject()
				for _, op := range []string{"publish", "subscribe"} {
					_ = ch.Set(op, opCallable(rt, emit, "Channel", op))
				}
				_ = rt.Set(name, ch) // JS: Channel.publish(...) etc.
			case "Agent":
				ag := rt.NewObject()
				// Agent.spawn maps to the Agent tool's default invocation; the
				// Agent tool's own schema validates name/prompt at dispatch. Its
				// result is the sub-agent's output → parse if JSON.
				_ = ag.Set("spawn", rawCallable(rt, emit, "Agent", true))
				_ = rt.Set(name, ag) // JS: Agent.spawn(...)
			default:
				// Built-in (WebFetch/Read/HTTP/…) or mcp__server__tool — a flat
				// callable by canonical name; the tool's own schema is the
				// contract, validated at the loop's dispatch. MCP results are a
				// structured JSON contract → parse; plain built-ins return text
				// → raw string (see rawCallable).
				_ = rt.Set(name, rawCallable(rt, emit, name, strings.HasPrefix(name, "mcp__")))
			}
		}
	}
}

// opCallable builds a JS function that injects {"op": op} into its object
// argument and dispatches it to toolName. Used for the multi-op meta-tools
// (Memory, Channel) whose JS methods are the op names.
func opCallable(rt *goja.Runtime, emit toolEmitter, toolName, op string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		input, err := opInput(rt, call, op)
		if err != nil {
			panic(rt.NewTypeError(fmt.Sprintf("%s.%s: %s", toolName, op, err)))
		}
		// Meta-tools have a known loomcycle-JSON result contract → return a
		// parsed object so `Memory.get(...).value` works.
		return invoke(rt, emit, toolName, input, true)
	}
}

// rawCallable builds a JS function that dispatches its object argument
// verbatim to toolName (no op injection). Used for Agent.spawn, every flat
// built-in (WebFetch/Read/HTTP/…), and mcp__server__tool callables.
//
// parseJSON picks the return mapping. true (MCP tools, Agent.spawn): parse a
// JSON result to an object (string fallback). false (plain built-ins like
// WebFetch/Read/HTTP): return the RAW string — their contract is "returns
// text", and auto-parsing a JSON-looking body would make the return type
// depend on content (object for a JSON page, string for HTML); the operator
// JSON.parse()s when they know the shape.
func rawCallable(rt *goja.Runtime, emit toolEmitter, toolName string, parseJSON bool) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		input, err := marshalArg(call)
		if err != nil {
			panic(rt.NewTypeError(fmt.Sprintf("%s: %s", toolName, err)))
		}
		return invoke(rt, emit, toolName, input, parseJSON)
	}
}

// invoke is the shared dispatch tail: suspend via emit, then map the result
// back into JS. err (ctx cancel/timeout) and IsError both become JS throws;
// the difference is that an IsError throw is an ordinary catchable Error
// (operator code may try/catch a failed tool), whereas a ctx-cancel throw
// will typically propagate to run failure as the run is already over.
// parseJSON selects the result mapping: parsed object (meta-tools) vs raw
// string (flat tools) — see the call sites.
func invoke(rt *goja.Runtime, emit toolEmitter, name string, input json.RawMessage, parseJSON bool) goja.Value {
	text, isError, err := emit(name, input)
	if err != nil {
		panic(rt.NewGoError(err))
	}
	if isError {
		panic(rt.NewGoError(errors.New(text)))
	}
	if text == "" {
		return goja.Undefined()
	}
	if parseJSON {
		return jsonToValue(rt, text)
	}
	return rt.ToValue(text)
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

// jsonToValue parses a meta-tool's JSON result into a JS value. Meta-tool
// results are always valid loomcycle-encoded JSON, so the parse succeeds; the
// raw-string fallback guards a malformed result rather than failing the call.
func jsonToValue(rt *goja.Runtime, text string) goja.Value {
	var v interface{}
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return rt.ToValue(text)
	}
	return rt.ToValue(v)
}
