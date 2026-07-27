package http

import (
	"context"
	"encoding/json"
	"fmt"
)

// Config implements connector.Config: the instance-configuration report, at the
// caller's disclosure level, as JSON.
//
// It shares buildConfig with the HTTP handler rather than assembling its own
// report — two transports answering the same question from two implementations is
// exactly the drift this surface was built to avoid.
//
// configViewFor cannot return the public level here: that requires an
// unauthenticated HTTP read under LOOMCYCLE_PUBLIC_CONFIG, and the marker it
// keys on is stamped only by publicOrAuthMiddleware. A connector caller has
// authenticated, so it resolves to authenticated or admin from its principal —
// and open mode (no auth configured) resolves to admin, matching every other
// operator surface.
func (s *Server) Config(ctx context.Context) ([]byte, error) {
	b, err := json.Marshal(s.buildConfig(ctx, configViewFor(ctx)))
	if err != nil {
		return nil, fmt.Errorf("marshal config report: %w", err)
	}
	return b, nil
}
