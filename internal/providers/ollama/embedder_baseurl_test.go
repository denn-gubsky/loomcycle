package ollama

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestEmbedder_BaseURLDefaultFollowsTheRegistration — both registrations share
// newEmbedder, so the no-base_url fallback must branch on the provider id.
// A single localhost default would send an embedder constructed as Ollama Cloud
// to a local box, and the "here is the default I picked" log line would name
// OLLAMA_BASE_URL — the wrong variable for that id — to the operator debugging
// it. Unreachable from the shipped binary (buildEmbedder always supplies a base
// URL) but reachable by any library caller of the exported constructors.
func TestEmbedder_BaseURLDefaultFollowsTheRegistration(t *testing.T) {
	cases := []struct {
		providerID string
		want       string
	}{
		{"ollama-local", defaultBaseURL},
		{"ollama", cloudBaseURL},
	}
	for _, tc := range cases {
		e, err := newEmbedder(tc.providerID, providers.EmbedderOptions{Model: "nomic-embed-text"})
		if err != nil {
			t.Fatalf("%s: newEmbedder: %v", tc.providerID, err)
		}
		if e.baseURL != tc.want {
			t.Errorf("%s: baseURL = %q, want %q", tc.providerID, e.baseURL, tc.want)
		}
		if e.Provider() != tc.providerID {
			t.Errorf("Provider() = %q, want %q", e.Provider(), tc.providerID)
		}
	}
}

// An explicit base_url always wins over either default — the branch must not
// reintroduce a fallback for a caller who configured one.
func TestEmbedder_ExplicitBaseURLOverridesBothDefaults(t *testing.T) {
	for _, id := range []string{"ollama", "ollama-local"} {
		e, err := newEmbedder(id, providers.EmbedderOptions{
			Model:   "nomic-embed-text",
			BaseURL: "http://embed.internal:11434/",
		})
		if err != nil {
			t.Fatalf("%s: newEmbedder: %v", id, err)
		}
		// Trailing slash is trimmed so the "/api/embed" join stays single-slashed.
		if e.baseURL != "http://embed.internal:11434" {
			t.Errorf("%s: baseURL = %q, want the configured endpoint", id, e.baseURL)
		}
	}
}
