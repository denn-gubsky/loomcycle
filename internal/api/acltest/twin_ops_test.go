package acltest

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A meta-tool whose ops are several HTTP routes asks each op its own route's
// question. Before, channeldef asked `subscribe channel:*` (its list route)
// for create, update, delete and purge, and retention_override asked `read
// runtime:retention` for set and clear, so a `manage` deny the routes honoured
// did not hold over MCP.

// bindDeny writes one list as the operator and binds it to subject.
func (e *env) bindDeny(subject, name, rules string) {
	e.t.Helper()
	if resp, raw := e.do("POST", "/v1/_acldef", operator, `{"op":"create","name":"`+name+`","rules":`+rules+`}`, nil); resp.StatusCode != 201 {
		e.t.Fatalf("create %s: %d %s", name, resp.StatusCode, raw)
	}
	if resp, raw := e.do("POST", "/v1/_aclbinding", operator, `{"acl":"`+name+`","subject_kind":"user","subject_id":"`+subject+`"}`, nil); resp.StatusCode != 201 {
		e.t.Fatalf("bind %s: %d %s", name, resp.StatusCode, raw)
	}
}

func TestACLTwins_AChannelManageDenyHoldsForEveryChannelDefOp(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	for _, name := range []string{"ops", "news"} {
		if resp, raw := e.do("POST", "/v1/_channels", operator, `{"name":"`+name+`","scope":"tenant"}`, nil); resp.StatusCode/100 != 2 {
			t.Fatalf("create %s: %d %s", name, resp.StatusCode, raw)
		}
	}
	e.bindDeny("delegate", "no-ops-manage", `[{"effect":"deny","actions":["manage"],"resources":["channel:ops"]}]`)
	if resp, raw := e.do("PATCH", "/v1/_channels/ops", delegate, `{"description":"x"}`, nil); resp.StatusCode != 403 || codeOf(raw) != "acl_denied" {
		t.Errorf("HTTP PATCH: %d %s", resp.StatusCode, raw)
	}
	for _, op := range []string{"purge", "update", "delete"} {
		if o := e.viaMCP(delegate, call{family: "channeldef", input: `{"op":"` + op + `","name":"ops","description":"x"}`}); o.code != "acl_denied" {
			t.Errorf("MCP channeldef %s of the denied channel: %+v", op, o)
		}
	}
	// Another channel is not covered by the deny.
	if o := e.viaMCP(delegate, call{family: "channeldef", input: `{"op":"update","name":"news","description":"x"}`}); o.code == "acl_denied" {
		t.Errorf("MCP channeldef update of another channel was refused: %+v", o)
	}
}

// Creating a channel asks `manage` of the name it is about to get, on HTTP
// and over MCP alike, so a deny on a name pattern holds for a new channel.
func TestACLTwins_AChannelManageDenyHoldsForCreateOnEveryTransport(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	e.bindDeny("delegate", "no-secret-channels", `[{"effect":"deny","actions":["manage"],"resources":["channel:secret-*"]}]`)
	if resp, raw := e.do("POST", "/v1/_channels", delegate, `{"name":"secret-a","scope":"tenant"}`, nil); resp.StatusCode != 403 || codeOf(raw) != "acl_denied" {
		t.Errorf("HTTP create: %d %s", resp.StatusCode, raw)
	}
	if o := e.viaMCP(delegate, call{family: "channeldef", input: `{"op":"create","name":"secret-b","scope":"tenant"}`}); o.code != "acl_denied" {
		t.Errorf("MCP channeldef create: %+v", o)
	}
	if resp, raw := e.do("POST", "/v1/_channels", delegate, `{"name":"public-a","scope":"tenant"}`, nil); resp.StatusCode/100 != 2 {
		t.Errorf("HTTP create of a name the deny does not cover: %d %s", resp.StatusCode, raw)
	}
}

func TestACLTwins_ARetentionManageDenyHoldsForTheMCPWrites(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	e.bindDeny("delegate", "no-retention-manage", `[{"effect":"deny","actions":["manage"],"resources":["runtime:retention"]}]`)
	if resp, raw := e.do("PUT", "/v1/_retention/overrides", delegate, `{"agent":"a","until":"2099-01-01T00:00:00Z"}`, nil); resp.StatusCode != 403 || codeOf(raw) != "acl_denied" {
		t.Errorf("HTTP PUT: %d %s", resp.StatusCode, raw)
	}
	for _, op := range []string{"set", "clear"} {
		if o := e.viaMCP(delegate, call{family: "retention_override", input: `{"op":"` + op + `","agent":"a","until":"2099-01-01T00:00:00Z"}`}); o.code != "acl_denied" {
			t.Errorf("MCP retention_override %s: %+v", op, o)
		}
	}
	// A read is not a manage: list still answers.
	if o := e.viaMCP(delegate, call{family: "retention_override", input: `{"op":"list"}`}); o.code == "acl_denied" {
		t.Errorf("MCP retention_override list was refused by a manage deny: %+v", o)
	}
}

// The erasure report is the read its GET route asks, not the execute's
// manage: a principal denied only manage still reads the report over MCP.
func TestACLTwins_TheErasureReportAsksReadOverMCP(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	e.bindDeny("delegate", "no-erasure-execute", `[{"effect":"deny","actions":["manage"],"resources":["runtime:erasure"]}]`)
	if o := e.viaMCP(delegate, call{family: "erasure", input: `{"op":"report","subject":"bob"}`}); o.code == "acl_denied" {
		t.Errorf("MCP erasure report was refused by a manage deny: %+v", o)
	}
	if o := e.viaMCP(delegate, call{family: "erasure", input: `{"op":"execute","subject":"bob","tier":"rows","dry_run":true}`}); o.code != "acl_denied" {
		t.Errorf("MCP erasure execute: %+v", o)
	}
}

