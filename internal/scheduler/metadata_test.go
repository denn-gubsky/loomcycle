package scheduler

import "testing"

// TestBuildRunInput_ThreadsDefMetadata pins that a schedule def's non-secret
// metadata reaches the spawned run as trusted RunInput.Metadata. Fails on the
// pre-feature scheduleDef, which had no metadata field. The scheduler has no
// external inbound body, so PayloadMetadata stays nil.
func TestBuildRunInput_ThreadsDefMetadata(t *testing.T) {
	def := scheduleDef{
		Agent:    "digest",
		Metadata: map[string]any{"repo": "acme/app", "policy": "nightly"},
	}
	in := buildRunInput(def, map[string]bool{}, nil)

	if in.Metadata["repo"] != "acme/app" || in.Metadata["policy"] != "nightly" {
		t.Errorf("schedule def metadata not threaded to RunInput.Metadata: %v", in.Metadata)
	}
	if in.PayloadMetadata != nil {
		t.Errorf("scheduler has no inbound body; PayloadMetadata must be nil, got %v", in.PayloadMetadata)
	}
}
