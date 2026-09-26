package http

import (
	"fmt"
	"net/http"
)

// writeJSONError writes the API's error envelope: {"code": …, "error": …}.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"code":%q,"error":%q}`, code, msg)
}
