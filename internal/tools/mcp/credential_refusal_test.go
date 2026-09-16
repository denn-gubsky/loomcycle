package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
)

// THE CROSSING. The HTTP client refuses an unresolved per-run credential, but
// what an AGENT sees is a tools.Result — and the classification has to survive
// the hop from the client's error, through mcpTool.Execute, onto that Result.
// Testing either end proves nothing about the hop: the client test asserts an
// error, and a classifier test asserts a category, and deleting the wiring
// between them leaves both green.
//
// So this drives the real dispatch and asserts on the Result the model is
// handed, plus the fact that nothing reached the peer.
func TestMCPTool_MissingRunCredentialIsRefusedAndClassified(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := mcphttp.New(mcphttp.Config{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${run.credentials.jobs}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) { return client, nil }, nil, nil)
	tool := mcp.NewTool(pool, "jobs", mcp.ToolDescriptor{Name: "search"})

	// A run that carries no credentials at all — which is exactly the state a
	// restored run is in, since per-run secrets never enter the snapshot.
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_resumed"})
	res, execErr := tool.Execute(ctx, json.RawMessage(`{}`))
	if execErr != nil {
		t.Fatalf("Execute returned a hard error, want a classified tool result: %v", execErr)
	}

	if !res.IsError {
		t.Fatal("the call reported success without the credential it declares")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the peer received %d request(s); a refused call must not be sent at all", got)
	}
	if res.Error == nil {
		t.Fatal("the refusal reached the model with no category — an agent can only guess whether to retry")
	}
	if res.Error.Category != tools.CategoryBusiness {
		t.Errorf("category = %q, want business", res.Error.Category)
	}
	if res.Error.Retryable {
		t.Error("marked retryable — no number of retries produces a credential the run does not have")
	}
	if res.Error.Description == "" {
		t.Error("no description: the category alone does not tell the agent what to do instead")
	}
	// The description is model-visible prompt text; it must not carry operator
	// config. The peer URL is the one thing conveniently to hand here.
	if strings.Contains(res.Error.Description, srv.URL) {
		t.Error("the description leaks the peer URL into model-visible text")
	}
}

// The operator's documented opt-out: the POSIX fallback form says "proceeding
// without this value is intended". It must still resolve and still be sent —
// otherwise the refusal would have no escape hatch and would break every
// deployment that relies on it.
func TestMCPTool_FallbackFormStillSendsTheCall(t *testing.T) {
	var seen atomic.Value // string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer srv.Close()

	client, err := mcphttp.New(mcphttp.Config{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${run.user_bearer:-anonymous}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) { return client, nil }, nil, nil)
	tool := mcp.NewTool(pool, "jobs", mcp.ToolDescriptor{Name: "search"})

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_nobearer"})
	res, execErr := tool.Execute(ctx, json.RawMessage(`{}`))
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if res.Error != nil {
		t.Errorf("the fallback form was refused (%v); it is the opt-out and must resolve", res.Error)
	}
	if got, _ := seen.Load().(string); got != "Bearer anonymous" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer anonymous")
	}
}
