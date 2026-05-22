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
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// ChannelDescriptor is one row in the GET /v1/_channels response.
// Mirrors the connector.ChannelDescriptor shape so the HTTP body, MCP
// list_channels response, and gRPC ListChannels response all encode
// the same way.
type ChannelDescriptor struct {
	Name            string    `json:"name"`
	Scope           string    `json:"scope,omitempty"`
	Semantic        string    `json:"semantic,omitempty"`
	Publisher       string    `json:"publisher,omitempty"`
	Period          string    `json:"period,omitempty"`
	DefaultTTL      int       `json:"default_ttl,omitempty"`
	MaxMessages     int       `json:"max_messages,omitempty"`
	MessageCount    int64     `json:"message_count"`
	OldestVisibleAt time.Time `json:"oldest_visible_at,omitempty"`
	NewestVisibleAt time.Time `json:"newest_visible_at,omitempty"`
}

type channelsListResponse struct {
	Channels []ChannelDescriptor `json:"channels"`
}

// handleListChannels serves GET /v1/_channels. Bearer-authed. Joins
// the operator-declared map in cfg.Channels with the aggregate stats
// over channel_messages. Channels declared but never published-to
// appear with MessageCount=0. Returns 500 on store error.
func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.ChannelStats(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	statsByName := make(map[string]struct {
		Count           int64
		OldestVisibleAt time.Time
		NewestVisibleAt time.Time
	}, len(stats))
	for _, st := range stats {
		statsByName[st.Channel] = struct {
			Count           int64
			OldestVisibleAt time.Time
			NewestVisibleAt time.Time
		}{st.MessageCount, st.OldestVisibleAt, st.NewestVisibleAt}
	}

	out := make([]ChannelDescriptor, 0, len(s.cfg.Channels))
	for name, ch := range s.cfg.Channels {
		desc := channelDescriptorFromConfig(name, ch)
		if st, ok := statsByName[name]; ok {
			desc.MessageCount = st.Count
			desc.OldestVisibleAt = st.OldestVisibleAt
			desc.NewestVisibleAt = st.NewestVisibleAt
		}
		out = append(out, desc)
	}

	// Also surface channels that have rows but are NOT in the
	// declared yaml (shouldn't normally happen post-v0.8.4 since
	// publish validates against the declared set; included so
	// operators investigating orphaned rows after a yaml edit can
	// still see them).
	for name, st := range statsByName {
		if _, declared := s.cfg.Channels[name]; declared {
			continue
		}
		out = append(out, ChannelDescriptor{
			Name:            name,
			MessageCount:    st.Count,
			OldestVisibleAt: st.OldestVisibleAt,
			NewestVisibleAt: st.NewestVisibleAt,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(channelsListResponse{Channels: out})
}

func channelDescriptorFromConfig(name string, ch config.Channel) ChannelDescriptor {
	return ChannelDescriptor{
		Name:        name,
		Scope:       ch.Scope,
		Semantic:    ch.Semantic,
		Publisher:   ch.Publisher,
		Period:      ch.Period,
		DefaultTTL:  ch.DefaultTTL,
		MaxMessages: ch.MaxMessages,
	}
}
