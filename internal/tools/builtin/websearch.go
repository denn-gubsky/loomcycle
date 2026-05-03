package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// WebSearch is a lightweight discovery tool: query → ranked list of
// (title, URL, snippet). The model uses it to find pages, then follows
// up with WebFetch for actual content. Capped at 25 results and 256
// chars per snippet so a search call can never blow the context window.
//
// Backend: Brave Search API at https://api.search.brave.com/.
// Required: BRAVE_API_KEY (free tier 2k queries/month). The plan also
// listed a DuckDuckGo HTML scraping fallback; we deferred that to a
// later release because HTML scraping is fragile (DDG can change markup
// any day) and would need its own test infrastructure.
type WebSearch struct {
	// APIKey for Brave. Required: empty key refuses every call.
	APIKey string
	// Endpoint overrides Brave's URL — for tests. Default uses Brave.
	Endpoint string
	// MaxResultsDefault is the default for max_results when unspecified.
	// Plan default is 5. Hard ceiling 25 regardless of caller value.
	MaxResultsDefault int
	// Timeout per call. Default 15s.
	Timeout time.Duration
	// HTTPClient overrides the default. Tests inject one to control
	// dialing; in production the default *http.Client is used.
	HTTPClient *http.Client
	// AllowedHosts narrows the result set to URLs whose host suffix-
	// matches one of these entries. nil = no narrowing (return what
	// Brave returned). The per-run wrapper in narrowing.go sets this.
	AllowedHosts []string
	// FilterMode selects what happens to results whose URL host isn't
	// in AllowedHosts. WebSearchFilterDrop (default when AllowedHosts
	// is set) omits non-matching results entirely; WebSearchFilterKeep
	// returns everything Brave returned and lets the caller filter
	// downstream. Ignored when AllowedHosts is nil.
	FilterMode string
}

// FilterMode constants for WebSearch.FilterMode. The wire form
// (carried in the HTTP request body) uses these exact strings.
const (
	WebSearchFilterDrop = "drop"
	WebSearchFilterKeep = "keep"
)

func (s *WebSearch) Name() string { return "WebSearch" }
func (s *WebSearch) Description() string {
	return "Search the web (lightweight discovery). Returns titles + URLs + short snippets."
}

func (s *WebSearch) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query":       {"type": "string"},
			"max_results": {"type": "integer", "minimum": 1, "maximum": 25}
		},
		"required": ["query"]
	}`)
}

const (
	maxResultsHard    = 25
	maxSnippetChars   = 256
	defaultEndpoint   = "https://api.search.brave.com/res/v1/web/search"
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
	if s.APIKey == "" {
		return tools.Result{Text: "WebSearch requires BRAVE_API_KEY; refusing", IsError: true}, nil
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

	endpoint := s.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	q := url.Values{}
	q.Set("q", args.Query)
	q.Set("count", fmt.Sprint(max))
	httpReq, err := http.NewRequestWithContext(reqCtx, "GET", endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return tools.Result{Text: "build request: " + err.Error(), IsError: true}, nil
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Subscription-Token", s.APIKey)

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return tools.Result{Text: "search request: " + err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tools.Result{Text: "read search response: " + err.Error(), IsError: true}, nil
	}
	if resp.StatusCode != 200 {
		return tools.Result{Text: fmt.Sprintf("brave search returned %d: %s", resp.StatusCode, string(body)), IsError: true}, nil
	}

	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return tools.Result{Text: "parse brave response: " + err.Error(), IsError: true}, nil
	}
	if len(parsed.Web.Results) == 0 {
		return tools.Result{Text: "no results", IsError: false}, nil
	}

	// Per-run host narrowing — applied here so the filter is the LAST
	// thing to touch the result list, after Brave's count and the
	// hard-25 cap. Drop mode is the default when AllowedHosts is set;
	// keep mode is opt-in for callers that want to see everything and
	// filter downstream.
	results := parsed.Web.Results
	if s.AllowedHosts != nil && s.FilterMode != WebSearchFilterKeep {
		filtered := results[:0]
		for _, r := range results {
			if u, err := url.Parse(r.URL); err == nil && hostAllowed(u.Hostname(), s.AllowedHosts) {
				filtered = append(filtered, r)
			}
		}
		results = filtered
	}
	if len(results) == 0 {
		return tools.Result{Text: "no results"}, nil
	}

	var b strings.Builder
	rendered := 0
	for _, r := range results {
		if rendered >= max {
			break
		}
		title := stripHTML(r.Title) // brave returns title with <strong> highlighting
		desc := stripHTML(r.Description)
		if len(desc) > maxSnippetChars {
			desc = desc[:maxSnippetChars] + "…"
		}
		rendered++
		fmt.Fprintf(&b, "[%d] %s — %s\n   %s\n", rendered, title, r.URL, desc)
	}
	return tools.Result{Text: strings.TrimRight(b.String(), "\n")}, nil
}
