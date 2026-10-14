package webhook

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A definition's body limit is honoured below the server's request limit and
// capped at it: a stored definition may name more, but a delivery is never
// read past what the server accepts on any other ingest path.
func TestReceiver_BodyLimitIsCappedByTheServer(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.MaxRequestBytes = 4 << 20
	rec := &Receiver{cfg: cfg}
	for _, tc := range []struct {
		def  int
		want int64
	}{
		{0, defaultBodySizeLimitBytes},
		{2 << 20, 2 << 20},
		{4 << 20, 4 << 20},
		{64 << 20, 4 << 20},
	} {
		if got := rec.bodyLimit(config.Webhook{BodySizeLimitBytes: tc.def}); got != tc.want {
			t.Errorf("body_size_limit_bytes %d: read up to %d, want %d", tc.def, got, tc.want)
		}
	}
}

// A body nested past the depth limit, or holding more values than the token
// limit, is refused before it is decoded; one at the limits is projected.
func TestProjectPayload_RefusesABodyPastTheShapeLimits(t *testing.T) {
	deep := func(n int) []byte {
		return []byte(strings.Repeat(`{"a":`, n) + `1` + strings.Repeat(`}`, n))
	}
	if _, err := projectPayload(map[string]string{"x": "$.a"}, deep(maxJSONDepth)); err != nil {
		t.Errorf("a body %d deep: %v", maxJSONDepth, err)
	}
	if _, err := projectPayload(map[string]string{"x": "$.a"}, deep(maxJSONDepth+1)); err == nil || !strings.Contains(err.Error(), "nested deeper") {
		t.Errorf("a body %d deep: err=%v, want it refused", maxJSONDepth+1, err)
	}
	if err := checkJSONShape([]byte(`[1,2,3,4]`), 8, 5); err == nil {
		t.Error("six tokens passed a limit of five")
	}
	if err := checkJSONShape([]byte(`[1,2,3]`), 8, 5); err != nil {
		t.Errorf("five tokens at a limit of five: %v", err)
	}
	// Malformed JSON is left to the decoder that follows, which reports it.
	if err := checkJSONShape([]byte(`{"a":`), 8, 100); err != nil {
		t.Errorf("malformed JSON: %v, want it left to the decoder", err)
	}
}
