package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A hook's failure detail goes to the server log. Its URL's path and query can
// carry the secret (a Slack incoming-webhook URL is one), and the receiver's
// error body can echo a credential, so the detail names only scheme and host
// and keeps a short, redacted snippet of the body.
func TestWebhookCallError_LogDetailHidesURLPathAndRedactsBody(t *testing.T) {
	const leak = "xoxb-1234567890-abcdefghij"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("bad token " + leak + " " + strings.Repeat("x", 4096)))
	}))
	defer srv.Close()
	c := newWebhookClient(nil, nil)

	err := c.post(context.Background(), c.http, srv.URL+"/services/T0/B0/PATHSECRET?token=QUERYSECRET", nil, nil, struct{}{}, &struct{}{})
	if err == nil {
		t.Fatal("a 500 must fail the call")
	}
	msg := err.Error()
	for _, s := range []string{"PATHSECRET", "QUERYSECRET", leak} {
		if strings.Contains(msg, s) {
			t.Errorf("log detail carries %q: %s", s, msg)
		}
	}
	if !strings.Contains(msg, srv.URL) || !strings.Contains(msg, "500") {
		t.Errorf("log detail lost the host or the status: %s", msg)
	}
	if len(msg) > 512 {
		t.Errorf("log detail is %d bytes; the body snippet must be short", len(msg))
	}

	// A transport failure's net/http error quotes the whole URL.
	srv.Close()
	err = c.post(context.Background(), c.http, srv.URL+"/services/T0/B0/PATHSECRET?token=QUERYSECRET", nil, nil, struct{}{}, &struct{}{})
	if err == nil {
		t.Fatal("a closed receiver must fail the call")
	}
	if msg := err.Error(); strings.Contains(msg, "PATHSECRET") || strings.Contains(msg, "QUERYSECRET") {
		t.Errorf("transport detail carries the URL's path or query: %s", msg)
	}
}
