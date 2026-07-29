package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// describedTool is a tool fixture with a controllable description, so the
// rendering tests assert on shape rather than on whatever a real builtin's help
// text happens to say today.
type describedTool struct{ name, desc string }

func (d describedTool) Name() string                 { return d.name }
func (d describedTool) Description() string          { return d.desc }
func (d describedTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (d describedTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{}, nil
}

func toolFixtures() []tools.Tool {
	return []tools.Tool{
		describedTool{"Write", "Write a file to an attached volume. Overwrites if it exists.\nMore detail here."},
		describedTool{"Read", "Read a file from an attached volume."},
		describedTool{"mcp__jobs__patchApplication", "Patch a job application record."},
	}
}

// TestToolInject_InventoryReachesTheAssembledPrompt is the WIRING test: the
// expander, the renderer and applyMemoryInjection have to be connected, not
// merely each correct. A previous feature in this subsystem shipped built and
// unit-tested with no caller, so this asserts on the prompt the run actually gets.
func TestToolInject_InventoryReachesTheAssembledPrompt(t *testing.T) {
	s := &Server{}
	def := config.AgentDef{SystemPrompt: "You are helpful.\n\n{{tool:Context.tools}}"}
	mi := memInject{Tenant: "t1", UserID: "u1", AgentName: "a", Tools: toolFixtures()}

	got, _ := s.applyMemoryInjection(context.Background(), def, mi)

	for _, want := range []string{
		"You are helpful.",
		`<tool-result tool="Context" op="tools">`,
		"- Read", "- Write", "- mcp__jobs__patchApplication",
		"</tool-result>",
	} {
		if !strings.Contains(got.SystemPrompt, want) {
			t.Errorf("missing %q in assembled prompt:\n%s", want, got.SystemPrompt)
		}
	}
	if strings.Contains(got.SystemPrompt, "{{tool:") {
		t.Errorf("placeholder left unexpanded:\n%s", got.SystemPrompt)
	}
}

// TestToolInject_ListsOnlyTheAgentsOwnTools: the inventory is the run's RESOLVED
// tool list, never the server's registry. A prompt that advertised a tool the
// dispatcher will refuse teaches the model to attempt a call that always fails.
func TestToolInject_ListsOnlyTheAgentsOwnTools(t *testing.T) {
	s := &Server{}
	def := config.AgentDef{SystemPrompt: "{{tool:Context.tools}}"}
	// Only Read is bound to this run.
	mi := memInject{Tenant: "t1", UserID: "u1", AgentName: "a",
		Tools: []tools.Tool{describedTool{"Read", "Read a file."}}}

	got, _ := s.applyMemoryInjection(context.Background(), def, mi)

	if !strings.Contains(got.SystemPrompt, "- Read") {
		t.Fatalf("the agent's own tool is missing:\n%s", got.SystemPrompt)
	}
	for _, absent := range []string{"- Write", "- Bash", "- Agent"} {
		if strings.Contains(got.SystemPrompt, absent) {
			t.Errorf("advertised %q, which this run cannot call:\n%s", absent, got.SystemPrompt)
		}
	}
}

// TestToolInject_NoToolsRendersNothing: an agent with no tools must not get an
// empty framed section claiming a tool result.
func TestToolInject_NoToolsRendersNothing(t *testing.T) {
	s := &Server{}
	def := config.AgentDef{SystemPrompt: "Base.\n{{tool:Context.tools}}"}
	got, _ := s.applyMemoryInjection(context.Background(), def, memInject{Tenant: "t1", UserID: "u1"})
	if strings.Contains(got.SystemPrompt, "tool-result") {
		t.Fatalf("emitted a frame for an empty inventory:\n%s", got.SystemPrompt)
	}
}

// TestToolInject_FastPathUnchangedWithoutPlaceholder: an agent that references no
// placeholder must come back byte-identical, with no tool dispatch. Every
// non-memory agent in every deployment takes this path on every run.
func TestToolInject_FastPathUnchangedWithoutPlaceholder(t *testing.T) {
	s := &Server{}
	const base = "Just a prompt with no placeholders."
	def := config.AgentDef{SystemPrompt: base}
	got, _ := s.applyMemoryInjection(context.Background(), def, memInject{
		Tenant: "t1", UserID: "u1", Tools: toolFixtures(),
	})
	if got.SystemPrompt != base {
		t.Fatalf("prompt changed on the fast path:\ngot  %q\nwant %q", got.SystemPrompt, base)
	}
}

// TestToolInject_IsByteStableAcrossRuns pins the provider prompt-cache invariant.
// The rendered body must be a pure function of the tool SET: registry iteration
// order is not stable, and a body that reordered between runs would miss the
// cache on the entire system-prompt prefix every time.
func TestToolInject_IsByteStableAcrossRuns(t *testing.T) {
	s := &Server{}
	def := config.AgentDef{SystemPrompt: "{{tool:Context.tools}}"}
	shuffled := [][]tools.Tool{
		{describedTool{"Write", "w"}, describedTool{"Read", "r"}, describedTool{"Bash", "b"}},
		{describedTool{"Bash", "b"}, describedTool{"Write", "w"}, describedTool{"Read", "r"}},
		{describedTool{"Read", "r"}, describedTool{"Bash", "b"}, describedTool{"Write", "w"}},
	}
	var first string
	for i, set := range shuffled {
		got, _ := s.applyMemoryInjection(context.Background(), def,
			memInject{Tenant: "t1", UserID: "u1", Tools: set})
		if i == 0 {
			first = got.SystemPrompt
			continue
		}
		if got.SystemPrompt != first {
			t.Fatalf("expansion depends on tool ORDER, so the prompt is not cache-stable:\n%q\nvs\n%q", first, got.SystemPrompt)
		}
	}
}

// TestToolInject_CarriesNoSecretAdjacentField: whatever else the projection
// renders, it must never carry a credential fragment. A system prompt is
// persisted into every transcript, snapshot and prompt-cache entry, so a leak
// here is durable and fans out.
func TestToolInject_CarriesNoSecretAdjacentField(t *testing.T) {
	s := &Server{}
	def := config.AgentDef{SystemPrompt: "{{tool:Context.tools}}"}
	got, _ := s.applyMemoryInjection(context.Background(), def, memInject{
		Tenant: "t1", UserID: "u1", Tools: toolFixtures(),
	})
	for _, banned := range []string{"token_suffix", "bearer", "api_key", "Bearer "} {
		if strings.Contains(strings.ToLower(got.SystemPrompt), strings.ToLower(banned)) {
			t.Errorf("injected prompt contains %q:\n%s", banned, got.SystemPrompt)
		}
	}
}

// TestAllowedToolRefs_AllHaveRenderer pins the allowlist to its renderers: adding
// a ref in internal/memory without wiring one here would degrade silently to
// "renders nothing", which reads exactly like a broken deployment.
func TestAllowedToolRefs_AllHaveRenderer(t *testing.T) {
	s := &Server{}
	mi := memInject{Tenant: "t1", UserID: "u1", AgentName: "a", Tools: toolFixtures()}
	for _, raw := range meminject.AllToolRefs() {
		ref, ok := meminject.ParseToolRef(raw)
		if !ok {
			t.Fatalf("AllToolRefs returned %q, which ParseToolRef rejects", raw)
		}
		if body := s.renderToolResult(context.Background(), mi, ref); body == "" {
			t.Errorf("allowlisted ref %s has no renderer (or rendered empty) — wire one in renderToolResult", raw)
		}
	}
}

// TestToolInject_MatchesContextToolsOp: the injected inventory must agree with
// what Context op=tools reports, because an agent can call the tool and compare.
// Two independent implementations of "what tools do I have" would drift silently.
func TestToolInject_MatchesContextToolsOp(t *testing.T) {
	fixtures := toolFixtures()
	ctx := tools.WithAgentTools(context.Background(), toolNames(fixtures))
	ct := &builtin.Context{Tools: fixtures}
	req, _ := json.Marshal(map[string]any{"op": "tools"})
	res, err := ct.Execute(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("Context op=tools failed: err=%v isErr=%v text=%s", err, res.IsError, res.Text)
	}
	var parsed struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal([]byte(res.Text), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}

	s := &Server{}
	got, _ := s.applyMemoryInjection(context.Background(),
		config.AgentDef{SystemPrompt: "{{tool:Context.tools}}"},
		memInject{Tenant: "t1", UserID: "u1", Tools: fixtures})

	for _, tool := range parsed.Tools {
		if !strings.Contains(got.SystemPrompt, "- "+tool.Name) {
			t.Errorf("Context op=tools reports %q but the injected inventory omits it:\n%s", tool.Name, got.SystemPrompt)
		}
	}
}

func TestFirstSentence_CutsAtLineAndSentence(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Write a file. Overwrites if it exists.", "Write a file."},
		{"One line only", "One line only"},
		{"First line.\nSecond line.", "First line."},
		{"Version v1.2 is fine", "Version v1.2 is fine"},
		{"  padded  ", "padded"},
		{"", ""},
	}
	for _, c := range cases {
		if got := firstSentence(c.in, 120); got != c.want {
			t.Errorf("firstSentence(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFirstSentence_HardCapBacksOffToAWordBoundary: the real Document
// description hit the cap mid-word ("Markdown body) t…"), which reads as
// corruption rather than truncation.
func TestFirstSentence_HardCapBacksOffToAWordBoundary(t *testing.T) {
	got := firstSentence("alpha beta gamma delta epsilon zeta eta theta", 30)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("want a truncation marker, got %q", got)
	}
	trimmed := strings.TrimSuffix(got, "…")
	if strings.HasSuffix(trimmed, " ") {
		t.Errorf("trailing space before the marker: %q", got)
	}
	// The last kept token must be whole.
	fields := strings.Fields(trimmed)
	last := fields[len(fields)-1]
	if !strings.Contains("alpha beta gamma delta epsilon zeta eta theta", last+" ") &&
		!strings.HasSuffix("alpha beta gamma delta epsilon zeta eta theta", last) {
		t.Errorf("last token %q is a fragment: %q", last, got)
	}
}

// TestFirstSentence_NoSpaceInRangeStillTruncates: backing off to a word boundary
// must not collapse a long unbroken token to nothing.
func TestFirstSentence_NoSpaceInRangeStillTruncates(t *testing.T) {
	got := firstSentence(strings.Repeat("x", 200), 40)
	if len(got) < 30 {
		t.Fatalf("collapsed a space-free description to %q", got)
	}
}

// TestFirstSentence_HardCapKeepsRunesIntact: a byte-cap that split a multi-byte
// rune would render U+FFFD in every affected agent's system prompt.
func TestFirstSentence_HardCapKeepsRunesIntact(t *testing.T) {
	got := firstSentence(strings.Repeat("é", 100), 21)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("want a truncation marker, got %q", got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncation split a rune: %q", got)
	}
	if !isValidUTF8(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// TestToolInject_PreamblePresentAndCitesNoRFC: the list alone did not change
// behaviour in practice — the observed failure was reluctance to act, not
// ignorance of names — so the preamble telling the model to just call them is
// part of the feature. And it is model-visible text, so the house rule applies.
func TestToolInject_PreamblePresentAndCitesNoRFC(t *testing.T) {
	s := &Server{}
	got, _ := s.applyMemoryInjection(context.Background(),
		config.AgentDef{SystemPrompt: "{{tool:Context.tools}}"},
		memInject{Tenant: "t1", UserID: "u1", Tools: toolFixtures()})
	if !strings.Contains(got.SystemPrompt, "do not ask whether a tool exists") {
		t.Errorf("preamble missing:\n%s", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "RFC") {
		t.Errorf("model-visible injected text must not cite RFC letters/numbers:\n%s", got.SystemPrompt)
	}
}
