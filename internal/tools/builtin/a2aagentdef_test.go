package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// a2aAgentDefFixture builds an A2AAgentDef tool over in-memory SQLite +
// a stub Config with one yaml template (a remote peer reached by
// agent_card_url).
func a2aAgentDefFixture(t *testing.T) (*A2AAgentDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		A2AAgents: map[string]config.A2AAgent{
			"peer-jobs": {
				AgentCardURL:   "https://peer.example/.well-known/agent-card.json",
				Auth:           config.A2AAgentAuth{Scheme: "http", BearerCredentialRef: "peer-token"},
				ExpectedSkills: []config.A2AExpectedSkill{{ID: "search", Required: true}},
			},
		},
	}
	tool := &A2AAgentDef{
		Store:               s,
		Cfg:                 cfg,
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithAgentName(context.Background(), "a2a-orchestrator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithA2AAgentDefPolicy(ctx, tools.A2AAgentDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: "a2a-orchestrator",
	})
	return tool, ctx, func() { _ = s.Close() }
}

func TestA2AAgentDefTool_CreateRefusedOverStaticName(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"peer-jobs","overlay":{"agent_card_url":"https://x.example"}}`))
	if !res.IsError {
		t.Fatalf("create over static name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "static cfg.A2AAgents") {
		t.Errorf("refusal should mention static; got %s", res.Text)
	}
}

func TestA2AAgentDefTool_CreateWithCardURL(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc-peer","overlay":{"agent_card_url":"https://adhoc.example/card.json"},"description":"new peer"}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "adhoc-peer" {
		t.Errorf("name = %v, want adhoc-peer", out["name"])
	}
	if out["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", out["version"])
	}
	if out["promoted"].(bool) != true {
		t.Errorf("create default promote = false; want true")
	}
}

func TestA2AAgentDefTool_CreateWithEndpointBinding(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"direct-peer","overlay":{"endpoint":"https://peer.example/a2a","binding":"jsonrpc"}}`))
	if res.IsError {
		t.Fatalf("create with endpoint+binding: %s", res.Text)
	}
}

// A model-authored grpc endpoint pointing at a literal private/link-local IP
// (the cloud metadata service) must be refused at create — the gRPC binding
// dials outside the SSRF-blocking peerDialContext, so registration-time is
// the defense. A public-host grpc endpoint is still accepted (the dial-time
// hostname/rebinding case for gRPC is the documented deferred residual).
func TestA2AAgentDefTool_CreateRefusesPrivateGRPCEndpoint(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	for _, ep := range []string{"169.254.169.254:50051", "127.0.0.1:50051", "10.0.0.5:443", "grpc://192.168.1.10:50051", "dns:///[::1]:50051"} {
		in := `{"op":"create","name":"badgrpc","overlay":{"endpoint":"` + ep + `","binding":"grpc"}}`
		res, _ := tool.Execute(ctx, json.RawMessage(in))
		if !res.IsError {
			t.Errorf("grpc endpoint %q (private/loopback/link-local) should be refused; got %s", ep, res.Text)
		} else if !strings.Contains(res.Text, "private/loopback/link-local") {
			t.Errorf("grpc endpoint %q refusal should name the SSRF reason; got %s", ep, res.Text)
		}
	}

	// A public-host grpc endpoint passes the literal-IP gate.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"okgrpc","overlay":{"endpoint":"peer.example:50051","binding":"grpc"}}`))
	if res.IsError {
		t.Errorf("public-host grpc endpoint should be accepted; got %s", res.Text)
	}
}

func TestA2AAgentDefTool_CreateRefusesBothReachabilityModes(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"both","overlay":{"agent_card_url":"https://x.example","endpoint":"https://y.example","binding":"jsonrpc"}}`))
	if !res.IsError {
		t.Fatalf("setting both agent_card_url and endpoint should refuse")
	}
	if !strings.Contains(res.Text, "exactly one") {
		t.Errorf("refusal should mention exactly-one; got %s", res.Text)
	}
}

func TestA2AAgentDefTool_CreateRefusesNeitherReachabilityMode(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"neither","overlay":{"auth":{"scheme":"http"}}}`))
	if !res.IsError {
		t.Fatalf("setting neither agent_card_url nor endpoint+binding should refuse")
	}
	if !strings.Contains(res.Text, "neither") {
		t.Errorf("refusal should mention neither; got %s", res.Text)
	}
}

func TestA2AAgentDefTool_CreateRefusesEndpointWithoutBinding(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"nobind","overlay":{"endpoint":"https://peer.example/a2a"}}`))
	if !res.IsError {
		t.Fatalf("endpoint without binding should refuse")
	}
	if !strings.Contains(res.Text, "binding required") {
		t.Errorf("refusal should mention binding required; got %s", res.Text)
	}
}

func TestA2AAgentDefTool_CreateRefusesBadBinding(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badbind","overlay":{"endpoint":"https://peer.example/a2a","binding":"soap"}}`))
	if !res.IsError {
		t.Fatalf("unknown binding should refuse")
	}
	if !strings.Contains(res.Text, "unknown binding") {
		t.Errorf("refusal should name the bad binding; got %s", res.Text)
	}
}

func TestA2AAgentDefTool_CreateRefusesBadAuthScheme(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badauth","overlay":{"agent_card_url":"https://x.example","auth":{"scheme":"basic"}}}`))
	if !res.IsError {
		t.Fatalf("unknown auth scheme should refuse")
	}
	if !strings.Contains(res.Text, "auth.scheme") {
		t.Errorf("refusal should mention auth.scheme; got %s", res.Text)
	}
}

