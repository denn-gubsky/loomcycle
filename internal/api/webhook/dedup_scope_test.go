package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// These tests drive the receiver against a REAL sqlite store, so Layer 2 runs
// on the real runs.idempotency_key unique index and the real
// RunByIdempotencyKey — the database-wide index is exactly what made a
// byte-identical body sent to a second webhook dedup against the first.

// storeRunner stands in for the server's RunOnce: it creates the session + run
// the way the real runner does, stamping the input's tenant and idempotency
// key, and returns the store's ErrDuplicateIdempotencyKey verbatim.
type storeRunner struct {
	st store.Store

	mu    sync.Mutex
	calls int
}

func (r *storeRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	r.mu.Lock()
	r.calls++
	agentID := fmt.Sprintf("agent-%d", r.calls)
	r.mu.Unlock()
	sess, err := r.st.CreateSession(ctx, in.TenantID, in.Agent, in.UserID)
	if err != nil {
		return err
	}
	run, err := r.st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID:        agentID,
		UserID:         in.UserID,
		TenantID:       in.TenantID,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return err
	}
	if cb.OnRegistered != nil {
		cb.OnRegistered(agentID, run.ID, sess.ID, "")
	}
	return nil
}

func (r *storeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

const scopeSecret = "shhh"

func openScopeStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newScopeReceiver(st store.Store, fr runner.Runner, webhooks map[string]config.Webhook) *Receiver {
	return New(Deps{
		Cfg:          &config.Config{Webhooks: webhooks},
		Store:        st,
		Runner:       fr,
		EnvAllowlist: map[string]bool{"WH_SECRET": true},
		Now:          fixedClock(time.Unix(1_700_000_000, 0)),
		Getenv:       mapGetenv(map[string]string{"WH_SECRET": scopeSecret}),
	})
}

func scopeWebhook(deliveryIDHeader string) config.Webhook {
	return config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "x",
		Auth: config.WebhookAuth{
			Kind: "hmac", Header: "X-Hub-Signature-256",
			SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: deliveryIDHeader,
		},
	}
}

// postSigned POSTs body to path, signed with the shared test secret, and
// returns the status plus the decoded JSON response.
func postSigned(t *testing.T, rec *Receiver, path string, body []byte, deliveryID string) (int, map[string]string) {
	t.Helper()
	mux := http.NewServeMux()
	rec.Mount(mux)
	req := httptest.NewRequest(http.MethodPost, path, bytesReader(body))
	req.Header.Set("X-Hub-Signature-256", githubSig(scopeSecret, body))
	if deliveryID != "" {
		req.Header.Set("X-Delivery-Id", deliveryID)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("POST %s: undecodable body %q: %v", path, w.Body.String(), err)
	}
	return w.Code, got
}

// assertFreshRun requires a 202 that started a NEW run (not a dedup ack).
func assertFreshRun(t *testing.T, label string, code int, got map[string]string) string {
	t.Helper()
	if code != http.StatusAccepted {
		t.Fatalf("%s: status = %d, want 202; body=%v", label, code, got)
	}
	if got["deduped"] != "" {
		t.Fatalf("%s: deduped against another delivery (run_id %q); want a fresh run", label, got["run_id"])
	}
	if got["run_id"] == "" {
		t.Fatalf("%s: no run_id in %v", label, got)
	}
	return got["run_id"]
}

func TestReceiver_IdenticalBodyToTwoWebhooks_StartsTwoRuns(t *testing.T) {
	st := openScopeStore(t)
	fr := &storeRunner{st: st}
	rec := newScopeReceiver(st, fr, map[string]config.Webhook{
		"hook-a": scopeWebhook(""),
		"hook-b": scopeWebhook(""),
	})
	body := []byte(`{"goal":"same bytes"}`)

	code, got := postSigned(t, rec, "/v1/_webhooks/hook-a", body, "")
	runA := assertFreshRun(t, "hook-a", code, got)
	code, got = postSigned(t, rec, "/v1/_webhooks/hook-b", body, "")
	runB := assertFreshRun(t, "hook-b", code, got)

	if runA == runB {
		t.Errorf("both webhooks answered run %q; each must start its own", runA)
	}
	if n := fr.callCount(); n != 2 {
		t.Errorf("runner invoked %d times, want 2", n)
	}
}

