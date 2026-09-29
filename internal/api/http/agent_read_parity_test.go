package http

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// httpOnlyAgentFields are agentResponse fields the gRPC read cannot carry:
// they come from the serving replica's in-process registry of resident
// sub-agents (fillResidentState), which the gRPC server has no handle on.
var httpOnlyAgentFields = map[string]bool{"resident": true, "resident_state": true}

// jsonFields lists a struct's JSON field names, from its tags.
func jsonFields(t *testing.T, v any) []string {
	t.Helper()
	rt := reflect.TypeOf(v)
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// sourceBlock returns the text of path between start and the first end after
// it, or skips when the file is not in this checkout.
func sourceBlock(t *testing.T, path, start, end string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("%s not readable from here: %v", path, err)
	}
	src := string(b)
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("%s: %q not found — this test asserts nothing until it matches again", path, start)
	}
	blk := src[i+len(start):]
	if j := strings.Index(blk, end); j >= 0 {
		blk = blk[:j]
	}
	return blk
}

func declaredNames(re *regexp.Regexp, blk string) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(blk, -1) {
		out[m[1]] = true
	}
	return out
}

var (
	tsFieldRe    = regexp.MustCompile(`(?m)^\s*([a-z0-9_]+)\??:`)
	protoFieldRe = regexp.MustCompile(`(?m)^\s*(?:repeated\s+)?[\w.]+\s+([a-z0-9_]+)\s*=\s*\d+;`)
)

// The TS Agent is a hand-written mirror of agentResponse. Every field the HTTP
// run read sends must be declared there, or a typed TS caller cannot read it —
// the drift that left interactive, awaited_state, replica_id and the resident
// fields unnamed in the adapter while the server sent them.
func TestAgentResponse_TSMirrorDeclaresEveryField(t *testing.T) {
	for _, tc := range []struct {
		goType any
		ts     string
	}{
		{agentResponse{}, "export interface Agent {"},
		{agentResponseUsage{}, "export interface AgentUsage {"},
	} {
		fields := jsonFields(t, tc.goType)
		if len(fields) < 5 {
			t.Fatalf("only %d fields read from %T — the reflection has stopped matching", len(fields), tc.goType)
		}
		declared := declaredNames(tsFieldRe, sourceBlock(t, "../../../adapters/ts/src/types.ts", tc.ts, "\n}"))
		for _, f := range fields {
			if !declared[f] {
				t.Errorf("%T sends %q but TS %q does not declare it", tc.goType, f, tc.ts)
			}
		}
	}
}

// The gRPC Agent message and the Python client's dict of it carry every field
// of the HTTP run read, bar the per-replica resident pair. A field added to
// agentResponse fails here until both carry it (or it is listed as HTTP-only
// with the reason).
func TestAgentResponse_GRPCAndPythonCarryEveryField(t *testing.T) {
	for _, tc := range []struct {
		goType any
		proto  string
	}{
		{agentResponse{}, "message Agent {"},
		{agentResponseUsage{}, "message AgentUsage {"},
	} {
		declared := declaredNames(protoFieldRe, sourceBlock(t, "../../../proto/loomcycle.proto", tc.proto, "\n}"))
		for _, f := range jsonFields(t, tc.goType) {
			if !httpOnlyAgentFields[f] && !declared[f] {
				t.Errorf("%T sends %q but proto %q does not declare it", tc.goType, f, tc.proto)
			}
		}
	}

	py := sourceBlock(t, "../../../adapters/python/loomcycle/client.py", "def _agent_to_dict(", "\ndef ")
	for _, v := range []any{agentResponse{}, agentResponseUsage{}} {
		for _, f := range jsonFields(t, v) {
			if !httpOnlyAgentFields[f] && !strings.Contains(py, `"`+f+`":`) {
				t.Errorf("%T sends %q but the Python _agent_to_dict does not map it", v, f)
			}
		}
	}
}

// store.ParentContext rides the run read and the run-state stream on every
// transport, and has three hand-written mirrors: the proto message, the TS
// interface and the Python dict. A field the runtime stamps and a mirror does
// not name is invisible to that client — which is what a consumer reading a
// walk's members by state would hit first.
func TestParentContext_EveryMirrorDeclaresEveryField(t *testing.T) {
	fields := jsonFields(t, store.ParentContext{})
	if len(fields) < 5 {
		t.Fatalf("only %d fields read from store.ParentContext — the reflection has stopped matching", len(fields))
	}
	proto := declaredNames(protoFieldRe, sourceBlock(t, "../../../proto/loomcycle.proto", "message ParentContext {", "\n}"))
	ts := declaredNames(tsFieldRe, sourceBlock(t, "../../../adapters/ts/src/types.ts", "export interface ParentContext {", "\n}"))
	py := sourceBlock(t, "../../../adapters/python/loomcycle/client.py", "def _parent_context_to_dict(", "\ndef ")
	for _, f := range fields {
		if !proto[f] {
			t.Errorf("store.ParentContext has %q but proto ParentContext does not declare it", f)
		}
		if !ts[f] {
			t.Errorf("store.ParentContext has %q but TS ParentContext does not declare it", f)
		}
		if !strings.Contains(py, `"`+f+`":`) {
			t.Errorf("store.ParentContext has %q but the Python _parent_context_to_dict does not map it", f)
		}
	}
}