func TestA2AAgentDefTool_CreateRefusesBadCredentialRef(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badref","overlay":{"agent_card_url":"https://x.example","auth":{"bearer_credential_ref":"has spaces"}}}`))
	if !res.IsError {
		t.Fatalf("malformed bearer_credential_ref should refuse")
	}
	if !strings.Contains(res.Text, "bearer_credential_ref") {
		t.Errorf("refusal should mention bearer_credential_ref; got %s", res.Text)
	}
}

func TestA2AAgentDefTool_ForkBootstrapsTemplate(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	// Bootstrap v1 from yaml + fork v2 rotating only the credential ref.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"peer-jobs","overlay":{"auth":{"bearer_credential_ref":"rotated-token"}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if v := out["version"].(float64); v != 2 {
		t.Errorf("version = %v, want 2 (v1 bootstrap + v2 fork)", v)
	}
	if out["promoted"].(bool) != true {
		t.Errorf("fork default promote = false; want true")
	}
	def := out["definition"].(map[string]any)
	// agent_card_url survived from the template; only the cred ref rotated.
	if def["agent_card_url"] != "https://peer.example/.well-known/agent-card.json" {
		t.Errorf("fork lost template agent_card_url; got %v", def["agent_card_url"])
	}
	auth := def["auth"].(map[string]any)
	if auth["bearer_credential_ref"] != "rotated-token" {
		t.Errorf("cred ref not rotated; got %v", auth["bearer_credential_ref"])
	}
}

// TestA2AAgentDefTool_ForkFlipsReachabilityMode verifies the overlay
// can flip a peer from card-URL to direct endpoint without leaving the
// stale agent_card_url that would trip the both-set validation refusal.
func TestA2AAgentDefTool_ForkFlipsReachabilityMode(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"peer-jobs","overlay":{"endpoint":"https://peer.example/a2a","binding":"grpc"}}`))
	if res.IsError {
		t.Fatalf("fork flip: %s", res.Text)
	}
	def := decodeResult(t, res.Text)["definition"].(map[string]any)
	if _, ok := def["agent_card_url"]; ok {
		t.Errorf("agent_card_url should be cleared after flipping to endpoint; got %v", def["agent_card_url"])
	}
	if def["binding"] != "grpc" {
		t.Errorf("binding = %v, want grpc", def["binding"])
	}
}

func TestA2AAgentDefTool_NoScopesIsDefaultDeny(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	ctx = tools.WithA2AAgentDefPolicy(ctx, tools.A2AAgentDefPolicyValue{})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"x","overlay":{"agent_card_url":"https://x.example"}}`))
	if !res.IsError {
		t.Fatalf("empty scopes should default-deny")
	}
	if !strings.Contains(res.Text, "default-deny") {
		t.Errorf("refusal should mention default-deny; got %s", res.Text)
	}
}

func TestA2AAgentDefTool_NamedScope(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	ctx = tools.WithA2AAgentDefPolicy(ctx, tools.A2AAgentDefPolicyValue{
		Scopes: []string{"named:adhoc-peer"},
	})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc-peer","overlay":{"agent_card_url":"https://x.example"}}`))
	if res.IsError {
		t.Fatalf("named scope should allow matching name; got %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"other","overlay":{"agent_card_url":"https://x.example"}}`))
	if !res.IsError {
		t.Fatalf("named scope should refuse non-matching name")
	}
}

func TestA2AAgentDefTool_RetireRoundTrip(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"retire-peer","overlay":{"agent_card_url":"https://x.example"}}`))
	defID := decodeResult(t, res.Text)["def_id"].(string)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":true}`))
	if res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	if decodeResult(t, res.Text)["retired"].(bool) != true {
		t.Errorf("retired = false, want true")
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":false}`))
	if res.IsError {
		t.Fatalf("un-retire: %s", res.Text)
	}
	if decodeResult(t, res.Text)["retired"].(bool) != false {
		t.Errorf("retired = true, want false")
	}
}

func TestA2AAgentDefTool_ListReturnsVersions(t *testing.T) {
	tool, ctx, cleanup := a2aAgentDefFixture(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		op := `create`
		if i > 0 {
			op = `fork`
		}
		_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","name":"multi-peer","overlay":{"agent_card_url":"https://x.example"}}`))
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"multi-peer"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	versions := decodeResult(t, res.Text)["versions"].([]any)
	if len(versions) != 3 {
		t.Errorf("got %d versions, want 3", len(versions))
	}
}

// TestMergedA2AAgentDef_DriftDetection_VsLookupSubstrate pins json-tag
// parity between mergedA2AAgentDef (substrate-write) and
// lookup.SubstrateA2AAgentDef (substrate-read).
func TestMergedA2AAgentDef_DriftDetection_VsLookupSubstrate(t *testing.T) {
	mergedTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(mergedA2AAgentDef{}))
	substrateTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(lookup.SubstrateA2AAgentDef{}))

	for tag := range mergedTags {
		if !substrateTags[tag] {
			t.Errorf("mergedA2AAgentDef has json tag %q but lookup.SubstrateA2AAgentDef does not — mirror it on the lookup side", tag)
		}
	}
	for tag := range substrateTags {
		if !mergedTags[tag] {
			t.Errorf("lookup.SubstrateA2AAgentDef has json tag %q but mergedA2AAgentDef does not — substrate-write is the source-of-truth shape", tag)
		}
	}
}
