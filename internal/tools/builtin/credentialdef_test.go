package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func credFixture(t *testing.T, userID string, kek string) (*CredentialDef, context.Context) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(kek, "")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	tool := &CredentialDef{Engine: credential.NewEngine(st, sealer)}
	ctx := tools.WithAgentName(context.Background(), "agent-1")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: userID, TenantID: "tnt"})
	return tool, ctx
}

func credExec(t *testing.T, c *CredentialDef, ctx context.Context, body string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := c.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute hard error: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		_ = json.Unmarshal([]byte(res.Text), &out)
	}
	return out, res
}

func testKEK() string { return base64.StdEncoding.EncodeToString(make([]byte, 32)) }

func TestCredentialDefTool_CreateGetIsMetadataOnly(t *testing.T) {
	c, ctx := credFixture(t, "u1", testKEK())

	out, res := credExec(t, c, ctx, `{"op":"create","scope":"tenant","name":"serper","value":"sk-secret-xyz"}`)
	if res.IsError {
		t.Fatalf("create: %q", res.Text)
	}
	// The create result must never echo the plaintext value.
	if strings.Contains(res.Text, "sk-secret-xyz") {
		t.Errorf("create result leaked the plaintext value: %s", res.Text)
	}
	if out["name"] != "serper" || out["backend"] != "inline" {
		t.Errorf("create meta = %s", res.Text)
	}

	// get returns metadata, never the value.
	_, res = credExec(t, c, ctx, `{"op":"get","scope":"tenant","name":"serper"}`)
	if res.IsError || strings.Contains(res.Text, "sk-secret-xyz") {
		t.Errorf("get should return metadata only, no value: %s (err=%v)", res.Text, res.IsError)
	}
}

func TestCredentialDefTool_UserScopeStampsOwnSubject(t *testing.T) {
	// user u1 stores a user-scoped token.
	c1, ctx1 := credFixture(t, "u1", testKEK())
	if _, res := credExec(t, c1, ctx1, `{"op":"create","scope":"user","name":"telegram","value":"u1-token"}`); res.IsError {
		t.Fatalf("u1 create: %q", res.Text)
	}
	// A different user (u2) on the SAME engine/store can't see u1's user-scoped
	// credential — the tool stamps scope_id from the caller's own subject.
	c2 := &CredentialDef{Engine: c1.Engine}
	ctx2 := tools.WithRunIdentity(tools.WithAgentName(context.Background(), "agent-1"),
		tools.RunIdentityValue{AgentID: "a", UserID: "u2", TenantID: "tnt"})
	_, res := credExec(t, c2, ctx2, `{"op":"get","scope":"user","name":"telegram"}`)
	if !res.IsError {
		t.Errorf("user u2 resolved u1's user-scoped credential — isolation breach: %s", res.Text)
	}
	// u2 lists their own (empty) user bucket — must not include u1's.
	out, _ := credExec(t, c2, ctx2, `{"op":"list","scope":"user"}`)
	if creds, _ := out["credentials"].([]any); len(creds) != 0 {
		t.Errorf("u2's user-scope list leaked u1's credential: %v", creds)
	}
}

func TestCredentialDefTool_FailClosedNoKEK(t *testing.T) {
	c, ctx := credFixture(t, "u1", "") // no KEK
	_, res := credExec(t, c, ctx, `{"op":"create","scope":"tenant","name":"x","value":"v"}`)
	if !res.IsError || !strings.Contains(res.Text, "LOOMCYCLE_SECRET_KEY") {
		t.Errorf("create with no KEK should fail pointing at LOOMCYCLE_SECRET_KEY; got %q (err=%v)", res.Text, res.IsError)
	}
}

func TestCredentialDefTool_ScopeUserRequiresUserID(t *testing.T) {
	c, ctx := credFixture(t, "", testKEK()) // no user id on the run
	_, res := credExec(t, c, ctx, `{"op":"create","scope":"user","name":"x","value":"v"}`)
	if !res.IsError {
		t.Error("scope=user with no user_id on the run should error")
	}
}

func TestMaskCredentialCreateValue_ViaTool(t *testing.T) {
	// The redaction of the create `value` lives in internal/api/http
	// (maskCredentialCreateValue); here we just assert the tool's own result
	// never contains the value (covered above) and that a bad-name create is
	// refused before any store write.
	c, ctx := credFixture(t, "u1", testKEK())
	if _, res := credExec(t, c, ctx, `{"op":"create","scope":"tenant","name":"bad name!","value":"v"}`); !res.IsError {
		t.Error("create with an invalid name charset should be refused")
	}
}
