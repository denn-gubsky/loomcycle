package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebFetch_ReturnsNoResponseHeaders: WebFetch hands the model the page's
// text and nothing of the response around it, so no header reaches the
// model: neither a credential (which the HTTP tool would show by name with a
// [redacted] value) nor any other. That holds for an error status too, which
// the HTTP tool returns as an ordinary result.
func TestWebFetch_ReturnsNoResponseHeaders(t *testing.T) {
	const secret = "s3cr3t-session-value"
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				h := w.Header()
				h.Set("Set-Cookie", "session="+secret+"; HttpOnly")
				h.Set("X-Auth-Token", secret)
				h.Set("X-Request-Id", "req-visible-header")
				h.Set("Content-Type", "text/html")
				w.WriteHeader(status)
				fmt.Fprint(w, "<html><body><p>The page text.</p></body></html>")
			}))
			defer srv.Close()

			f := &WebFetch{HTTP: &HTTP{HostAllowlist: []string{mustHost(t, srv.URL)}, AllowPrivateIPs: true}}
			body, _ := json.Marshal(map[string]string{"url": srv.URL})
			res, err := f.Execute(context.Background(), body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res.Text, "The page text.") {
				t.Fatalf("the page text is missing: %q", res.Text)
			}
			for _, leak := range []string{secret, "Set-Cookie", "X-Auth-Token", "[redacted]", "X-Request-Id", "req-visible-header", "Content-Type", "HTTP GET"} {
				if strings.Contains(res.Text, leak) {
					t.Errorf("WebFetch output carries %q from the response around the page:\n%s", leak, res.Text)
				}
			}
		})
	}
}
