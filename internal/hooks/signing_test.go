package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sigv1"
)

// signingCanary is the signing secret's value in these tests: if it appears in
// any request, log line, error or decision, a secret leaked.
const signingCanary = "CANARY-signing-secret-4f1d9c"

// delivery is one request a hook receiver saw: its headers and raw body.
type delivery struct {
	header http.Header
	body   []byte
}

// signedReceiver records every call it receives, as received.
func signedReceiver(t *testing.T, status int) (*httptest.Server, func() []delivery) {
	t.Helper()
	var mu sync.Mutex
	var got []delivery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, delivery{header: r.Header.Clone(), body: b})
		mu.Unlock()
		w.WriteHeader(status)
		if status < 300 {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []delivery { mu.Lock(); defer mu.Unlock(); return append([]delivery(nil), got...) }
}

// signingSub resolves $cred:sign to the canary and records the URL each
// resolution was made for; any other reference is unresolved.
func signingSub(targets *[]string) HeaderSubstitute {
	var mu sync.Mutex
	return func(_ context.Context, s, targetURL string) (string, []string, error) {
		mu.Lock()
		*targets = append(*targets, targetURL)
		mu.Unlock()
		if s == "$cred:sign" {
			return signingCanary, nil, nil
		}
		if strings.Contains(s, "$cred:") {
			return s, []string{strings.TrimPrefix(s, "$cred:")}, nil
		}
		return s, nil, nil
	}
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[48][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// A HookDef's signing_secret makes every call carry a signature over exactly
// the bytes sent, with the hook's name and a delivery id beside it. The
// resolution is made for the callback URL, as a header credential's is, so a
// credential restricted by allowed_urls signs only a call it covers.
func TestDispatcher_SignedHookCallVerifiesOverTheBytesSent(t *testing.T) {
	srv, got := signedReceiver(t, http.StatusOK)
	s := NewSet()
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: srv.URL, SigningSecret: "$cred:sign"})
	d := NewDispatcher(s, nil)
	var targets []string
	d.SetHeaderSubstitute(signingSub(&targets))
	at := time.Unix(1_760_000_000, 0)
	d.now = func() time.Time { return at }

	if out := d.RunPre(context.Background(), Identity{Agent: "w"}, preCall()); out.Deny != nil {
		t.Fatalf("denied: %+v", out.Deny)
	}
	calls := got()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	c := calls[0]
	sig := c.header.Get("X-Loomcycle-Signature")
	if !sigv1.Verify(signingCanary, c.body, sig, at, sigv1.DefaultTolerance) {
		t.Fatalf("signature %q does not verify over the body received (%s)", sig, c.body)
	}
	if ts, _ := sigv1.Fields(sig); ts != "1760000000" {
		t.Fatalf("t = %q, want the dispatcher's clock", ts)
	}
	if c.header.Get("X-Loomcycle-Hook") != "gate" {
		t.Fatalf("X-Loomcycle-Hook = %q", c.header.Get("X-Loomcycle-Hook"))
	}
	if id := c.header.Get("X-Loomcycle-Delivery"); !uuidRe.MatchString(id) {
		t.Fatalf("X-Loomcycle-Delivery = %q, want a UUID", id)
	}
	if len(targets) != 1 || targets[0] != srv.URL {
		t.Fatalf("the signing secret was resolved for %v, want the callback URL %q", targets, srv.URL)
	}
}

// Two calls of a run hook are two deliveries; an unsigned hook's call carries
// none of the signing headers (its wire is unchanged).
func TestDispatcher_EachRunHookCallIsItsOwnDelivery(t *testing.T) {
	srv, got := signedReceiver(t, http.StatusOK)
	s := NewSet()
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "signed", Phase: PhasePre, CallbackURL: srv.URL, SigningSecret: "$cred:sign"})
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "plain", Phase: PhasePre, CallbackURL: srv.URL})
	d := NewDispatcher(s, nil)
	var targets []string
	d.SetHeaderSubstitute(signingSub(&targets))
	for i := 0; i < 2; i++ {
		d.RunPre(context.Background(), Identity{Agent: "w"}, preCall())
	}
	var ids []string
	for _, c := range got() {
		if c.header.Get("X-Loomcycle-Hook") == "signed" {
			ids = append(ids, c.header.Get("X-Loomcycle-Delivery"))
			continue
		}
		for _, h := range []string{"X-Loomcycle-Signature", "X-Loomcycle-Hook", "X-Loomcycle-Delivery"} {
			if c.header.Get(h) != "" {
				t.Fatalf("an unsigned hook's call carried %s", h)
			}
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("delivery ids = %v, want two distinct", ids)
	}
}

