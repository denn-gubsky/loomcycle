package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
)

// decodeErrorBody fails the test unless body is JSON, and returns its fields.
func decodeErrorBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("error body is not JSON (%v): %s", err, body)
	}
	return got
}

// %q writes a control byte as \x01 and DEL as \x7f — Go escapes no JSON
// decoder accepts — so an error message carrying one produced an undecodable
// envelope.
func TestWriteJSONError_ControlBytesDecodeToTheMessage(t *testing.T) {
	const msg = "bad byte \x01 and \x7f here"
	rec := httptest.NewRecorder()
	writeJSONError(rec, http.StatusBadRequest, "bad_input", msg)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	got := decodeErrorBody(t, rec.Body.Bytes())
	if got["code"] != "bad_input" || got["error"] != msg {
		t.Errorf("body = %v, want code bad_input and the message verbatim", got)
	}
}

// Adapters match on the raw body, so printable text — quotes, backslashes and
// HTML metacharacters included — must render exactly as the %q envelope did,
// with no < escaping and no trailing newline.
func TestWriteJSONError_PrintableTextMatchesTheQuotedEnvelopeBytes(t *testing.T) {
	const code, msg = "c", `agent_id "a_1" <b> & back\slash`
	rec := httptest.NewRecorder()
	writeJSONError(rec, http.StatusConflict, code, msg)
	if want := fmt.Sprintf(`{"code":%q,"error":%q}`, code, msg); rec.Body.String() != want {
		t.Errorf("body = %s, want %s", rec.Body.String(), want)
	}
}

// Every 429 writeQuotaError sends decodes, keeping its extra fields, when a
// user id or provider name carries a byte %q would have escaped Go-style.
func TestWriteQuotaError_BodiesDecodeWithControlBytes(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		code  string
		field string
		value string
	}{
		{"per-user quota", &concurrency.ErrPerUserQuotaExhausted{UserID: "u\x01", Cap: 2},
			"per_user_quota_exhausted", "user_id", "u\x01"},
		{"provider cap", &concurrency.ErrProviderConcurrencyExhausted{Provider: "p\x01", Cap: 3},
			"provider_concurrency_exhausted", "provider", "p\x01"},
		{"backpressure", fmt.Errorf("queue \x01: %w", &concurrency.BackpressureError{}),
			"backpressure", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeQuotaError(rec, c.err)
			if rec.Code != http.StatusTooManyRequests {
				t.Errorf("status = %d, want 429", rec.Code)
			}
			got := decodeErrorBody(t, rec.Body.Bytes())
			if got["code"] != c.code || got["error"] != c.err.Error() {
				t.Errorf("body = %v, want code %s and error %q", got, c.code, c.err.Error())
			}
			if c.field != "" && got[c.field] != c.value {
				t.Errorf("%s = %v, want %q", c.field, got[c.field], c.value)
			}
		})
	}
}
