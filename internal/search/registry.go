package search

import "fmt"

// Registry is the set of constructed search providers, keyed by ID.
type Registry struct {
	byID map[string]Provider
}

// ProviderSpec is the minimal per-provider config the registry builder needs,
// decoupling internal/search from internal/config. BaseURL is SearXNG-only;
// Options is the test-injection seam (endpoint/client).
type ProviderSpec struct {
	ID      string
	BaseURL string
	Options []Option
}

// BuildRegistry constructs a Registry from specs, dispatching each ID to its
// driver constructor. An unknown ID is an error — config-load validates the set
// against KnownProviderIDs first, so this is a defense-in-depth backstop.
func BuildRegistry(specs []ProviderSpec) (*Registry, error) {
	byID := make(map[string]Provider, len(specs))
	for _, sp := range specs {
		var p Provider
		switch sp.ID {
		case "brave":
			p = NewBrave(sp.Options...)
		case "serper":
			p = NewSerper(sp.Options...)
		case "exa":
			p = NewExa(sp.Options...)
		case "tavily":
			p = NewTavily(sp.Options...)
		case "searxng":
			if sp.BaseURL == "" {
				return nil, fmt.Errorf("search provider %q requires a base_url", sp.ID)
			}
			p = NewSearXNG(sp.BaseURL, sp.Options...)
		default:
			return nil, fmt.Errorf("unknown search provider %q", sp.ID)
		}
		byID[sp.ID] = p
	}
	return &Registry{byID: byID}, nil
}

// Get returns the provider for id.
func (r *Registry) Get(id string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byID[id]
	return p, ok
}

// IDs returns the registered provider IDs (unordered).
func (r *Registry) IDs() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	return out
}

// KnownProviderIDs is the set of provider IDs this build supports — config
// validation rejects any search_providers entry outside it.
func KnownProviderIDs() []string {
	return []string{"brave", "serper", "exa", "tavily", "searxng"}
}

// Ensure the interface is satisfied at compile time for each driver.
var (
	_ Provider = (*braveDriver)(nil)
	_ Provider = (*serperDriver)(nil)
	_ Provider = (*exaDriver)(nil)
	_ Provider = (*tavilyDriver)(nil)
	_ Provider = (*searxngDriver)(nil)
)
