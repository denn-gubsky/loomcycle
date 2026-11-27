package grpc

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A surface frame carries the page's record, so a gRPC client sees what the
// SSE stream and the transcript do (RFC GW D24).
func TestEventToProto_SurfaceCarriesThePage(t *testing.T) {
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	out := eventToProto(providers.Event{Type: providers.EventSurface, Surface: &providers.SurfaceInfo{
		ExposureID: "x1", Workspace: "web", Title: "Chart", Kind: "panel", BasePath: "/v1/_workspace/exposures/x1/", ExpiresAt: &at,
	}})
	s := out.GetSurface()
	if out.GetType() != "surface" || s == nil {
		t.Fatalf("event = %+v", out)
	}
	if s.GetExposureId() != "x1" || s.GetWorkspace() != "web" || s.GetTitle() != "Chart" || s.GetKind() != "panel" ||
		s.GetBasePath() != "/v1/_workspace/exposures/x1/" || !s.GetExpiresAt().AsTime().Equal(at) || s.GetReason() != "" {
		t.Errorf("surface = %+v", s)
	}
	closed := eventToProto(providers.Event{Type: providers.EventSurfaceClosed, Surface: &providers.SurfaceInfo{
		ExposureID: "x1", Reason: "idle"}}).GetSurface()
	if closed.GetReason() != "idle" || closed.GetExpiresAt() != nil {
		t.Errorf("surface_closed = %+v", closed)
	}
	if eventToProto(providers.Event{Type: providers.EventText, Text: "hi"}).GetSurface() != nil {
		t.Error("a text frame carries a surface")
	}
}
