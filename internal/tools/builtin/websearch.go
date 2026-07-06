package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/search"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// WebSearch is a lightweight discovery tool: query → ranked list of
// (title, URL, snippet). The model uses it to find pages, then follows up with
// WebFetch for actual content. Capped at 25 results and 256 chars per snippet so
// a search call can never blow the context window.
//
// RFC BB: WebSearch is now a multi-provider FALLBACK CIRCUIT over the
// internal/search catalog (brave/serper/exa/tavily/searxng). It walks the
// resolver's cascade, resolves each provider's key (a tenant CredentialDef
// overrides the operator host key; RFC AR/AX), and on error/empty falls over to
// the next provider — the model sees the same title/URL/snippet text regardless
// of which provider answered.
type WebSearch struct {
	// Registry is the constructed set of search providers; Resolver holds the
	// fallback order + last-outcome availability. Both nil = WebSearch refuses
	// (no providers configured).
	Registry *search.Registry
	Resolver *search.Resolver
	// HostKeys maps provider id → operator host API key (from cfg.Env). Each
	// is passed to ResolveKeyOrOperator so a tenant CredentialDef of the same
	// env-var name overrides it; a keyless provider (searxng) has no entry.
	HostKeys map[string]string

	// MaxResultsDefault is the default for max_results when unspecified.
	// Plan default is 5. Hard ceiling 25 regardless of caller value.
	MaxResultsDefault int
	// Timeout per provider attempt. Default 15s.
	Timeout time.Duration
	// AllowedHosts narrows results to URLs whose host suffix-matches one of
	// these. nil = no narrowing. The per-run wrapper in narrowing.go sets it.
	AllowedHosts []string
	// FilterMode: WebSearchFilterDrop (default when AllowedHosts is set) omits
	// non-matching results; WebSearchFilterKeep returns everything. Ignored
	// when AllowedHosts is nil.
	FilterMode string
}

// FilterMode constants for WebSearch.FilterMode.
const (
	WebSearchFilterDrop = "drop"
	WebSearchFilterKeep = "keep"
)

func (s *WebSearch) Name() string { return "WebSearch" }
func (s *WebSearch) Description() string {
	return "Search the web (lightweight discovery). Returns titles + URLs + short snippets."
}

func (s *WebSearch) InputSchema() json.RawMessage {
	// max_results: model-facing cap is wide enough that overshoot (model
	// trained on WebSearch APIs that allow up to 100) does not produce a
	// schema-violation error round-trip; the runtime silently clamps to
	// maxResultsHard = 25 in Execute(). Without this widening, a single
	// overshoot like max_results=30 made the agent believe the tool itself was
	// broken and cascade into elaborate-query retry loops (see job-searcher
	// prod failure 2026-05-05).
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query":       {"type": "string"},
			"max_results": {"type": "integer", "minimum": 1, "maximum": 100}
		},
		"required": ["query"]
	}`)
}

const (
	maxResultsHard    = 25
	maxSnippetChars   = 256
	defaultMaxResults = 5
)

func (s *WebSearch) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(args.Query) == "" {
		return tools.Result{Text: "query is required", IsError: true}, nil
	}
	if s.Registry == nil || s.Resolver == nil {
		return tools.Result{Text: "WebSearch is not configured (no search providers)", IsError: true}, nil
	}

	max := args.MaxResults
	if max <= 0 {
		max = s.MaxResultsDefault
		if max <= 0 {
			max = defaultMaxResults
		}
	}
	if max > maxResultsHard {
		max = maxResultsHard
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	q := search.Query{Text: args.Query, MaxResults: max}

	// The fallback circuit: walk the cascade, skip un-keyable / cooled-down
	// providers, and fall over on a failed or empty result. Phase 1 uses the
	// global cascade; Phase 3 threads the per-agent search_providers list.
	var lastErr error
	sawSuccess := false
	attempted := 0
	for _, id := range s.Resolver.Cascade(nil) {
		p, ok := s.Registry.Get(id)
		if !ok {
			continue // in the priority list but not built (shouldn't happen post-validate)
		}
		if !s.Resolver.Available(id) {
			continue // in a failure cooldown from an earlier call this process
		}
		apiKey := ""
		if env := p.KeyEnvName(); env != "" {
			// RFC AR: a tenant/user CredentialDef of this env-var name overrides
			// the operator host key. RFC AX: a restricted run with no override
			// gets ErrOperatorKeyForbidden. Either "no key" case → skip this
			// provider silently (it isn't a failure, so no cooldown).
			k, _, _, err := providers.ResolveKeyOrOperator(reqCtx, env, s.HostKeys[id])
			if err != nil || k == "" {
				continue
			}
			apiKey = k
		}
		attempted++
		res, err := p.Search(reqCtx, q, apiKey)
		if err != nil {
			s.Resolver.MarkOutcome(id, err)
			lastErr = err
			log.Printf("websearch: provider %q failed: %v (falling over)", id, err)
			continue
		}
		s.Resolver.MarkOutcome(id, nil)
		sawSuccess = true
		results := filterSearchHosts(res.Results, s.AllowedHosts, s.FilterMode)
		if len(results) == 0 {
			continue // empty (or filtered to empty) → try the next provider
		}
		return tools.Result{Text: renderSearchResults(results, max)}, nil
	}

	switch {
	case attempted == 0:
		return tools.Result{Text: "WebSearch: no keyable search provider is available; configure a search provider (search_providers/search_priority) and its API key", IsError: true}, nil
	case sawSuccess:
		return tools.Result{Text: "no results"}, nil
	default:
		return tools.Result{Text: "WebSearch: all providers failed; last error: " + lastErr.Error(), IsError: true}, nil
	}
}

// filterSearchHosts applies the per-run host narrowing to the normalized
// results — provider-agnostic, so it's the last thing to touch the list.
func filterSearchHosts(in []search.Result, allowed []string, mode string) []search.Result {
	if allowed == nil || mode == WebSearchFilterKeep {
		return in
	}
	out := make([]search.Result, 0, len(in))
	for _, r := range in {
		if u, err := url.Parse(r.URL); err == nil && hostAllowed(u.Hostname(), allowed) {
			out = append(out, r)
		}
	}
	return out
}

// renderSearchResults renders normalized results as the numbered text list the
// model has always seen. stripHTML handles Brave's <strong> highlight tags (a
// no-op on the other providers' plain text).
func renderSearchResults(results []search.Result, max int) string {
	var b strings.Builder
	rendered := 0
	for _, r := range results {
		if rendered >= max {
			break
		}
		title := stripHTML(r.Title)
		desc := stripHTML(r.Snippet)
		if len(desc) > maxSnippetChars {
			desc = desc[:maxSnippetChars] + "…"
		}
		rendered++
		fmt.Fprintf(&b, "[%d] %s — %s\n   %s\n", rendered, title, r.URL, desc)
	}
	return strings.TrimRight(b.String(), "\n")
}