func TestReceiver_SameNamedWebhookInTwoTenants_EachStartsOwnRun(t *testing.T) {
	st := openScopeStore(t)
	ctx := context.Background()
	for _, tenant := range []string{"acme", "globex"} {
		def, err := json.Marshal(map[string]any{
			"enabled": true, "delivery": "spawn", "agent": "x", "tenant_id": tenant,
			"auth": map[string]any{"kind": "hmac", "header": "X-Hub-Signature-256", "signing_secret_env": "WH_SECRET"},
		})
		if err != nil {
			t.Fatal(err)
		}
		row, err := st.WebhookDefCreate(ctx, store.WebhookDefRow{
			DefID: "wd-" + tenant, Name: "gh", Definition: def, TenantID: tenant,
		})
		if err != nil {
			t.Fatalf("WebhookDefCreate(%s): %v", tenant, err)
		}
		if err := st.WebhookDefSetActive(ctx, tenant, "gh", row.DefID, "test"); err != nil {
			t.Fatalf("WebhookDefSetActive(%s): %v", tenant, err)
		}
	}
	fr := &storeRunner{st: st}
	rec := newScopeReceiver(st, fr, nil)
	body := []byte(`{"goal":"same bytes"}`)

	runs := map[string]string{}
	for _, tenant := range []string{"acme", "globex"} {
		code, got := postSigned(t, rec, "/v1/_webhooks/"+tenant+"/gh", body, "")
		runs[tenant] = assertFreshRun(t, tenant, code, got)
	}
	if runs["acme"] == runs["globex"] {
		t.Fatalf("both tenants answered run %q; each must start its own", runs["acme"])
	}
	// Each response names a run in the caller's own tenant.
	for tenant, runID := range runs {
		r, err := st.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetRun(%s): %v", runID, err)
		}
		if r.TenantID != tenant {
			t.Errorf("tenant %s was answered with run %s owned by tenant %q", tenant, runID, r.TenantID)
		}
	}
}

// The scoping must not weaken the replay guard: a true re-delivery to the
// same webhook is deduped by Layer 1 on the same receiver and by Layer 2 on a
// fresh receiver (a restart or another replica) sharing the store — both
// answering the ORIGINAL run.
func TestReceiver_ReplayToSameWebhook_ReturnsOriginalRunFromBothLayers(t *testing.T) {
	for _, tc := range []struct {
		name, header, did string
	}{
		{"body-hash", "", ""},
		{"delivery-id-header", "X-Delivery-Id", "evt-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openScopeStore(t)
			fr := &storeRunner{st: st}
			hooks := map[string]config.Webhook{"gh": scopeWebhook(tc.header)}
			body := []byte(`{"goal":"once"}`)

			rec := newScopeReceiver(st, fr, hooks)
			code, got := postSigned(t, rec, "/v1/_webhooks/gh", body, tc.did)
			original := assertFreshRun(t, "first delivery", code, got)

			// Layer 1: same receiver, inside the TTL.
			code, got = postSigned(t, rec, "/v1/_webhooks/gh", body, tc.did)
			if code != http.StatusOK || got["deduped"] != "true" || got["run_id"] != original {
				t.Errorf("layer-1 replay = %d %v; want 200 deduped run_id=%s", code, got, original)
			}

			// Layer 2: a fresh receiver has an empty cache; only the store knows.
			fresh := newScopeReceiver(st, fr, hooks)
			code, got = postSigned(t, fresh, "/v1/_webhooks/gh", body, tc.did)
			if code != http.StatusAccepted || got["deduped"] != "true" || got["run_id"] != original {
				t.Errorf("layer-2 replay = %d %v; want 202 deduped run_id=%s", code, got, original)
			}
			if tc.did != "" && got["delivery_id"] != tc.did {
				t.Errorf("delivery_id = %q, want the sender's id %q", got["delivery_id"], tc.did)
			}
			if n := fr.callCount(); n != 1 {
				t.Errorf("runner invoked %d times, want 1 (replays must not spawn)", n)
			}
		})
	}
}

// A sender-supplied delivery id is scoped the same way: two webhooks whose
// senders happen to pick the same id each get their own run, and the response
// still echoes the sender's id, not the internal key.
func TestReceiver_SameDeliveryIDHeaderOnTwoWebhooks_StartsTwoRuns(t *testing.T) {
	st := openScopeStore(t)
	fr := &storeRunner{st: st}
	rec := newScopeReceiver(st, fr, map[string]config.Webhook{
		"hook-a": scopeWebhook("X-Delivery-Id"),
		"hook-b": scopeWebhook("X-Delivery-Id"),
	})

	code, gotA := postSigned(t, rec, "/v1/_webhooks/hook-a", []byte(`{"goal":"a"}`), "evt-1")
	runA := assertFreshRun(t, "hook-a", code, gotA)
	code, gotB := postSigned(t, rec, "/v1/_webhooks/hook-b", []byte(`{"goal":"b"}`), "evt-1")
	runB := assertFreshRun(t, "hook-b", code, gotB)

	if runA == runB {
		t.Errorf("both webhooks answered run %q; each must start its own", runA)
	}
	for label, got := range map[string]map[string]string{"hook-a": gotA, "hook-b": gotB} {
		if got["delivery_id"] != "evt-1" {
			t.Errorf("%s delivery_id = %q, want the sender's id evt-1", label, got["delivery_id"])
		}
	}
}
