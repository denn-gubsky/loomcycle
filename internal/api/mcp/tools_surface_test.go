package mcp

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestToolSurface_DescriptionsListEveryOp.
//
// The description is hand-maintained prose listing the ops, and Document's had drifted 13
// ops behind the tool: query_documents, list_facts, the tag ops, the history ops,
// backlinks, related, unlinked_mentions, the canvas ops — and propose_entity, added in the
// same change as this test. A missing op is not cosmetic: for an MCP client the
// description IS the documentation, so an unlisted op is one no model will call.
//
// MEMORY WAS ADDED AFTER THE SAME DRIFT REACHED IT. Its description named four families
// and silently omitted the fifth — the whole consolidation surface (cursor_*, supersede,
// pending_*), eight ops. Covering one op-dispatched tool and not its sibling is how the
// second one drifts: the check has to follow the shape, not the instance.
func TestToolSurface_DescriptionsListEveryOp(t *testing.T) {
	for _, tc := range []struct {
		tool   string // the MCP tool name
		src    string // the builtin whose op enum is authoritative
		minOps int    // a floor, so a changed enum shape fails loudly instead of vacuously
	}{
		{"document", "../../tools/builtin/document.go", 20},
		{"memory", "../../tools/builtin/memory.go", 20},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			src, err := os.ReadFile(tc.src)
			if err != nil {
				t.Fatalf("read %s: %v", tc.src, err)
			}
			m := regexp.MustCompile(`"op":\s*\{"type": "string", "enum": \[([^\]]+)\]`).FindSubmatch(src)
			if m == nil {
				t.Fatalf("could not find the %s op enum — if its shape changed, update this test", tc.tool)
			}
			var ops []string
			for _, raw := range strings.Split(string(m[1]), ",") {
				if op := strings.Trim(strings.TrimSpace(raw), `"`); op != "" {
					ops = append(ops, op)
				}
			}
			if len(ops) < tc.minOps {
				t.Fatalf("only parsed %d ops — the enum shape probably changed, and a vacuous pass "+
					"here would let the description drift silently", len(ops))
			}
			desc := toolDescription(t, tc.tool)
			var missing []string
			for _, op := range ops {
				if !strings.Contains(desc, op) {
					missing = append(missing, op)
				}
			}
			if len(missing) > 0 {
				t.Errorf("the MCP %s description does not mention %v — an MCP client reads this as "+
					"the documentation, so an unlisted op is one no model will call", tc.tool, missing)
			}
		})
	}
}

// TestToolSurface_NoDesignDocCitationsInModelVisibleText.
//
// Every Description here is sent to an MCP client and put in front of a model. An
// internal design-document letter ("RFC AK") means nothing to a model and nothing to an
// operator reading a tool list; it is shorthand for a conversation they were not part of.
// Eighteen had accumulated, thirteen of them inside descriptions.
//
// Go comments are exempt on purpose — they are for us, and the citation is useful there.
func TestToolSurface_NoDesignDocCitationsInModelVisibleText(t *testing.T) {
	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatalf("read tools.go: %v", err)
	}
	cite := regexp.MustCompile(`RFC [A-Z]{1,2}\b`)
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue // a Go comment is ours, not the model's
		}
		if loc := cite.FindString(line); loc != "" {
			t.Errorf("tools.go:%d cites %q in model-visible text: %s", i+1, loc,
				truncateForMsg(trimmed))
		}
	}
}

func truncateForMsg(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// toolDescription pulls the live Description off one registered tool.
//
// THE DESCRIPTION ONLY — deliberately not the input schema. The first version of this
// test appended the marshalled schema "to also catch ops documented in a field", which
// made it vacuous: the schema's `op` enum lists every op by construction, so the
// assertion could never fail. It passed against a description gutted down to five ops.
func toolDescription(t *testing.T, name string) string {
	t.Helper()
	for _, tool := range toolDescriptors() {
		if tool.Name == name {
			return tool.Description
		}
	}
	t.Fatalf("no %q tool in the MCP surface", name)
	return ""
}

// rubricDone lists the MCP tools whose descriptions have been rewritten to the
// full rubric: purpose, inputs and their constraints, what it refuses or caps,
// and — the element this surface had ZERO of — when to use THIS tool rather
// than the neighbour a caller would otherwise pick.
//
// WHY A LIST AND NOT EVERY TOOL. 52 descriptions are being converted in
// clusters, and a test that fails for the 30 not yet reached would have to be
// skipped, which is the same as not having it. Every tool moved into this list
// is held to the rubric from then on; the companion test below makes the list
// itself hard to forget about.
//
// WHY BOUNDARIES ARE THE CHECKED ELEMENT. This surface is consumed by EXTERNAL
// agents over the MCP transport — a Claude Code session, someone else's client
// — with no loomcycle system prompt in front of them doing the disambiguating.
// The description is the entire briefing. Clusters here are dense and the names
// are near-synonyms: subscribe_channel vs peek_channel+ack_channel is an
// at-most-once vs at-least-once decision, get_snapshot vs export_snapshot is
// read vs move, register_agent vs agentdef is a scratch agent vs a durable one.
// Picking the wrong one of those is not a formatting problem.
var rubricDone = []string{
	// Channel cluster: one meta-tool, four single-op twins, an admin aggregate,
	// and the definition plane.
	"channel", "publish_channel", "subscribe_channel", "peek_channel",
	"ack_channel", "list_channels", "channeldef",
	// Snapshot cluster: all admin-only, and read-vs-move is the trap.
	"create_snapshot", "list_snapshots", "get_snapshot", "export_snapshot",
	"restore_snapshot", "delete_snapshot",
	// Run and agent cluster: one-vs-many, and scratch-vs-durable.
	"spawn_run", "spawn_runs", "cancel_run", "get_run", "list_runs",
	"compact_run", "register_agent", "unregister_agent", "list_agents",
}

func TestToolSurface_ConvertedToolsStateTheirBoundaries(t *testing.T) {
	boundary := regexp.MustCompile(`(?i)\bdo not use\b|\bdo NOT\b`)
	for _, name := range rubricDone {
		desc := toolDescription(t, name)
		if desc == "" {
			t.Errorf("%s: not in the tool surface — did it get renamed or removed?", name)
			continue
		}
		if !boundary.MatchString(desc) {
			t.Errorf("%s: description never says what NOT to use it for. An external agent "+
				"reads this cold, with no system prompt to disambiguate — name the neighbour "+
				"it would otherwise pick.", name)
		}
		if len(desc) < 250 {
			t.Errorf("%s: description is %d bytes, too short to carry inputs, limits AND a "+
				"boundary", name, len(desc))
		}
	}
}

// TestToolSurface_RubricListIsHonest keeps rubricDone from drifting into a
// decorative list: every name in it must still be a real tool, and the count
// must match what the cluster comments above claim. A tool dropped from the
// surface silently shrinking the checked set is the failure mode.
func TestToolSurface_RubricListIsHonest(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range rubricDone {
		if seen[n] {
			t.Errorf("%s listed twice in rubricDone", n)
		}
		seen[n] = true
	}
	if got, want := len(rubricDone), 22; got != want {
		t.Errorf("rubricDone has %d entries, expected %d — update the count deliberately when "+
			"a cluster is converted, so the number stays a claim rather than a side effect",
			got, want)
	}
}
