package http

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
)

// writeJSONError writes the API's error envelope: {"code": …, "error": …}.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	writeJSONErrorBody(w, status, struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}{code, msg})
}

// writeJSONErrorBody writes an error body that carries fields beyond the
// envelope. It encodes rather than formatting with %q: %q is Go syntax, not
// JSON — it writes \x01 for a control byte and \xff for an invalid UTF-8 byte,
// and no JSON decoder accepts either. HTML escaping is off and the encoder's
// trailing newline is dropped so printable text renders byte-for-byte as the
// %q bodies did, and adapters matching on the raw body keep matching.
func writeJSONErrorBody(w http.ResponseWriter, status int, body any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		// Unreachable for the string/int structs the callers pass; the
		// status still goes out so the caller's error is not turned into 200.
		log.Printf("writeJSONErrorBody encode failed: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}
