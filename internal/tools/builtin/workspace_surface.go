package builtin

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// surfaceOpened is the surface event expose puts on the run's own stream
// (RFC GW §5.3, D24): only what the RFC names, never a credential or a host
// path.
func surfaceOpened(name string, id, title, kind string, idle time.Duration) providers.Event {
	s := &providers.SurfaceInfo{ExposureID: id, Workspace: name, Title: title, Kind: kind, BasePath: lifecycle.ExposurePrefix(id)}
	if idle > 0 {
		at := time.Now().UTC().Add(idle)
		s.ExpiresAt = &at
	}
	return providers.Event{Type: providers.EventSurface, Surface: s}
}

// SurfaceNotifier tells a run that one of its pages went (RFC GW D24). When
// the closer is the run itself (its own tool call: unexpose, release), the
// event goes on its stream, which also records it; otherwise (a sweep, the
// run's end) it is appended to the run's transcript, which a re-attached
// stream reads, as an operator's retune is. A sub-agent's page is its own
// run's: a sub-agent's stream is its own (RFC GW D24, the run's stream).
func SurfaceNotifier(st store.Store) lifecycle.SurfaceNotifier {
	return func(ctx context.Context, ownerRunID string, c lifecycle.SurfaceClosed) {
		ev := providers.Event{Type: providers.EventSurfaceClosed,
			Surface: &providers.SurfaceInfo{ExposureID: c.ExposureID, Reason: c.Reason}}
		if tools.HasEventEmitter(ctx) && tools.RunID(ctx) == ownerRunID {
			tools.EventEmitter(ctx)(ev)
			return
		}
		if st == nil || ownerRunID == "" {
			return
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return
		}
		if err := st.AppendEvent(context.WithoutCancel(ctx), ownerRunID, string(ev.Type), payload); err != nil {
			slog.WarnContext(ctx, "workspace: cannot record a page's close on its run", "run_id", ownerRunID, "err", err)
		}
	}
}
