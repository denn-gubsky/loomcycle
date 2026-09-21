package config

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ⚠️ DERIVED FROM THE STRUCT, so the NEXT field is guarded too.
//
// `context` crosses six surfaces — the proto, the gRPC mapper, the MCP per-run
// schema, both adapters, and the editor's field registry — and a field that
// reaches only some of them is invisible from the transports it missed rather
// than broken in a way anyone notices. This project has shipped that shape
// repeatedly: the MCP schema that hid the per-run overrides, the Python stubs
// that dropped them, sixteen event types absent from the TS union.
//
// A hand-written list of fields would be a second copy of the struct. The
// struct's own json tags are the list.
func TestContextFields_ReachEverySurface(t *testing.T) {
	// ⚠️ THE MATCHER IS PER SURFACE, NOT PER FIELD. Each file spells a field in
	// its own dialect — a proto declaration, a Go identifier, a quoted JSON key
	// — and one loose "does the file mention this word" check reports coverage
	// that is not there: `recall` and `mode` occur in ordinary prose in every
	// one of these files. A style per FILE is six entries; a style per field
	// would be a second copy of the struct.
	surfaces := []struct {
		what string
		path string
		// scope narrows the file to the region that describes `context`. ⚠️
		// WITHOUT IT THE GUARD IS VACUOUS FOR SHORT KEYS: both nested surfaces
		// carry an unrelated `model` elsewhere (the agent's own routing model,
		// four other MCP schemas), so a file-wide search reported the new field
		// as present after it had been deleted. Found by probing this guard,
		// which is the only way that class of hole ever shows up.
		scope func(src string) string
		finds func(src, tag string) bool
	}{
		{"the gRPC proto", "../../proto/loomcycle.proto",
			func(src string) string { return balancedAfter(src, "message Context {", "{", "}") },
			func(src, tag string) bool { return strings.Contains(src, tag+" = ") }},
		{"the gRPC mapper", "../api/grpc/server.go",
			func(src string) string { return balancedAfter(src, "func contextFromProto(", "{", "}") },
			// Go identifiers, and the two sides disagree about capitalisation
			// (config.AutoRecapAtPct against proto AutorecapAtPct), so the
			// comparison folds case.
			func(src, tag string) bool {
				return strings.Contains(strings.ToLower(src), strings.ToLower(snakeToPascal(tag)))
			}},
		{"the MCP per-run schema", "../api/mcp/tools.go",
			func(src string) string { return balancedAfter(src, `"context":`, "{", "}") },
			func(src, tag string) bool { return strings.Contains(src, `"`+tag+`"`) }},
		{"the TS adapter", "../../adapters/ts/src/client.ts",
			func(src string) string { return balancedAfter(src, "function contextToWire(", "{", "}") },
			func(src, tag string) bool { return strings.Contains(src, "w."+tag+" =") }},
		{"the Python adapter", "../../adapters/python/loomcycle/client.py",
			func(src string) string { return afterAnchor(src, "out = pb.Context()", 1600) },
			func(src, tag string) bool { return strings.Contains(src, `"`+tag+`"`) }},
		{"the def-fields registry", "../../packages/def-fields/src/registries/agentdef.ts",
			func(src string) string { return balancedAfter(src, `key: "context"`, "[", "]") },
			func(src, tag string) bool { return strings.Contains(src, `key: "`+tag+`"`) }},
	}

	// state_schema is the one field carried as opaque bytes rather than by
	// name: the editor has no control for an arbitrary JSON-Schema blob, and
	// the TS serializer passes it through under its own camel spelling. Named
	// here so each exemption is a decision on the record rather than a silent
	// gap.
	exempt := map[string]map[string]bool{
		"state_schema": {"the def-fields registry": true, "the TS adapter": true},
	}

	var tags []string
	ctxType := reflect.TypeOf(Context{})
	for i := 0; i < ctxType.NumField(); i++ {
		tag := strings.Split(ctxType.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			tags = append(tags, tag)
		}
	}
	if len(tags) < 8 {
		t.Fatalf("only %d json-tagged fields found on config.Context — the struct's shape "+
			"changed and this guard is reading almost nothing: %v", len(tags), tags)
	}

	for _, s := range surfaces {
		b, err := os.ReadFile(s.path)
		if err != nil {
			t.Skipf("%s not readable from here: %v", s.path, err)
		}
		src := s.scope(string(b))
		if src == "" {
			t.Errorf("%s: could not locate the context region in %s — the file was restructured "+
				"and this guard would silently read nothing", s.what, s.path)
			continue
		}
		var missing []string
		for _, tag := range tags {
			if exempt[tag][s.what] {
				continue
			}
			if s.finds(src, tag) {
				continue
			}
			missing = append(missing, tag)
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("%s (%s) does not carry: %s\n\nA context field that reaches only some "+
				"transports is invisible from the rest, not broken — add it, or add a justified "+
				"entry to the exempt map.", s.what, s.path, strings.Join(missing, ", "))
		}
	}
}

func snakeToPascal(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// balancedAfter returns the first balanced open..close region following anchor,
// so a search is scoped to the block that describes `context` rather than to
// the whole file. "" when the anchor is gone, which the caller reports as a
// failure — a guard that cannot find what it guards must say so, not pass.
func balancedAfter(src, anchor, open, close string) string {
	i := strings.Index(src, anchor)
	if i < 0 {
		return ""
	}
	start := strings.Index(src[i:], open)
	if start < 0 {
		return ""
	}
	start += i
	depth := 0
	for j := start; j < len(src); j++ {
		switch string(src[j]) {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return src[start : j+1]
			}
		}
	}
	return ""
}

// afterAnchor returns a fixed window after anchor, for a surface with no
// bracketed block to balance (Python's serializer is a run of statements).
func afterAnchor(src, anchor string, n int) string {
	i := strings.Index(src, anchor)
	if i < 0 {
		return ""
	}
	if i+n > len(src) {
		n = len(src) - i
	}
	return src[i : i+n]
}
