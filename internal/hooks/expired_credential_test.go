package hooks

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// A webhook header naming a stored credential past its expires_at, through
// the real credential engine: the callback is never called and a closed hook
// denies, rather than calling it without the header or with the literal.
func TestWebhook_AnExpiredHeaderCredentialDeniesWithoutCalling(t *testing.T) {
	var calls atomic.Int64
	callback := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(callback.Close)

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
	past := time.Now().Add(-time.Minute)
	if _, err := engine.PutInline(context.Background(), credential.Identity{Scope: "tenant", Name: "hook_key"}, "stale-value", &past, nil, nil); err != nil {
		t.Fatal(err)
	}

	s := NewSet()
	mustRegister(t, s, &Hook{Owner: "agent:w", Name: "gate", Phase: PhasePre, CallbackURL: callback.URL, FailMode: FailClosed,
		Headers: map[string]string{"X-Api-Key": "$cred:hook_key"}})
	d := NewDispatcher(s, callback.Client())
	d.SetHeaderSubstitute(func(ctx context.Context, s, targetURL string) (string, []string, error) {
		return engine.Substitute(ctx, "", "", "", s, targetURL, nil)
	})
	if out := d.RunPre(context.Background(), Identity{Agent: "w"}, preCall()); out.Deny == nil {
		t.Error("a closed hook whose header credential expired did not deny")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the callback was called %d time(s) with an expired header credential", n)
	}
}