// A signing secret that does not resolve fails the call: nothing is sent, so a
// signed hook is never delivered unsigned, and its fail mode decides.
func TestDispatcher_AnUnresolvedSigningSecretIsNeverSentUnsigned(t *testing.T) {
	for name, set := range map[string]func(*Dispatcher){
		"not in the store":    func(d *Dispatcher) { var tg []string; d.SetHeaderSubstitute(signingSub(&tg)) },
		"no credential store": func(*Dispatcher) {},
		"resolver fault": func(d *Dispatcher) {
			d.SetHeaderSubstitute(func(context.Context, string, string) (string, []string, error) {
				return "", nil, errors.New("decrypt failed")
			})
		},
		"empty value": func(d *Dispatcher) {
			d.SetHeaderSubstitute(func(context.Context, string, string) (string, []string, error) { return "", nil, nil })
		},
	} {
		for _, fm := range []FailMode{FailClosed, FailOpen} {
			t.Run(name+"/"+string(fm), func(t *testing.T) {
				srv, got := signedReceiver(t, http.StatusOK)
				s := NewSet()
				mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: srv.URL, FailMode: fm, SigningSecret: "$cred:missing"})
				d := NewDispatcher(s, nil)
				set(d)
				out := d.RunPre(context.Background(), Identity{Agent: "w"}, preCall())
				if len(got()) != 0 {
					t.Fatalf("the hook was called: %+v", got())
				}
				if (out.Deny != nil) != (fm == FailClosed) {
					t.Fatalf("fail_mode %s: deny = %+v", fm, out.Deny)
				}
				if len(out.Decisions) != 1 || out.Decisions[0].Reason != "the hook's signing secret could not be resolved" {
					t.Fatalf("decisions = %+v", out.Decisions)
				}
			})
		}
	}
}

// loomcycle's signing headers win over a header of the same name: write-time
// validation refuses one, and a hook built some other way (a row stored before
// the refusal) still cannot replace them.
func TestDispatcher_SigningHeadersOverrideAnAuthorsHeaders(t *testing.T) {
	srv, got := signedReceiver(t, http.StatusOK)
	s := NewSet()
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: srv.URL, SigningSecret: "$cred:sign",
		Headers: map[string]string{"X-Loomcycle-Signature": "t=1,v1=00", "x-loomcycle-hook": "other", "X-Loomcycle-Delivery": "fixed"}})
	d := NewDispatcher(s, nil)
	var targets []string
	d.SetHeaderSubstitute(signingSub(&targets))
	d.RunPre(context.Background(), Identity{Agent: "w"}, preCall())
	c := got()[0]
	if !sigv1.Verify(signingCanary, c.body, c.header.Get("X-Loomcycle-Signature"), time.Now(), sigv1.DefaultTolerance) ||
		c.header.Get("X-Loomcycle-Hook") != "gate" || c.header.Get("X-Loomcycle-Delivery") == "fixed" {
		t.Fatalf("an author's header replaced loomcycle's: %v", c.header)
	}
	if n := len(c.header.Values("X-Loomcycle-Signature")); n != 1 {
		t.Fatalf("%d signature headers sent", n)
	}
}

// The channel worker calls a hook again for the same message after a failure.
// That retry keeps the delivery id — a receiver deduplicating on it sees one
// delivery — and is signed afresh, with a new t, over its own body.
func TestInvokeChannel_RetryKeepsTheDeliveryIDAndResigns(t *testing.T) {
	srv, got := signedReceiver(t, http.StatusOK)
	h := channelHook(srv.URL)
	h.SigningSecret = "$cred:sign"
	d := NewDispatcher(nil, nil)
	var targets []string
	d.SetHeaderSubstitute(signingSub(&targets))
	clock := time.Unix(1_760_000_000, 0)
	d.now = func() time.Time { return clock }

	call := channelCall()
	if _, err := d.InvokeChannel(context.Background(), h, call); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(90 * time.Second)
	call.Attempt = 2
	if _, err := d.InvokeChannel(context.Background(), h, call); err != nil {
		t.Fatal(err)
	}
	other := channelCall()
	other.MessageID = "m2"
	if _, err := d.InvokeChannel(context.Background(), h, other); err != nil {
		t.Fatal(err)
	}
	calls := got()
	if len(calls) != 3 {
		t.Fatalf("calls = %d", len(calls))
	}
	first, retry, next := calls[0], calls[1], calls[2]
	id := first.header.Get("X-Loomcycle-Delivery")
	if !uuidRe.MatchString(id) || retry.header.Get("X-Loomcycle-Delivery") != id {
		t.Fatalf("delivery ids: first %q, retry %q — a retry must keep its id", id, retry.header.Get("X-Loomcycle-Delivery"))
	}
	if next.header.Get("X-Loomcycle-Delivery") == id {
		t.Fatalf("another message's call reused delivery id %q", id)
	}
	s1, s2 := first.header.Get("X-Loomcycle-Signature"), retry.header.Get("X-Loomcycle-Signature")
	t1, _ := sigv1.Fields(s1)
	t2, _ := sigv1.Fields(s2)
	if t1 != "1760000000" || t2 != "1760000090" {
		t.Fatalf("t = %q then %q: the retry was not re-signed at its own time", t1, t2)
	}
	if !sigv1.Verify(signingCanary, retry.body, s2, clock, sigv1.DefaultTolerance) {
		t.Fatalf("the retry's signature does not verify over its body")
	}
	if !bytes.Contains(retry.body, []byte(`"attempt":2`)) {
		t.Fatalf("retry body = %s", retry.body)
	}
}

