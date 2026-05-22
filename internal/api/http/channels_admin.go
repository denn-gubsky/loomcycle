// channels_admin.go — admin/list endpoints for the v0.8.4 Channel
// surface. Phase 0 of the n8n integration RFC: GET /v1/_channels
// returns the operator-declared channel set joined with cheap
// aggregate stats from channel_messages, so n8n's credential-picker
// (and any other operator dashboard) can render a channel-name
// dropdown without re-parsing the loomcycle yaml.
//
// Counts + visible-at bounds come from a single SQL aggregate on
// channel_messages; channels declared in yaml but with no messages
// are still returned (zero counts) so the list reflects the full
// declared surface rather than only "channels that have ever been
// published to."
package http

import (
	"encoding/json"
	"net/http"
)

// handleListChannels serves GET /v1/_channels. Bearer-authed.
// Dispatches through the Connector (the canonical impl lives in
// connector_impl_n8n.go) so MCP + gRPC and the HTTP handler return
// the same shape from the same code path.
func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	resp, err := s.ListChannels(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
