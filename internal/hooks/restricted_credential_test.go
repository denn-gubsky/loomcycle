package hooks

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// A webhook header naming a credential restricted by allowed_urls, through the
// real credential engine: resolved for the callback URL, so a covered callback
// gets it, and a covered callback that redirects does not pass it on.
func TestWebhook_ARestrictedCredentialGoesOnlyToItsCallback(t *testing.T) {
	var sinkKeys atomic.Int64
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			sinkKeys.Add(1)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(sink.Close)
	var gotKey atomic.Value
	covered := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("X-Api-Key"))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(covered.Close)
	hop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(hop.Close)

	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	engine := credential.NewEngine(st, sealer)
	allowed := []string{covered.URL, hop.URL}
	list, err := credential.NormalizeAllowedURLs(&allowed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.PutInline(context.Background(), credential.Identity{Scope: "tenant", Name: "hook_key"}, "restricted-value", nil, list); err != nil {
		t.Fatal(err)
	}

	run := func(callback string) PreOutcome {
		t.Helper()
		s := NewSet()
		mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: callback, FailMode: FailClosed,
			Headers: map[string]string{"X-Api-Key": "$cred:hook_key"}})
		d := NewDispatcher(s, covered.Client()) // trusts httptest's certificate, which all three share
		d.SetHeaderSubstitute(func(ctx context.Context, s, targetURL string) (string, []string, error) {
			return engine.Substitute(ctx, "", "", "", s, targetURL, nil)
		})
		return d.RunPre(context.Background(), Identity{Agent: "w"}, preCall())
	}

	if out := run(covered.URL); out.Deny != nil {
		t.Fatalf("a covered callback was denied: %+v", out.Deny)
	}
	if v, _ := gotKey.Load().(string); v != "restricted-value" {
		t.Errorf("the covered callback got %q", v)
	}
	if out := run(sink.URL); out.Deny == nil {
		t.Error("a callback outside the list was called")
	}
	if out := run(hop.URL); out.Deny == nil {
		t.Error("a redirect from a covered callback was followed")
	}
	if n := sinkKeys.Load(); n != 0 {
		t.Errorf("the restricted credential reached a host outside its list %d time(s)", n)
	}
}
