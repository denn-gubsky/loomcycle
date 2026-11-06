package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
)

// auditLog keeps every audit event as JSON.
type auditLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (a *auditLog) Record(ev audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return json.NewEncoder(&a.buf).Encode(ev)
}

func (a *auditLog) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.String()
}

// syncBuffer is a log sink safe for the poller's goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// An OAuth credential's tokens, refresh token and device code reach exactly
// one place: the Authorization header of a request to the resource. Not a
// log line, a tool result, the audit trail, Context op=self, a snapshot or
// its findings, nor an error — even one built from a server answer that
// quotes the token. The access token is registered with the redactor.
func TestOAuthToken_NeverLeaks(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	engine := credential.NewEngine(st, sealer)

	f := oauthtest.New(t)
	var seenMu sync.Mutex
	var seenAuth []string
	f.Set(func(f *oauthtest.Server) {
		f.Interval = 1
		f.AccessTTL = 30 // inside the refresh threshold: every send refreshes
		f.ResourceHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seenMu.Lock()
			seenAuth = append(seenAuth, r.Header.Get("Authorization"))
			seenMu.Unlock()
			body, _ := io.ReadAll(r.Body)
			var probe struct {
				ID *int64 `json:"id"`
			}
			_ = json.Unmarshal(body, &probe)
			if probe.ID == nil {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			oauthtest.WriteJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": *probe.ID,
				"result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "docs", "version": "1"}}})
		})
	})
	audits := &auditLog{}
	mgr := credential.NewOAuthManager(engine, f.Client(), "replica-test", audits)
	bg, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr.SetBackground(bg)

	tool := &builtin.CredentialDef{Engine: engine, OAuth: mgr, Audit: audits}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a", UserID: "alice", TenantID: "t"})
	var texts []string
	call := func(body string) map[string]any {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(body))
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, res.Text)
		if res.IsError {
			t.Fatalf("%s: %s", body, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	call(`{"op":"create","usage":"oauth","scope":"tenant","name":"docs","oauth":{"resource":"` + f.Resource() + `"}}`)
	call(`{"op":"oauth_connect_start","scope":"tenant","name":"docs"}`)
	for deadline := time.Now().Add(15 * time.Second); ; {
		out := call(`{"op":"oauth_connect_status","scope":"tenant","name":"docs"}`)
		if oa, _ := out["oauth"].(map[string]any); oa["state"] == "connected" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
		time.Sleep(100 * time.Millisecond)
	}

	var registered []string
	var regMu sync.Mutex
	register := func(_ context.Context, _ string, v string) {
		regMu.Lock()
		defer regMu.Unlock()
		registered = append(registered, v)
	}
	client, err := mcphttp.New(mcphttp.Config{URL: f.Resource(), HTTPClient: f.Client(),
		Headers:        map[string]string{"Authorization": "Bearer $oauth:docs"},
		CredSubstitute: newCredSubstitute(engine, register, nil, mgr)})
	if err != nil {
		t.Fatal(err)
	}
	var errs []string
	for range 2 { // two sends: each refreshes, rotating the refresh token
		if _, err := mcp.Initialize(ctx, client, "t", "1"); err != nil {
			t.Fatalf("initialize: %v", err)
		}
	}
	seenMu.Lock()
	sent := slices.Clone(seenAuth)
	seenMu.Unlock()
	if len(sent) == 0 || sent[len(sent)-1] != "Bearer "+f.LastAccess() {
		t.Fatalf("the resource saw %d requests, last not the current token", len(sent))
	}
	regMu.Lock()
	if !slices.Contains(registered, f.LastAccess()) {
		t.Error("the access token was not registered with the redactor")
	}
	regMu.Unlock()

	// A 500 whose body quotes the refresh token, then a revoked grant whose
	// error_description quotes it: neither reaches an error, a log or audit.
	f.Set(func(f *oauthtest.Server) { f.Fail5xx = 1 })
	if _, err := mcp.Initialize(ctx, client, "t", "1"); err != nil {
		errs = append(errs, err.Error())
	}
	var rts []string
	f.Set(func(f *oauthtest.Server) { rts = slices.Clone(f.IssuedRefresh) })
	resp, err := f.Client().PostForm(f.Srv.URL+"/revoke", url.Values{"token": {rts[len(rts)-1]}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	time.Sleep(2100 * time.Millisecond) // past the 5xx backoff
	if _, err := mcp.Initialize(ctx, client, "t", "1"); err == nil {
		t.Fatal("a revoked grant still sent")
	} else {
		errs = append(errs, err.Error())
	}
	call(`{"op":"get","scope":"tenant","name":"docs"}`)
	call(`{"op":"list","scope":"tenant"}`)

	self, err := (&builtin.Context{}).Execute(ctx, json.RawMessage(`{"op":"self"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, env, err := snapshot.Capture(context.Background(), st, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	haystacks := map[string]string{
		"logs": logs.String(), "tool results": strings.Join(texts, "\n"), "audit": audits.String(),
		"Context op=self": self.Text, "snapshot": string(env), "errors": strings.Join(errs, "\n"),
	}
	secrets := f.Secrets()
	if len(secrets) < 4 {
		t.Fatalf("only %d secrets issued; the test exercised too little", len(secrets))
	}
	for where, hay := range haystacks {
		for _, s := range secrets {
			if strings.Contains(hay, s) {
				t.Errorf("%s carries a secret (%.8s…)", where, s)
			}
		}
	}
	if !strings.Contains(audits.String(), "oauth_needs_reauth") || !strings.Contains(audits.String(), "oauth_connected") {
		t.Errorf("the audit trail lacks the connect and the lost grant: %s", audits.String())
	}
}
