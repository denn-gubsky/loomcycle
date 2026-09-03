package builtin

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/recall"
)

type recallBagEmbedder struct{ dim int }

func (e recallBagEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, e.dim)
		for _, tok := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(tok))
			v[h.Sum32()%uint32(e.dim)]++
		}
		out[i] = v
	}
	return out, nil
}

func (e recallBagEmbedder) Model() string    { return "bag" }
func (e recallBagEmbedder) Provider() string { return "test" }
func (e recallBagEmbedder) Dimension() int   { return e.dim }

func TestRecall_SearchesRunIndex(t *testing.T) {
	ix := recall.NewIndex(recallBagEmbedder{dim: 256}, 0)
	ix.Harvest(context.Background(), []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "the license key is maple-7731 do not lose it"}}},
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "the weather was nice all afternoon"}}},
	})
	ctx := recall.NewContext(context.Background(), ix)

	tool := &Recall{} // no Memory — run-index only
	res, err := tool.Execute(ctx, json.RawMessage(`{"query":"what is the license key"}`))
	if err != nil || res.IsError {
		t.Fatalf("Execute: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Text, "maple-7731") {
		t.Fatalf("recalled text missing the needle: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"source":"run"`) {
		t.Fatalf("run-index hit not tagged source=run: %s", res.Text)
	}
}

func TestRecall_TopKClampAndDefault(t *testing.T) {
	ix := recall.NewIndex(recallBagEmbedder{dim: 128}, 0)
	for _, s := range []string{"alpha term", "beta term", "gamma term", "delta term", "epsilon term"} {
		ix.Harvest(context.Background(), []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: s}}}})
	}
	ctx := recall.NewContext(context.Background(), ix)
	tool := &Recall{}

	// Default top_k = 3.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"query":"term"}`))
	var got map[string][]map[string]any
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, res.Text)
	}
	if n := len(got["recalled"]); n != 3 {
		t.Fatalf("default returned %d hits, want 3", n)
	}
}

func TestRecall_EmptyWhenNothingAvailable(t *testing.T) {
	// No run index on ctx and no Memory backing: a clean, non-error "nothing".
	tool := &Recall{}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"anything"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("empty recall should not be an error: %+v", res)
	}
	if !strings.Contains(res.Text, "No earlier details") {
		t.Fatalf("unexpected empty-recall text: %s", res.Text)
	}
}

func TestRecall_MissingQuery(t *testing.T) {
	tool := &Recall{}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"  "}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("a blank query should be a tool error")
	}
}

func TestRecall_InvalidInput(t *testing.T) {
	tool := &Recall{}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{not json`))
	if !res.IsError {
		t.Fatal("invalid JSON input should be a tool error")
	}
}
