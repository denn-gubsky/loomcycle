package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sigv1"
)

// Only the body is signed. These tests pin that the delivery id a receiver
// dedupes on, and the hook identity it trusts, are in the body — the headers
// are unsigned copies a replayer can change.

// bodyDeliveryID is the delivery_id a call's body carries; "" when it has none.
func bodyDeliveryID(t *testing.T, body []byte) string {
	t.Helper()
	var p struct {
		DeliveryID *string `json:"delivery_id"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if p.DeliveryID == nil {
		return ""
	}
	return *p.DeliveryID
}

// Every payload a signed hook is sent carries its delivery id INSIDE the
// signed body — pre, post, a run event and a channel message alike — equal to
// the header's copy, beside the hook_name the header copies. An unsigned
// hook's payload has no delivery_id key at all.
func TestDispatcher_SignedPayloadCarriesItsDeliveryIDInTheBody(t *testing.T) {
	srv, got := signedReceiver(t, http.StatusOK)
	s := NewSet()
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "pre", Phase: PhasePre, CallbackURL: srv.URL, SigningSecret: "$cred:sign"})
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "post", Phase: PhasePost, CallbackURL: srv.URL, SigningSecret: "$cred:sign"})
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "start", Phase: PhaseAgentStart, CallbackURL: srv.URL, SigningSecret: "$cred:sign"})
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "plain", Phase: PhasePre, CallbackURL: srv.URL})
	d := NewDispatcher(s, nil)
	var targets []string
	d.SetHeaderSubstitute(signingSub(&targets))
	ident := Identity{Agent: "w"}
	d.RunPre(context.Background(), ident, preCall())
	d.RunPost(context.Background(), ident, preCall(), ToolResult{Text: "ok"})
	d.RunGate(context.Background(), ident, PhaseAgentStart, LifecycleInfo{})
	h := channelHook(srv.URL)
	h.SigningSecret = "$cred:sign"
	if _, err := d.InvokeChannel(context.Background(), h, channelCall()); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	unsigned := 0
	for _, c := range got() {
		if c.header.Get("X-Loomcycle-Signature") == "" {
			unsigned++
			if bytes.Contains(c.body, []byte(`"delivery_id"`)) {
				t.Fatalf("an unsigned call's payload carries delivery_id: %s", c.body)
			}
			continue
		}
		name := c.header.Get("X-Loomcycle-Hook")
		seen[name] = true
		id := bodyDeliveryID(t, c.body)
		if !uuidRe.MatchString(id) || id != c.header.Get("X-Loomcycle-Delivery") {
			t.Fatalf("%s: body delivery_id %q, header copy %q", name, id, c.header.Get("X-Loomcycle-Delivery"))
		}
		var p struct {
			HookName string `json:"hook_name"`
		}
		if err := json.Unmarshal(c.body, &p); err != nil || p.HookName != name {
			t.Fatalf("body hook_name %q, header copy %q", p.HookName, name)
		}
	}
	for _, name := range []string{"pre", "post", "start", "screen"} {
		if !seen[name] {
			t.Errorf("no signed call from %s", name)
		}
	}
	if unsigned != 1 {
		t.Errorf("unsigned calls = %d, want 1", unsigned)
	}
}

// A captured signed delivery replayed within the tolerance with a fresh
// X-Loomcycle-Delivery header (and another X-Loomcycle-Hook) still verifies —
// the headers are not signed — but its body carries the ORIGINAL delivery_id,
// so a receiver deduplicating on the body refuses it. Changing the body's id
// instead breaks the signature.
func TestSignedDelivery_AReplayCannotChangeItsDeliveryID(t *testing.T) {
	srv, got := signedReceiver(t, http.StatusOK)
	s := NewSet()
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: srv.URL, SigningSecret: "$cred:sign"})
	d := NewDispatcher(s, nil)
	var targets []string
	d.SetHeaderSubstitute(signingSub(&targets))
	at := time.Unix(1_760_000_000, 0)
	d.now = func() time.Time { return at }
	d.RunPre(context.Background(), Identity{Agent: "w"}, preCall())
	orig := got()[0]
	origID := bodyDeliveryID(t, orig.body)
	if origID == "" {
		t.Fatal("the signed body carries no delivery_id")
	}

	// The receiver the docs describe: verify the raw body, then dedupe on the
	// body's delivery_id.
	seenIDs := map[string]bool{}
	accept := func(body []byte, h http.Header) bool {
		if !sigv1.Verify(signingCanary, body, h.Get("X-Loomcycle-Signature"), at.Add(time.Minute), sigv1.DefaultTolerance) {
			return false
		}
		id := bodyDeliveryID(t, body)
		if id == "" || seenIDs[id] {
			return false
		}
		seenIDs[id] = true
		return true
	}
	if !accept(orig.body, orig.header) {
		t.Fatal("the original delivery was refused")
	}
	replayed := orig.header.Clone()
	replayed.Set("X-Loomcycle-Delivery", "00000000-0000-4000-8000-000000000000")
	replayed.Set("X-Loomcycle-Hook", "another-hook")
	if accept(orig.body, replayed) {
		t.Fatal("a replay with a fresh X-Loomcycle-Delivery header was accepted as a new delivery")
	}
	tampered := bytes.Replace(orig.body, []byte(origID), []byte("00000000-0000-4000-8000-000000000000"), 1)
	if bytes.Equal(tampered, orig.body) {
		t.Fatal("test setup: the body's id was not replaced")
	}
	if sigv1.Verify(signingCanary, tampered, orig.header.Get("X-Loomcycle-Signature"), at, sigv1.DefaultTolerance) {
		t.Fatal("a body whose delivery_id was changed still verifies")
	}
}

// A channel hook's retry keeps the delivery_id inside its body too.
func TestInvokeChannel_RetryKeepsTheBodyDeliveryID(t *testing.T) {
	srv, got := signedReceiver(t, http.StatusOK)
	h := channelHook(srv.URL)
	h.SigningSecret = "$cred:sign"
	d := NewDispatcher(nil, nil)
	var targets []string
	d.SetHeaderSubstitute(signingSub(&targets))
	call := channelCall()
	for attempt := 1; attempt <= 2; attempt++ {
		call.Attempt = attempt
		if _, err := d.InvokeChannel(context.Background(), h, call); err != nil {
			t.Fatal(err)
		}
	}
	calls := got()
	a, b := bodyDeliveryID(t, calls[0].body), bodyDeliveryID(t, calls[1].body)
	if a == "" || a != b {
		t.Fatalf("body delivery_id %q then %q: a retry must keep it", a, b)
	}
}

// A signed call whose payload cannot carry a delivery id is refused, never
// sent with the id only in an unsigned header.
func TestDispatcher_ASignedPayloadWithoutADeliveryIDIsNotSent(t *testing.T) {
	srv, got := signedReceiver(t, http.StatusOK)
	d := NewDispatcher(nil, nil)
	var targets []string
	d.SetHeaderSubstitute(signingSub(&targets))
	h := &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: srv.URL, SigningSecret: "$cred:sign", Timeout: time.Second}
	var out PreHookResult
	if err := d.invoke(context.Background(), h, &struct{ Phase Phase }{PhasePre}, &out); err == nil {
		t.Fatal("a payload with no delivery_id was signed and sent")
	}
	if len(got()) != 0 {
		t.Fatalf("the hook was called: %+v", got())
	}
}