// Releasing a team channel's held messages asks `manage` on every transport,
// as releasing any other channel's does.
func TestACLTwins_TeamChannelReleaseAsksManageOnEveryTransport(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	e.bindDeny("delegate", "no-channel-manage", `[{"effect":"deny","actions":["manage"],"resources":["channel:*"]}]`)
	if resp, raw := e.do("POST", "/v1/_teamdef/t/channels/c/release", delegate, `{}`, nil); resp.StatusCode != 403 || codeOf(raw) != "acl_denied" {
		t.Errorf("HTTP: %d %s", resp.StatusCode, raw)
	}
	asDelegate := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+delegate)
	if _, err := e.grpc.ReleaseTeamChannel(asDelegate, &loomcyclepb.ReleaseTeamChannelRequest{Team: "t", Name: "c"}); !grpcDenied(err) {
		t.Errorf("gRPC: %v", err)
	}
	if o := e.viaMCP(delegate, call{family: "release_team_channel", input: `{"team":"t","name":"c"}`}); o.code != "acl_denied" {
		t.Errorf("MCP: %+v", o)
	}
}

// An op a multi-route meta-tool does not map is refused before its handler
// runs, never let through on the tool's weakest question.
func TestACLTwins_AnUnmappedOpOfAMultiRouteToolIsRefused(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	for _, tool := range []string{"channeldef", "retention_override", "erasure"} {
		if o := e.viaMCP(delegate, call{family: tool, input: `{"op":"no_such_op","name":"ops"}`}); o.code != "unknown_op" {
			t.Errorf("MCP %s no_such_op: %+v", tool, o)
		}
	}
}

// The per-op gate reads the op and the name exactly as the handler does, and
// refuses an argument object naming either twice — a duplicate key or a case
// variant — so it cannot ask one op's question (or one channel's) while the
// handler, whose struct decoding matches keys case-insensitively and keeps
// the last, runs another.
func TestACLTwins_AnAmbiguousOpOrNameIsRefused(t *testing.T) {
	e := newEnv(t, authz.ModeCompat)
	for _, name := range []string{"ops", "news"} {
		if resp, raw := e.do("POST", "/v1/_channels", operator, `{"name":"`+name+`","scope":"tenant"}`, nil); resp.StatusCode/100 != 2 {
			t.Fatalf("create %s: %d %s", name, resp.StatusCode, raw)
		}
	}
	e.bindDeny("delegate", "no-manage", `[{"effect":"deny","actions":["manage"],"resources":["runtime:retention","runtime:erasure","channel:ops"]}]`)
	soon := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []call{
		// until within the 24-hour cap, so on the unfixed gate the set really
		// runs rather than failing validation.
		{family: "retention_override", input: `{"op":"list","OP":"set","agent":"a","until":"` + soon + `"}`},
		{family: "retention_override", input: `{"op":"list","Op":"clear","agent":"a"}`},
		{family: "retention_override", input: `{"op":"list","op":"clear","agent":"a"}`},
		{family: "erasure", input: `{"op":"report","oP":"execute","subject":"bob","tier":"rows","dry_run":true}`},
		{family: "erasure", input: `{"OP":"execute","subject":"bob","tier":"rows","dry_run":true}`},
		{family: "channeldef", input: `{"op":"update","name":"news","Name":"ops","description":"x"}`},
		{family: "channeldef", input: `{"op":"delete","name":"news","NAME":"ops"}`},
	} {
		o := e.viaMCP(delegate, c)
		if o.code != "ambiguous_arguments" && o.code != "acl_denied" {
			t.Errorf("MCP %s %s: %+v, want refused", c.family, c.input, o)
		}
	}
	// The denied channel still exists: no ambiguous call reached its handler.
	if resp, raw := e.do("GET", "/v1/_channels", operator, ``, nil); resp.StatusCode != 200 || !strings.Contains(string(raw), `"ops"`) {
		t.Errorf("the denied channel is gone: %d %s", resp.StatusCode, raw)
	}
}

// `mount volume:<name>` is the question a member's agent definition asks of
// each volume it grants (the member capability ceiling), so a tenant rule can
// narrow it: writing the deny is accepted, and a member's definition granting
// that volume is refused while another volume is still granted.
func TestMemberCeiling_AMountDenyKeepsTheVolumeFromAMembersAgent(t *testing.T) {
	e := newEnvWith(t, authz.ModeCompat, func(cfg *config.Config, st store.Store) ([]tools.Tool, func(*lchttp.Server)) {
		cfg.Volumes = map[string]config.Volume{
			"secret": {Path: t.TempDir(), Mode: "rw"},
			"public": {Path: t.TempDir(), Mode: "rw"},
		}
		return []tools.Tool{&builtin.AgentDef{Store: st, Cfg: cfg}}, func(*lchttp.Server) {}
	})
	e.bindDeny("alice", "no-secret-mount", `[{"effect":"deny","actions":["mount"],"resources":["volume:secret"]}]`)
	create := func(name, vol string) (int, string) {
		resp, raw := e.do("POST", "/v1/_agentdef", member, `{"op":"create","name":"`+name+`","overlay":{"system_prompt":"x","tools":["Read"],"volumes":["`+vol+`"]}}`, nil)
		return resp.StatusCode, string(raw)
	}
	if code, raw := create("with-secret", "secret"); code/100 == 2 || !strings.Contains(raw, "secret") {
		t.Errorf("a member's agent granting the denied volume: %d %s", code, raw)
	}
	if code, raw := create("with-public", "public"); code/100 != 2 {
		t.Errorf("a member's agent granting another volume: %d %s", code, raw)
	}
}
