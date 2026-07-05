package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentDefTool_ForkPopulatesContentSHA256(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"v2","max_iterations":32}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)

	hash, _ := out["content_sha256"].(string)
	if !strings.HasPrefix(hash, "sha256:") || len(hash) != 71 {
		t.Errorf("fork response content_sha256 = %q (want sha256:<64hex>)", hash)
	}

	defID, _ := out["def_id"].(string)
	row, err := tool.Store.AgentDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("AgentDefGet: %v", err)
	}
	if row.ContentSHA256 != hash {
		t.Errorf("DB row ContentSHA256 = %q, response said %q", row.ContentSHA256, hash)
	}
}

func TestAgentDefTool_ForkSameOverlayProducesSameHash(t *testing.T) {
	// Different rows (different def_id, version) but identical content
	// MUST hash identically. This is the "operator's bundle == deployed"
	// invariant: same input bytes always yield the same hash.
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	overlay := `{"op":"fork","name":"researcher","overlay":{"system_prompt":"same","tools":["Read"]}}`

	res1, _ := tool.Execute(ctx, json.RawMessage(overlay))
	if res1.IsError {
		t.Fatalf("fork 1: %s", res1.Text)
	}
	res2, _ := tool.Execute(ctx, json.RawMessage(overlay))
	if res2.IsError {
		t.Fatalf("fork 2: %s", res2.Text)
	}

	h1, _ := decodeResult(t, res1.Text)["content_sha256"].(string)
	h2, _ := decodeResult(t, res2.Text)["content_sha256"].(string)
	if h1 == "" || h2 == "" {
		t.Fatalf("missing hashes: h1=%q h2=%q", h1, h2)
	}
	if h1 != h2 {
		t.Errorf("identical content yielded different hashes: %s vs %s", h1, h2)
	}
}

func TestAgentDefTool_ForkDifferentContentDifferentHash(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res1, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"v1"}}`))
	res2, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"v2"}}`))

	h1, _ := decodeResult(t, res1.Text)["content_sha256"].(string)
	h2, _ := decodeResult(t, res2.Text)["content_sha256"].(string)
	if h1 == h2 {
		t.Errorf("different system_prompt didn't move the hash; both %s", h1)
	}
}

// TestAgentDefTool_ForkInteractiveConfigMovesHash is the F14 regression: a
// fork that changes ONLY channels / interruption / evaluation_scopes must
// move the content hash. Pre-F14 those fields were excluded from the hash,
// so such a fork produced an IDENTICAL content_sha256 to its parent and the
// execCreate/dedup path treated it as a no-op duplicate — silently dropping
// a real ACL change. Each sub-fork below must differ from the baseline.
func TestAgentDefTool_ForkInteractiveConfigMovesHash(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	base, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"v1"}}`))
	if base.IsError {
		t.Fatalf("base fork: %s", base.Text)
	}
	baseHash := decodeResult(t, base.Text)["content_sha256"].(string)

	cases := map[string]string{
		"channels":          `{"op":"fork","name":"researcher","overlay":{"system_prompt":"v1","channels":{"publish":["alerts"]}}}`,
		"evaluation_scopes": `{"op":"fork","name":"researcher","overlay":{"system_prompt":"v1","evaluation_scopes":["submit_self"]}}`,
		"interruption":      `{"op":"fork","name":"researcher","overlay":{"system_prompt":"v1","interruption":{"enabled":true,"max_pending":2}}}`,
	}
	for field, overlay := range cases {
		res, _ := tool.Execute(ctx, json.RawMessage(overlay))
		if res.IsError {
			t.Fatalf("%s fork: %s", field, res.Text)
		}
		h := decodeResult(t, res.Text)["content_sha256"].(string)
		if h == baseHash {
			t.Errorf("%s-only fork did NOT move the content hash (still %s) — would be wrongly deduped (F14)", field, baseHash)
		}
	}
}

func TestAgentDefTool_GetSurfacesContentSHA256(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// Fork to mint a row with a hash.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"x"}}`))
	defID := decodeResult(t, res.Text)["def_id"].(string)

	// Now get it.
	getRes, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if getRes.IsError {
		t.Fatalf("get: %s", getRes.Text)
	}
	got := decodeResult(t, getRes.Text)
	if h, _ := got["content_sha256"].(string); !strings.HasPrefix(h, "sha256:") {
		t.Errorf("get response missing content_sha256: %+v", got)
	}
}

func TestAgentDefTool_VerifyMatchesOnSameHash(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	// Fork + promote one row so verify has an active deployment to
	// answer against.
	forkRes, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"deployed"},"promote":true}`))
	deployedHash := decodeResult(t, forkRes.Text)["content_sha256"].(string)

	verifyRes, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"researcher","content_sha256":"`+deployedHash+`"}`))
	if verifyRes.IsError {
		t.Fatalf("verify: %s", verifyRes.Text)
	}
	out := decodeResult(t, verifyRes.Text)
	if matches, _ := out["matches"].(bool); !matches {
		t.Errorf("matches = false, want true: %+v", out)
	}
	if got, _ := out["current_sha256"].(string); got != deployedHash {
		t.Errorf("current_sha256 = %q, want %q", got, deployedHash)
	}
	if deployed, _ := out["deployed"].(bool); !deployed {
		t.Error("deployed = false")
	}
}

func TestAgentDefTool_VerifyFalseOnDifferentHash(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"deployed"},"promote":true}`))

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"researcher","content_sha256":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`))
	if res.IsError {
		t.Fatalf("verify: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if m, _ := out["matches"].(bool); m {
		t.Errorf("matches = true on different hash: %+v", out)
	}
	if h, _ := out["current_sha256"].(string); !strings.HasPrefix(h, "sha256:") {
		t.Errorf("current_sha256 = %q (want sha256:<hex>)", h)
	}
}

func TestAgentDefTool_VerifyDeployedFalseOnUnknownName(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"never-existed","content_sha256":"sha256:abc"}`))
	if res.IsError {
		t.Fatalf("verify: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if m, _ := out["matches"].(bool); m {
		t.Error("matches=true on unknown name")
	}
	if d, _ := out["deployed"].(bool); d {
		t.Error("deployed=true on unknown name")
	}
	if h, _ := out["current_sha256"].(string); h != "" {
		t.Errorf("current_sha256 = %q (want empty)", h)
	}
}

func TestAgentDefTool_VerifyFalseWhenCallerOmitsHash(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"deployed"},"promote":true}`))

	// Omitting content_sha256 from the input must NEVER report matches=true
	// (avoid the empty-string == empty-string trap that would falsely
	// report "in sync" against a row whose hash hasn't been backfilled yet).
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"verify","name":"researcher"}`))
	if res.IsError {
		t.Fatalf("verify: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if m, _ := out["matches"].(bool); m {
		t.Errorf("matches = true on empty caller hash: %+v", out)
	}
}

func TestAgentDefTool_ListIncludesContentSHA256(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"researcher","overlay":{"system_prompt":"v2"}}`))

	listRes, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"researcher"}`))
	if listRes.IsError {
		t.Fatalf("list: %s", listRes.Text)
	}
	var listOut struct {
		Versions []map[string]any `json:"versions"`
	}
	if err := json.Unmarshal([]byte(listRes.Text), &listOut); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listOut.Versions) == 0 {
		t.Fatalf("list returned no versions: %s", listRes.Text)
	}
	for i, d := range listOut.Versions {
		h, _ := d["content_sha256"].(string)
		if !strings.HasPrefix(h, "sha256:") {
			t.Errorf("version[%d] missing content_sha256: %+v", i, d)
		}
	}
}
