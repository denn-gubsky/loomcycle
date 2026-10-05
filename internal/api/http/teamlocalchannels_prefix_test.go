package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// A runtime ChannelDef under the reserved prefix would be a declaration, from
// outside a team, of a name inside it.
func TestCreateChannel_RefusesReservedTeamPrefix(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	_, err := srv.CreateChannel(context.Background(), connector.ChannelCreateRequest{Name: "_team/sdlc/events", Scope: "user"})
	if err == nil || !strings.Contains(err.Error(), "reserved for a team's own channels") {
		t.Fatalf("want the reserved-prefix refusal, got %v", err)
	}
}
