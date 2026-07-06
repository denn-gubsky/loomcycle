package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/search"
)

// TestWebSearch_Live exercises the real Brave Search API through
// loomcycle's WebSearch tool — same code path the job-searcher agent
// hits in production. Skipped when BRAVE_API_KEY is unset so the
// default `go test ./...` run stays hermetic.
//
// Run it explicitly:
//
//	BRAVE_API_KEY=$(grep '^BRAVE_API_KEY=' .env.local | cut -d= -f2-) \
//	  go test -v -count=1 -run TestWebSearch_Live \
//	  ./internal/tools/builtin/
//
// The `-count=1` defeats Go's test-result cache so the call actually
// goes out to Brave each time, not just on the first run.
func TestWebSearch_Live(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("BRAVE_API_KEY"))
	if apiKey == "" {
		t.Skip("BRAVE_API_KEY not set; skipping live Brave call")
	}

	reg, err := search.BuildRegistry([]search.ProviderSpec{{ID: "brave"}}) // real Brave endpoint
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	tool := &WebSearch{
		Registry: reg,
		Resolver: search.NewResolver([]string{"brave"}),
		HostKeys: map[string]string{"brave": apiKey},
		Timeout:  20 * time.Second,
	}

	// Three queries chosen to mirror what job-searcher actually
	// emitted in production: a plain-language one (often the most
	// productive on Brave's free tier), one with a site: filter
	// (the failure mode reported), and a feeds query (Wellfound
	// is a job board the agent is supposed to crawl).
	cases := []struct {
		name  string
		query string
		max   int
	}{
		{"plain", "Robotics Developer remote full-time", 5},
		{"site_filter", `site:linkedin.com/jobs "Mobile Developer" remote`, 5},
		{"jobs_board", `Wellfound "Robotics" remote`, 5},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]any{
				"query":       c.query,
				"max_results": c.max,
			})
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}

			res, err := tool.Execute(context.Background(), input)
			if err != nil {
				t.Fatalf("Execute returned error: %v", err)
			}

			t.Logf("query: %q", c.query)
			t.Logf("isError=%v", res.IsError)
			t.Logf("response (%d chars):\n%s", len(res.Text), res.Text)

			if res.IsError {
				// Don't fail the whole run on a single Brave hiccup —
				// the user is investigating WHY production saw
				// repeated empty results. Keep going so they see
				// the pattern across all three query shapes.
				t.Errorf("Brave returned error: %s", res.Text)
				return
			}
			if strings.TrimSpace(res.Text) == "no results" {
				t.Errorf("Brave returned 'no results' — this is the production failure mode")
				return
			}

			// Smoke check: the rendered format is `[N] Title — URL\n   Description`.
			// At least one numbered line should be present on a successful call.
			if !strings.Contains(res.Text, "[1] ") {
				t.Errorf("response missing expected `[1] ` marker — render contract drifted?")
			}
		})
	}

	// Header echo — Brave's response headers carry rate-limit signal
	// (X-RateLimit-Remaining, X-RateLimit-Reset). loomcycle's WebSearch
	// throws those away today, so we can't assert on them; the next
	// case below would be a separate diagnostic if a per-minute cap
	// is what the agent kept tripping. Logged here as a follow-up.
	fmt.Println("\nTip: if all three sub-tests passed, Brave is reachable from this host with this key.")
	fmt.Println("If only the plain query passed, the site:/quoted-string queries hit Brave's free-tier oddity")
	fmt.Println("(operators report site: filters return zero on freemium; need pro tier for site narrowing).")
}
