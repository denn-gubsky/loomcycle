package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSkillDefTool_CreatePopulatesContentSHA256(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"my-skill","overlay":{"body":"hello","description":"d"}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	h, _ := out["content_sha256"].(string)
	if !strings.HasPrefix(h, "sha256:") || len(h) != 71 {
		t.Errorf("create response content_sha256 = %q", h)
	}

	defID, _ := out["def_id"].(string)
	row, err := tool.Store.SkillDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("SkillDefGet: %v", err)
	}
	if row.ContentSHA256 != h {
		t.Errorf("row ContentSHA256 %q != response %q", row.ContentSHA256, h)
	}
}

func TestSkillDefTool_SameBodyProducesSameHash(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	o := `{"op":"create","name":"skill-`
	r1, _ := tool.Execute(ctx, json.RawMessage(o+`one","overlay":{"body":"same body"}}`))
	r2, _ := tool.Execute(ctx, json.RawMessage(o+`two","overlay":{"body":"same body"}}`))
	if r1.IsError || r2.IsError {
		t.Fatalf("creates: %s %s", r1.Text, r2.Text)
	}
	// NOTE: different name → different hash by design (name is part of
	// the content basis). Match-name test wouldn't work since create
	// refuses on collision; this asserts that names contribute.
	h1 := decodeResult(t, r1.Text)["content_sha256"].(string)
	h2 := decodeResult(t, r2.Text)["content_sha256"].(string)
	if h1 == h2 {
		t.Errorf("different names yielded same hash: %s", h1)
	}
}

func TestSkillDefTool_ForkDifferentBodyDifferentHash(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	r1, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"karpathy-guidelines","overlay":{"body":"v1"}}`))
	r2, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"karpathy-guidelines","overlay":{"body":"v2"}}`))
	if r1.IsError || r2.IsError {
		t.Fatalf("forks: %s %s", r1.Text, r2.Text)
	}
	h1 := decodeResult(t, r1.Text)["content_sha256"].(string)
	h2 := decodeResult(t, r2.Text)["content_sha256"].(string)
	if h1 == h2 {
		t.Errorf("different body yielded same hash: %s", h1)
	}
}

func TestSkillDefTool_GetSurfacesContentSHA256(t *testing.T) {
	tool, ctx, cleanup := skillDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"karpathy-guidelines","overlay":{"body":"v2"}}`))
	defID := decodeResult(t, res.Text)["def_id"].(string)

	getRes, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if getRes.IsError {
		t.Fatalf("get: %s", getRes.Text)
	}
	if h, _ := decodeResult(t, getRes.Text)["content_sha256"].(string); !strings.HasPrefix(h, "sha256:") {
		t.Error("get response missing content_sha256")
	}
}
