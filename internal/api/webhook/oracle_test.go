package webhook

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// TestWebhook_BadSignatureIsIndistinguishableFromUnknownName: an
// unauthenticated caller cannot tell whether a webhook name exists. An unknown
// name, a disabled def and every authentication failure on a live def (a
// signature mismatch, a missing signature header, a wrong bearer) answer the
// same status, headers and body bytes; only the delivery ring keeps the real
// verdict, for the operator.
func TestWebhook_BadSignatureIsIndistinguishableFromUnknownName(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	hmacAuth := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}
	defs := map[string]config.Webhook{
		"gh":     {Enabled: true, Delivery: "spawn", Agent: "x", Auth: hmacAuth},
		"off":    {Enabled: false, Delivery: "spawn", Agent: "x", Auth: hmacAuth},
		"bearer": {Enabled: true, Delivery: "spawn", Agent: "x", Auth: config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "WH_BEARER"}},
	}
	env := map[string]string{"WH_SECRET": "shhh", "WH_BEARER": "the-bearer"}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	rec := newTestReceiver(t, defs, fr, nil, env, []string{"WH_SECRET", "WH_BEARER"}, now)

	body := []byte(`{"goal":"a"}`)
	badSig := http.Header{}
	badSig.Set("X-Hub-Signature-256", githubSig("not-the-secret", body))
	wrongBearer := http.Header{}
	wrongBearer.Set("Authorization", "Bearer not-the-bearer")

	baseline := doPost(rec, "nope", body, http.Header{})
	if baseline.Code != http.StatusNotFound {
		t.Fatalf("unknown name: status = %d, want 404", baseline.Code)
	}
	for _, c := range []struct {
		name, hook string
		hdr        http.Header
		verdict    string // "" = no ring for this def
	}{
		{"disabled def", "off", badSig, ""},
		{"signature mismatch", "gh", badSig, verdictRejectedSig},
		{"missing signature header", "gh", http.Header{}, verdictRejectedSig},
		{"wrong bearer", "bearer", wrongBearer, verdictRejectedSig},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := doPost(rec, c.hook, body, c.hdr)
			if w.Code != baseline.Code {
				t.Errorf("status = %d, want %d (as an unknown name)", w.Code, baseline.Code)
			}
			if !reflect.DeepEqual(w.Header(), baseline.Header()) {
				t.Errorf("headers = %v, want %v (as an unknown name)", w.Header(), baseline.Header())
			}
			if !bytes.Equal(w.Body.Bytes(), baseline.Body.Bytes()) {
				t.Errorf("body = %q, want %q (as an unknown name)", w.Body.String(), baseline.Body.String())
			}
			if c.verdict == "" {
				return
			}
			recs, ok := rec.recentSnapshot(webhookKey(lookup.WebhookOwner{Static: true}, c.hook), 1)
			if !ok || len(recs) == 0 || recs[0].Verdict != c.verdict {
				t.Errorf("delivery ring = %+v, want the real verdict %q kept for the operator", recs, c.verdict)
			}
		})
	}
	if fr.wasCalled() {
		t.Fatal("a run started on an unauthenticated delivery")
	}
}

