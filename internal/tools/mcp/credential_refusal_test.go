package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// A RUN-LESS request keeps the old drop-and-warn, and this is the half the
// first version of the refusal got wrong.
//
// The same Client.do serves both a per-run tool call and the boot-time
// enumeration handshake, and the handshake runs on a bare context.Background().
// There is no run there, so "this run does not carry the credential" is not a
// statement about anything — and refusing meant every static MCP server whose
// headers use a per-run credential failed to enumerate at boot and was marked
// skipped. That is the whole per-user MCP pattern, broken before any run
// exists.
//
// Caught by the suite, not by a probe: every probe drove the tool-call path,
// where a run identity is always present.
func TestMCPClient_RunlessHandshakeStillEnumerates(t *testing.T) {
	var sawAuth atomic.Value // string
	var handshakes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		handshakes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"` +
			mcp.ProtocolVersion + `","capabilities":{},"serverInfo":{"name":"f","version":"1"}}}`))
	}))
	defer srv.Close()

	client, err := mcphttp.New(mcphttp.Config{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${run.credentials.user_token}"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Exactly what cmd/loomcycle does at boot: no run identity on ctx at all.
	if _, err := mcp.Initialize(context.Background(), client, "loomcycle", "test"); err != nil {
		t.Fatalf("a run-less handshake was refused (%v); every static MCP server "+
			"using a per-run credential would fail to enumerate at boot", err)
	}
	if handshakes.Load() == 0 {
		t.Fatal("the handshake never reached the server")
	}
	if got, _ := sawAuth.Load().(string); got != "" {
		t.Errorf("Authorization = %q, want empty — the header is dropped when there is no run to authenticate as", got)
	}
}

// The refusal must not be RETRIED. GetWithRetry exists for a peer that is still
// booting and backs off up to 16s per attempt until the ctx dies; spending that
// on a not-retryable condition is the failure mode the classification exists to
// prevent, and here it would be loomcycle doing it to itself while a run waits.
func TestMCPPool_CredentialRefusalIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := mcphttp.New(mcphttp.Config{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${run.credentials.user_token}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) { return client, nil }, nil, nil)

	// A RUN with no credential — so the refusal fires — and a generous ctx, so
	// nothing but the early return can stop the backoff loop.
	ctx, cancel := context.WithTimeout(
		tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_norcred"}),
		30*time.Second)
	defer cancel()

	start := time.Now()
	_, _, err = pool.GetWithRetry(ctx, "jobs", func(string, ...any) {})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the handshake to fail")
	}
	if !errors.Is(err, tools.ErrRunCredentialUnavailable) {
		t.Errorf("error = %v, want one wrapping ErrRunCredentialUnavailable", err)
	}
	// The first backoff alone is 500ms; anything near it means it looped.
	if elapsed > 400*time.Millisecond {
		t.Errorf("took %s — it backed off and retried a not-retryable refusal", elapsed)
	}
	if got := attempts.Load(); got != 0 {
		t.Errorf("the peer saw %d request(s); a refused handshake sends none, let alone repeatedly", got)
	}
}
