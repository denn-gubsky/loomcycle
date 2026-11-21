package http

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// calcServer is a server holding only the Calc tool, with the per-token run
// rate limiter on.
func calcServer(t *testing.T) *Server {
	t.Helper()
	s, _ := tokenAuthServer(t, "")
	s.tools = []tools.Tool{&builtin.Calc{}}
	s.runRate = newRunRateLimiter(nil)
	return s
}

// The connector answers with the in-run tool's own result: exact decimal
// arithmetic, not a float sum.
func TestServerCalc_AnswersWithTheToolsResult(t *testing.T) {
	s := calcServer(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob"})
	res, err := s.Calc(ctx, []byte(`{"expr":"0.1 + 0.2"}`))
	if err != nil || res.IsError {
		t.Fatalf("Calc = %+v, %v", res, err)
	}
	if !strings.Contains(res.Text, `"result":0.3`) || !strings.Contains(res.Text, `"exact":true`) {
		t.Errorf("Calc text = %s, want result 0.3 exact", res.Text)
	}
}

// Each call takes one unit of the token's run rate, as Crypto does: a token
// rated at a burst of two gets two answers and then a rate refusal, which
// never reaches the tool.
func TestServerCalc_EachCallTakesOneUnitOfTheTokensRunRate(t *testing.T) {
	s := calcServer(t)
	p := auth.Principal{TenantID: "acme", Subject: "rita",
		Restrictions: auth.Restrictions{Rate: &auth.RateLimit{PerMinute: 1, Burst: 2}}}
	ctx := auth.WithPrincipal(context.Background(), p)
	for i := 0; i < 2; i++ {
		if res, err := s.Calc(ctx, []byte(`{"expr":"1 + 1"}`)); err != nil || res.IsError {
			t.Fatalf("call %d within the rate = %+v, %v", i+1, res, err)
		}
	}
	res, err := s.Calc(ctx, []byte(`{"expr":"1 + 1"}`))
	if !errors.Is(err, runner.ErrRateLimited) {
		t.Fatalf("call past the rate = %+v, %v; want runner.ErrRateLimited", res, err)
	}
}
