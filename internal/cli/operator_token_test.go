package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --tenant-metrics sends the marker that confines a scrape token to its own
// tenant's series; without the flag the field is not sent at all.
func TestOperatorTokenCreate_TenantMetricsFlagSendsTheMarker(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"def_id":"d"}`))
	}))
	defer ts.Close()
	base := []string{"--target", ts.URL, "--token", "t", "--name", "prom", "--tenant", "acme", "--scopes", "metrics:read"}
	var out, errb bytes.Buffer
	if rc := runOperatorTokenCreate(append(base, "--tenant-metrics"), &out, &errb); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errb.String())
	}
	if got["tenant_metrics"] != true {
		t.Errorf("payload with the flag = %v, want tenant_metrics true", got)
	}
	if rc := runOperatorTokenCreate(base, &out, &errb); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errb.String())
	}
	if _, sent := got["tenant_metrics"]; sent {
		t.Errorf("payload without the flag = %v, want no tenant_metrics", got)
	}
}
