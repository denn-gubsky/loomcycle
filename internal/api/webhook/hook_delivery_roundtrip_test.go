package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

// A signed hook delivery verifies at loomcycle's own inbound receiver, with the
// receiver's default auth (hmac, X-Loomcycle-Signature): loomcycle can verify
// loomcycle. A delivery checked against a different secret, or after the
// tolerance, does not.
func TestVerifySignature_AcceptsASignedHookDelivery(t *testing.T) {
	const secretEnv = "LOOMCYCLE_TEST_HOOK_SIGNING"
	const secret = "roundtrip-signing-secret"
	type seen struct {
		header http.Header
		body   []byte
	}
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Header.Clone(), b}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	s := hooks.NewSet()
	if _, err := s.Register(&hooks.Hook{Owner: "agent:w", Name: "gate", Phase: hooks.PhasePre, CallbackURL: srv.URL, SigningSecret: "$cred:sign"}); err != nil {
		t.Fatal(err)
	}
	d := hooks.NewDispatcher(s, nil)
	d.SetHeaderSubstitute(func(_ context.Context, ref, _ string) (string, []string, error) {
		if ref == "$cred:sign" {
			return secret, nil, nil
		}
		return ref, []string{ref}, nil
	})
	if out := d.RunPre(context.Background(), hooks.Identity{Agent: "w"}, hooks.ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://example.com"}`)}); out.Deny != nil {
		t.Fatalf("denied: %+v", out.Deny)
	}
	req := <-got

	auth := config.WebhookAuth{Kind: "hmac", SigningSecretEnv: secretEnv}
	getenv := func(name string) string {
		switch name {
		case secretEnv:
			return secret
		case "LOOMCYCLE_TEST_OTHER":
			return "a-different-secret"
		}
		return ""
	}
	sent := time.Now()
	// The receiver's own path: resolve the def's secrets, then verify.
	verifyBody := func(a config.WebhookAuth, body []byte, at time.Time) error {
		secrets, err := verificationSecrets(context.Background(), a, "", nil, nil, getenv, t.Logf)
		if err != nil {
			return err
		}
		return verifySignature(a, secrets, body, req.header.Get, func() time.Time { return at })
	}
	if err := verifyBody(auth, req.body, sent); err != nil {
		t.Fatalf("the receiver refused a signed hook delivery: %v (header %q)", err, req.header.Get("X-Loomcycle-Signature"))
	}
	if err := verifyBody(config.WebhookAuth{Kind: "hmac", SigningSecretEnv: "LOOMCYCLE_TEST_OTHER"}, req.body, sent); err == nil {
		t.Fatal("the delivery verified against another secret")
	}
	if err := verifyBody(auth, req.body, sent.Add(signatureTolerance+time.Minute)); err == nil {
		t.Fatal("a delivery older than the tolerance verified")
	}
	tampered := append([]byte(nil), req.body...)
	tampered[len(tampered)-2] ^= 1
	if err := verifyBody(auth, tampered, sent); err == nil {
		t.Fatal("a tampered body verified")
	}
	// Mid-rotation the receiver holds two secrets; the delivery signed with
	// either one verifies.
	if err := verifySignature(auth, []string{"the-new-secret", secret}, req.body, req.header.Get, func() time.Time { return sent }); err != nil {
		t.Fatalf("a delivery signed with the previous secret was refused mid-rotation: %v", err)
	}
}