// TestWebhook_UnresolvableSecretDoesNotNameTheSecret: a def whose secret
// cannot be resolved stays 503 (so a sender retries once the operator fixes
// it), but the unauthenticated caller learns neither the env var nor the
// credential reference; the log keeps them for the operator. An auth.kind=none
// def the operator has not enabled is the same config-side failure and
// answers the same body.
func TestWebhook_UnresolvableSecretDoesNotNameTheSecret(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"a"}`)
	for _, c := range []struct {
		name, ref string
		auth      config.WebhookAuth
	}{
		{"env var not allowlisted", "WH_SECRET", config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}},
		{"stored credential missing", "$cred:hook", config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", Secret: "$cred:hook"}},
		{"unauthenticated mode not enabled", "LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED", config.WebhookAuth{Kind: "none"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged strings.Builder
			rec := newTestReceiver(t, map[string]config.Webhook{"gh": {Enabled: true, Delivery: "spawn", Agent: "x", Auth: c.auth}},
				&fakeRunner{runID: "r", agentID: "a"}, nil, map[string]string{"WH_SECRET": "shhh"}, nil, now)
			rec.logf = func(format string, args ...any) { logged.WriteString(fmt.Sprintf(format, args...)) }

			h := http.Header{}
			h.Set("X-Hub-Signature-256", githubSig("shhh", body))
			w := doPost(rec, "gh", body, h)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			if got, want := strings.TrimSpace(w.Body.String()), `{"error":"unavailable"}`; got != want {
				t.Errorf("body = %s, want %s", got, want)
			}
			if !strings.Contains(logged.String(), c.ref) {
				t.Errorf("log %q does not name %s for the operator", logged.String(), c.ref)
			}
			recs, ok := rec.recentSnapshot(webhookKey(lookup.WebhookOwner{Static: true}, "gh"), 1)
			if !ok || len(recs) == 0 || recs[0].Verdict != verdictUnresolved {
				t.Errorf("delivery ring = %+v, want verdict %q", recs, verdictUnresolved)
			}
		})
	}
}

// rawStatusLines sends a POST with Expect: 100-continue to a real server and
// returns every status line it answers with: the interim 100 Continue, if
// the server asks for the body, then the final status. A server that reads
// the body only for a live webhook would send 100 Continue only for a name
// that exists.
func rawStatusLines(t *testing.T, srvURL, path string, body []byte) []string {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srvURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\nExpect: 100-continue\r\nX-Hub-Signature-256: sha256=00\r\n\r\n", path, len(body))
	br := bufio.NewReader(conn)
	var lines []string
	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			// No answer within the window: the server is waiting for the
			// body without having asked for it.
			lines = append(lines, "(no answer: "+err.Error()+")")
			if _, werr := conn.Write(body); werr != nil {
				return lines
			}
			continue
		}
		lines = append(lines, resp.Status)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusContinue {
			return lines
		}
		if _, err := conn.Write(body); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWebhook_ExpectContinueProbeIsIndistinguishable: a client that sends
// Expect: 100-continue (curl does, for a body over 1 KiB) learns from the
// interim answer whether the server wants the body. The receiver must want it,
// or not, the same way for an unknown name as for a live one.
func TestWebhook_ExpectContinueProbeIsIndistinguishable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	defs := map[string]config.Webhook{
		"gh":  {Enabled: true, Delivery: "spawn", Agent: "x", Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}},
		"off": {Enabled: false, Delivery: "spawn", Agent: "x", Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}},
	}
	rec := newTestReceiver(t, defs, &fakeRunner{runID: "r", agentID: "a"}, nil, map[string]string{"WH_SECRET": "shhh"}, []string{"WH_SECRET"}, now)
	mux := http.NewServeMux()
	rec.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := bytes.Repeat([]byte("a"), 2048)
	want := rawStatusLines(t, srv.URL, "/v1/_webhooks/nope", body)
	for _, name := range []string{"gh", "off"} {
		if got := rawStatusLines(t, srv.URL, "/v1/_webhooks/"+name, body); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: status lines %q, want %q (as an unknown name)", name, got, want)
		}
	}
}

// TestWebhook_OversizeBodyIsIndistinguishableFromUnknownName: a body over the
// def's own limit, or over the server's request limit, gets the unknown-name
// answer; the ring keeps rejected_body for the operator.
func TestWebhook_OversizeBodyIsIndistinguishableFromUnknownName(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	defs := map[string]config.Webhook{
		"gh": {Enabled: true, Delivery: "spawn", Agent: "x", BodySizeLimitBytes: 512,
			Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}},
	}
	rec := newTestReceiver(t, defs, &fakeRunner{runID: "r", agentID: "a"}, nil, map[string]string{"WH_SECRET": "shhh"}, []string{"WH_SECRET"}, now)
	rec.cfg.Env.MaxRequestBytes = 4096

	for _, size := range []int{1000, 8000} { // over the def's limit; over the server's
		body := bytes.Repeat([]byte("a"), size)
		hdr := http.Header{}
		hdr.Set("X-Hub-Signature-256", githubSig("shhh", body))
		want := doPost(rec, "nope", body, hdr)
		got := doPost(rec, "gh", body, hdr)
		if got.Code != want.Code || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) || !reflect.DeepEqual(got.Header(), want.Header()) {
			t.Errorf("%d-byte body: live def %d %q %v, unknown name %d %q %v", size, got.Code, got.Body.String(), got.Header(), want.Code, want.Body.String(), want.Header())
		}
		recs, ok := rec.recentSnapshot(webhookKey(lookup.WebhookOwner{Static: true}, "gh"), 1)
		if !ok || len(recs) == 0 || recs[0].Verdict != "rejected_body" {
			t.Errorf("%d-byte body: delivery ring = %+v, want rejected_body", size, recs)
		}
	}
}

// TestTeamWebhook_OversizeBodyIsIndistinguishableFromUnknownName: the same
// for a team's own webhook.
func TestTeamWebhook_OversizeBodyIsIndistinguishableFromUnknownName(t *testing.T) {
	rec, th, _ := newLocalHookReceiver(t, map[string]runner.TeamWebhook{"/triage/github": ghHook()}, nil)
	body := bytes.Repeat([]byte("a"), defaultBodySizeLimitBytes+1)
	want := post(rec, "/v1/_teams/triage/webhooks/nope", body, signed(body, ""))
	got := post(rec, "/v1/_teams/triage/webhooks/github", body, signed(body, ""))
	if got.Code != want.Code || !bytes.Equal(got.Body.Bytes(), want.Body.Bytes()) {
		t.Errorf("team webhook %d %q, unknown %d %q", got.Code, got.Body.String(), want.Code, want.Body.String())
	}
	if len(th.publishes()) != 0 {
		t.Error("an over-size delivery published")
	}
}
