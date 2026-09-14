package builtin

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// catalogContext builds the Context tool the way the SERVER does: pointed at
// the complete runtime catalog. That is what makes every introspection op here
// a disclosure decision rather than a formatting one — the tool can see
// everything in the deployment, and only the ctx allowlist keeps an agent's
// view inside its own grant.
func catalogContext() *Context {
	return &Context{Tools: []tools.Tool{
		&fakeTool{NameVal: "Read", DescVal: "read a file", SchemaVal: `{}`},
		&fakeTool{NameVal: "Bash", DescVal: "run a shell command", SchemaVal: `{}`},
		&fakeTool{NameVal: "HTTP", DescVal: "call an API", SchemaVal: `{}`},
		&fakeTool{NameVal: "mcp__slack__send", DescVal: "post a message", SchemaVal: `{}`},
		&fakeTool{NameVal: "mcp__slack__list", DescVal: "list channels", SchemaVal: `{}`},
		&fakeTool{NameVal: "mcp__payroll__pay", DescVal: "move money", SchemaVal: `{}`},
	}}
}

func disclosedBy(t *testing.T, ct *Context, ctx context.Context, op string) []string {
	t.Helper()
	res, _ := ct.Execute(ctx, json.RawMessage(`{"op":"`+op+`"}`))
	var parsed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(res.Text), &parsed); err != nil {
		t.Fatalf("op=%s: %v (%s)", op, err, res.Text)
	}
	names := make([]string, 0, len(parsed.Tools))
	for _, x := range parsed.Tools {
		names = append(names, x.Name)
	}
	sort.Strings(names)
	return names
}

// TestContextDisclosure_OnlyWhatTheAgentHolds is the whole contract, in the two
// directions it can break.
//
// The Context tool sees the runtime-wide catalog, so an agent granted Read must
// not learn from it that this deployment also has a payroll MCP server. Tool
// NAMES and DESCRIPTIONS are the disclosure: they tell a model what the
// operator built, which other tenants integrate with, and what to ask for next.
//
// Both introspection ops are checked, because they are separate filters over
// the same catalog and only one of them was ever the "famous" one.
func TestContextDisclosure_OnlyWhatTheAgentHolds(t *testing.T) {
	ct := catalogContext()
	for _, op := range []string{"tools", "guide"} {
		ctx := tools.WithAgentTools(context.Background(), []string{"Read", "mcp__slack__send"})
		got := disclosedBy(t, ct, ctx, op)
		want := []string{"Read", "mcp__slack__send"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("op=%s disclosed %v, want exactly %v", op, got, want)
		}
		for _, leaked := range []string{"Bash", "HTTP", "mcp__payroll__pay", "mcp__slack__list"} {
			for _, g := range got {
				if g == leaked {
					t.Errorf("op=%s leaked %q, which this agent does not hold", op, leaked)
				}
			}
		}
	}
}

// TestContextDisclosure_HonoursTheGlobConvention: a grant of "mcp__slack__*"
// covers that server's tools and nothing else.
//
// The disclosure filter used a plain map lookup, so it understood neither the
// prefix glob nor "*" — the SAME two forms the exposure layer and the
// substrate's tool-ceiling checks have always honoured. A third interpretation
// of an allowlist is a bug with extra steps.
func TestContextDisclosure_HonoursTheGlobConvention(t *testing.T) {
	ct := catalogContext()
	ctx := tools.WithAgentTools(context.Background(), []string{"Read", "mcp__slack__*"})
	got := disclosedBy(t, ct, ctx, "tools")
	want := []string{"Read", "mcp__slack__list", "mcp__slack__send"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("glob grant disclosed %v, want %v", got, want)
	}
	for _, g := range got {
		if g == "mcp__payroll__pay" {
			t.Error("a grant of mcp__slack__* disclosed another server's tool")
		}
	}
}

// TestContextDisclosure_WildcardMeansEverything: the operator surfaces — the
// MCP session, the gRPC substrate plane, the off-run admin path — all stamp
// "*", which is the codebase's established "unrestricted" marker.
//
// Treated as a literal name it matched nothing, so those surfaces introspected
// an EMPTY tool list: the filter failed in both directions at once, hiding
// everything from the operator while showing everything to an unstamped caller.
func TestContextDisclosure_WildcardMeansEverything(t *testing.T) {
	ct := catalogContext()
	ctx := tools.WithAgentTools(context.Background(), []string{"*"})
	got := disclosedBy(t, ct, ctx, "tools")
	if len(got) != 6 {
		t.Errorf(`a "*" grant disclosed %d tools, want all 6: %v`, len(got), got)
	}
}

// TestContextDisclosure_UnstampedCtxDisclosesNothing pins the direction the
// filter must fail in. "The runtime did not attach an allowlist" is a
// misconfiguration, not a licence to enumerate the deployment — the same line
// AgentDef's create takes on the same ctx value.
func TestContextDisclosure_UnstampedCtxDisclosesNothing(t *testing.T) {
	ct := catalogContext()
	for _, op := range []string{"tools", "guide"} {
		if got := disclosedBy(t, ct, context.Background(), op); len(got) != 0 {
			t.Errorf("op=%s on an unstamped ctx disclosed %v", op, got)
		}
	}
	// op=doc returns the FULL schema for one tool, so it is the sharpest
	// disclosure of the three and must refuse rather than answer.
	res, _ := ct.Execute(context.Background(), json.RawMessage(`{"op":"doc","name":"mcp__payroll__pay"}`))
	if !res.IsError {
		t.Errorf("op=doc on an unstamped ctx described a tool instead of refusing: %s", res.Text)
	}
}

// TestContextDisclosure_DocRespectsTheGrant: op=doc hands over a tool's whole
// input schema, so it needs the same filter as the listing ops rather than a
// name lookup over the catalog.
func TestContextDisclosure_DocRespectsTheGrant(t *testing.T) {
	ct := catalogContext()
	ctx := tools.WithAgentTools(context.Background(), []string{"Read"})

	res, _ := ct.Execute(ctx, json.RawMessage(`{"op":"doc","name":"Read"}`))
	if res.IsError {
		t.Fatalf("op=doc refused a tool the agent holds: %s", res.Text)
	}
	res, _ = ct.Execute(ctx, json.RawMessage(`{"op":"doc","name":"mcp__payroll__pay"}`))
	if !res.IsError {
		t.Errorf("op=doc described a tool the agent does not hold: %s", res.Text)
	}
}
