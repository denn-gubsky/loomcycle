package hooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// headerHook records the headers of each call.
func headerHook(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var mu sync.Mutex
	var last http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.Header.Clone()
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() http.Header { mu.Lock(); defer mu.Unlock(); return last }
}

var sub = func(_ context.Context, s string) (string, []string, error) {
	if strings.Contains(s, "$cred:hook_secret") {
		return strings.ReplaceAll(s, "$cred:hook_secret", "s3cr3t"), nil, nil
	}
	if strings.Contains(s, "$cred:") {
		return s, []string{"missing"}, nil
	}
	return s, nil, nil
}

func preCall() ToolCall { return ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)} }

// A header naming a credential is sent with the credential's value, resolved
// for the run when the hook is called; a literal header is sent as written.
func TestDispatcher_WebhookHeadersResolveCredentialsAtCallTime(t *testing.T) {
	srv, got := headerHook(t)
	s := NewSet()
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: srv.URL,
		Headers: map[string]string{"Authorization": "Bearer $cred:hook_secret", "X-App": "jobember"}})
	d := NewDispatcher(s, nil)
	d.SetHeaderSubstitute(sub)
	if out := d.RunPre(context.Background(), Identity{Agent: "w"}, preCall()); out.Deny != nil {
		t.Fatalf("denied: %+v", out.Deny)
	}
	h := got()
	if h.Get("Authorization") != "Bearer s3cr3t" || h.Get("X-App") != "jobember" {
		t.Fatalf("headers = %v", h)
	}
	if h.Get("Content-Type") != "application/json" {
		t.Fatalf("the call's own Content-Type was lost: %v", h)
	}
}

// A credential that does not resolve fails the call — the literal reference is
// never sent — and a closed hook then denies.
func TestDispatcher_AnUnresolvedHeaderCredentialFailsTheCall(t *testing.T) {
	for name, set := range map[string]func(*Dispatcher){
		"not in the store":    func(d *Dispatcher) { d.SetHeaderSubstitute(sub) },
		"no credential store": func(*Dispatcher) {},
	} {
		t.Run(name, func(t *testing.T) {
			srv, got := headerHook(t)
			s := NewSet()
			mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: srv.URL, FailMode: FailClosed,
				Headers: map[string]string{"Authorization": "Bearer $cred:missing"}})
			d := NewDispatcher(s, nil)
			set(d)
			out := d.RunPre(context.Background(), Identity{Agent: "w"}, preCall())
			if out.Deny == nil {
				t.Fatalf("a closed hook with an unresolved credential let the call through")
			}
			if got() != nil {
				t.Fatalf("the hook was called with headers %v", got())
			}
			if len(out.Decisions) != 1 || out.Decisions[0].Reason != "a header credential could not be resolved" {
				t.Fatalf("decisions = %+v", out.Decisions)
			}
		})
	}
}

func TestHeaders_ValidationAndResolveCarryThem(t *testing.T) {
	bad := map[string]map[string]string{
		"bad name":        {"X App": "v"},
		"reserved header": {"Content-Type": "text/plain"},
		"line break":      {"X-App": "a\r\nX-Injected: 1"},
	}
	for name, h := range bad {
		if err := (EventHooks{PhasePre: {{Inline: &Inline{Name: "g", URL: "https://x", Headers: h}}}}).Validate(""); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	code := Def{Event: PhasePre, Body: DefBody{Kind: BodyKindCode, Code: "function hook(){}", Headers: map[string]string{"X": "y"}}}
	if err := code.Validate(); err == nil {
		t.Errorf("headers accepted on a code body")
	}
	defs := map[string]Def{"|gate": {Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h", Headers: map[string]string{"Authorization": "$cred:k"}}}}
	s := NewSet()
	agent := EventHooks{PhasePre: {{Ref: "gate"}, {Inline: &Inline{Name: "g", URL: "https://x", Headers: map[string]string{"X-A": "1"}}}}}
	if err := Resolve(context.Background(), Source{Owner: "agent:w"}, agent, nil, fakeLookup(defs), Permits{}, s); err != nil {
		t.Fatal(err)
	}
	hs := s.List()
	if hs[0].Headers["Authorization"] != "$cred:k" || hs[1].Headers["X-A"] != "1" {
		t.Fatalf("resolved headers = %v / %v", hs[0].Headers, hs[1].Headers)
	}
}
