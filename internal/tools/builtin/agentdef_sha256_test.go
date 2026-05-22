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

	overlay := `{"op":"fork","name":"researcher","overlay":{"system_prompt":"same","allowed_tools":["Read"]}}`

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
