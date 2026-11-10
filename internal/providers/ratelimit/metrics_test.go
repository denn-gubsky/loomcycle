package ratelimit

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// A retried 429 is counted per provider and by where its wait came from, and
// a wrapping driver's retries are counted under its own name, as its attempt
// spans are.
func TestDo_CountsEachRetryUnderTheAttemptsProvider(t *testing.T) {
	restore, err := lcotel.SetMetricsForTest(false, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	run := func(ctx context.Context) {
		n := 0
		_, err := Do(ctx, Config{
			ParseHeader: AnthropicRetryAfter, Provider: "openai", MaxAttempts: 3, Jitter: 0,
			Schedule: []time.Duration{time.Millisecond}, MaxTotalWait: time.Second,
			OnRetry: func(string, int, time.Duration, string) {},
		}, func(context.Context) (*http.Response, error) {
			n++
			if n < 3 {
				return fakeResp(429, nil, ""), nil
			}
			return fakeResp(200, nil, "ok"), nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run(context.Background())
	run(lcotel.WithProviderOverride(context.Background(), "deepseek"))

	var b bytes.Buffer
	if err := lcotel.WritePrometheus(context.Background(), &b, lcotel.PromOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`loomcycle_provider_retries_total{provider="openai",reason="schedule"} 2`,
		`loomcycle_provider_retries_total{provider="deepseek",reason="schedule"} 2`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, b.String())
		}
	}
}