// The signing secret's value appears in nothing loomcycle sends or writes: not
// in the request beyond the MAC it keys, not in a log line, not in an error or
// a decision — on a call that succeeds and on one the receiver refuses.
func TestDispatcher_SigningSecretValueAppearsInNoLogEventOrRequest(t *testing.T) {
	var logs bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		srv, got := signedReceiver(t, status)
		s := NewSet()
		mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: srv.URL, FailMode: FailClosed, SigningSecret: "$cred:sign"})
		d := NewDispatcher(s, nil)
		var targets []string
		d.SetHeaderSubstitute(signingSub(&targets))
		out := d.RunPre(context.Background(), Identity{Agent: "w", RunID: "r1"}, preCall())
		events, _ := json.Marshal(out)
		var seen strings.Builder
		fmt.Fprintf(&seen, "%s\n%+v\n", events, out)
		for _, c := range got() {
			fmt.Fprintf(&seen, "%v\n%s\n", c.header, c.body)
		}
		if strings.Contains(seen.String(), signingCanary) {
			t.Fatalf("status %d: the signing secret leaked into the request or the outcome:\n%s", status, seen.String())
		}
		h := channelHook(srv.URL)
		h.SigningSecret = "$cred:sign"
		h.FailMode = FailClosed
		_, err := d.InvokeChannel(context.Background(), h, channelCall())
		if err != nil && (strings.Contains(err.Error(), signingCanary) || strings.Contains(DecisionReason(err), signingCanary)) {
			t.Fatalf("status %d: the signing secret leaked into an error: %v", status, err)
		}
	}
	if strings.Contains(logs.String(), signingCanary) {
		t.Fatalf("the signing secret leaked into a log line:\n%s", logs.String())
	}
}

func TestDef_SigningSecretValidationAndHash(t *testing.T) {
	ok := Def{Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h", SigningSecret: "$cred:sign"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a $cred: reference refused: %v", err)
	}
	for name, d := range map[string]Def{
		"literal":         {Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h", SigningSecret: "s3cr3t"}},
		"ref with text":   {Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h", SigningSecret: "x$cred:sign"}},
		"env reference":   {Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h", SigningSecret: "${LOOMCYCLE_KEY}"}},
		"github app mint": {Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h", SigningSecret: "$ghapp:app"}},
		"code body":       {Event: PhasePre, Body: DefBody{Kind: BodyKindCode, Code: "function hook(){}", SigningSecret: "$cred:sign"}},
	} {
		if err := d.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The reference is content; leaving it unset hashes as before the field.
	plain := Def{Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h"}}
	if SignDef("g", plain) == SignDef("g", ok) {
		t.Fatal("signing_secret does not take part in the content hash")
	}
	buf, _ := json.Marshal(plain)
	if bytes.Contains(buf, []byte("signing_secret")) {
		t.Fatalf("an unset signing_secret is serialized: %s", buf)
	}
}

// The three signing headers are loomcycle's on every hook, signed or not.
func TestHeaders_SigningHeaderNamesAreReserved(t *testing.T) {
	for _, name := range []string{"X-Loomcycle-Signature", "x-loomcycle-hook", "X-LOOMCYCLE-DELIVERY"} {
		d := Def{Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h", Headers: map[string]string{name: "v"}}}
		if err := d.Validate(); err == nil {
			t.Errorf("HookDef header %s accepted", name)
		}
		in := EventHooks{PhasePre: {{Inline: &Inline{Name: "g", URL: "https://x", Headers: map[string]string{name: "v"}}}}}
		if err := in.Validate(""); err == nil {
			t.Errorf("inline header %s accepted", name)
		}
	}
}

// A HookDef's signing_secret reaches the hook a run resolves from it.
func TestResolve_CarriesTheSigningSecretReference(t *testing.T) {
	defs := map[string]Def{"|gate": {Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h", SigningSecret: "$cred:sign"}}}
	s := NewSet()
	if err := Resolve(context.Background(), Source{Owner: "agent:w"}, EventHooks{PhasePre: {{Ref: "gate"}}}, nil, fakeLookup(defs), Permits{}, s); err != nil {
		t.Fatal(err)
	}
	if got := s.List()[0].SigningSecret; got != "$cred:sign" {
		t.Fatalf("resolved hook's signing secret = %q", got)
	}
}
