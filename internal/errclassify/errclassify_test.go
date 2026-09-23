package errclassify

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestCategoryOf_ClassifiesEveryKnownSentinel pins the category AND the
// retryability of each condition. Retryability is the half that actually
// changes agent behaviour, so asserting the category alone would let a
// transient/retryable=false pairing through — which reads fine and tells the
// agent to give up on something that would have worked.
func TestCategoryOf_ClassifiesEveryKnownSentinel(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		want      tools.ErrorCategory
		retryable bool
		wantHint  *time.Duration
	}{
		{"tier unavailable", resolve.ErrTierUnavailable, tools.CategoryTransient, true, nil},
		{"pin unavailable", resolve.ErrPinUnavailable, tools.CategoryTransient, true, nil},
		{"backpressure", runner.ErrBackpressure, tools.CategoryTransient, true, ptr(backpressureBackoff)},
		{"per-user quota", runner.ErrPerUserQuotaExhausted, tools.CategoryTransient, true, ptr(backpressureBackoff)},
		{"provider concurrency", runner.ErrProviderConcurrencyExhausted, tools.CategoryTransient, true, ptr(backpressureBackoff)},
		{"runtime paused", runner.ErrRuntimePaused, tools.CategoryTransient, true, ptr(pausedBackoff)},
		{"deadline exceeded", context.DeadlineExceeded, tools.CategoryTransient, true, nil},

		{"token limit", runner.ErrTokenLimitExceeded, tools.CategoryBusiness, false, nil},
		{"tier agent not available", resolve.ErrTierAgentNotAvailable, tools.CategoryBusiness, false, nil},
		{"vector unsupported", store.ErrVectorUnsupported, tools.CategoryBusiness, false, nil},
		{"embedder not configured", store.ErrEmbedderNotConfigured, tools.CategoryBusiness, false, nil},
		{"capability unsupported", store.ErrCapabilityUnsupported, tools.CategoryBusiness, false, nil},
		{"dimension mismatch", store.ErrDimensionMismatch, tools.CategoryBusiness, false, nil},
		{"memory quota", store.ErrMemoryQuotaExceeded, tools.CategoryBusiness, false, nil},
		{"value too large", store.ErrMemoryValueTooLarge, tools.CategoryBusiness, false, nil},

		{"operator key restricted", resolve.ErrOperatorKeyRestricted, tools.CategoryPermission, false, nil},
		{"operator key forbidden", providers.ErrOperatorKeyForbidden, tools.CategoryPermission, false, nil},

		{"unknown agent (runner)", runner.ErrUnknownAgent, tools.CategoryValidation, false, nil},
		{"unknown agent (resolve)", resolve.ErrUnknownAgent, tools.CategoryValidation, false, nil},
		{"unknown provider", runner.ErrUnknownProvider, tools.CategoryValidation, false, nil},
		{"invalid argument (runner)", runner.ErrInvalidArgument, tools.CategoryValidation, false, nil},
		{"invalid argument (resolve)", resolve.ErrInvalidArgument, tools.CategoryValidation, false, nil},
		{"session not found", runner.ErrSessionNotFound, tools.CategoryValidation, false, nil},
		{"session required", runner.ErrSessionRequired, tools.CategoryValidation, false, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CategoryOf(tc.err)
			if !ok {
				t.Fatalf("CategoryOf(%v) returned ok=false; every case here must classify", tc.err)
			}
			if got.Category != tc.want {
				t.Errorf("category = %q, want %q", got.Category, tc.want)
			}
			if got.Retryable != tc.retryable {
				t.Errorf("retryable = %v, want %v", got.Retryable, tc.retryable)
			}
			if strings.TrimSpace(got.Description) == "" {
				t.Error("description is empty; a category with no next step is not actionable")
			}
			switch {
			case tc.wantHint == nil && got.RetryAfter != nil:
				t.Errorf("RetryAfter = %v, want absent", *got.RetryAfter)
			case tc.wantHint != nil && got.RetryAfter == nil:
				t.Errorf("RetryAfter absent, want %v", *tc.wantHint)
			case tc.wantHint != nil && *got.RetryAfter != *tc.wantHint:
				t.Errorf("RetryAfter = %v, want %v", *got.RetryAfter, *tc.wantHint)
			}
		})
	}
}

// TestCategoryOf_RetryAfterOnlyOnRetryable guards the invariant that makes the
// hint safe to act on: a backoff attached to a non-retryable failure would tell
// an agent to wait and then retry something that can never succeed.
func TestCategoryOf_RetryAfterOnlyOnRetryable(t *testing.T) {
	for _, err := range []error{
		runner.ErrTokenLimitExceeded,
		resolve.ErrTierAgentNotAvailable,
		resolve.ErrOperatorKeyRestricted,
		runner.ErrUnknownAgent,
		runner.ErrInvalidArgument,
		store.ErrDimensionMismatch,
	} {
		got, ok := CategoryOf(err)
		if !ok {
			t.Fatalf("CategoryOf(%v) did not classify", err)
		}
		if got.Retryable {
			t.Fatalf("%v: expected non-retryable", err)
		}
		if got.RetryAfter != nil {
			t.Errorf("%v: non-retryable error carries RetryAfter=%v", err, *got.RetryAfter)
		}
	}
}

// TestCategoryOf_WrappedErrorsStillClassify — every real call site wraps with
// fmt.Errorf("%w"), so a classifier that only matched bare sentinels would
// classify nothing in production while passing a naive test.
func TestCategoryOf_WrappedErrorsStillClassify(t *testing.T) {
	wrapped := fmt.Errorf("spawn_run: %w", runner.ErrRuntimePaused)
	got, ok := CategoryOf(wrapped)
	if !ok {
		t.Fatal("wrapped sentinel did not classify")
	}
	if got.Category != tools.CategoryTransient {
		t.Errorf("category = %q, want transient", got.Category)
	}
}

// TestCategoryOf_ProviderConcurrencyStructClassifies — the per-provider gate
// returns a struct pointer, not the runner sentinel, so errors.Is alone misses
// it. This is the case the errors.As branch exists for.
func TestCategoryOf_ProviderConcurrencyStructClassifies(t *testing.T) {
	err := error(&concurrency.ErrProviderConcurrencyExhausted{})
	got, ok := CategoryOf(fmt.Errorf("admit: %w", err))
	if !ok {
		t.Fatal("typed provider-concurrency error did not classify")
	}
	if got.Category != tools.CategoryTransient || !got.Retryable {
		t.Errorf("got %q retryable=%v, want transient/true", got.Category, got.Retryable)
	}
}

// TestCategoryOf_UnknownStaysUnclassified — there is deliberately no fallback
// bucket. An invented category is worse than an absent one, because the agent
// acts on it.
func TestCategoryOf_UnknownStaysUnclassified(t *testing.T) {
	for _, err := range []error{
		errors.New("something nobody has reasoned about"),
		nil,
		context.Canceled,
		fmt.Errorf("wrapped: %w", context.Canceled),
	} {
		if got, ok := CategoryOf(err); ok {
			t.Errorf("CategoryOf(%v) classified as %q; want unclassified", err, got.Category)
		}
	}
}

// --- the drift guard ---

// deliberatelyUnclassified lists sentinels that are known and intentionally
// carry no category, with the reason. Adding a name here is a DECISION, which
// is the point: the test below fails on any sentinel that is neither
// classified nor listed, so a new typed error cannot be added to the runtime
// and silently reach an agent as an unlabelled string.
var deliberatelyUnclassified = map[string]string{
	// Infrastructure faults with no caller-actionable next step. Surfacing
	// "internal error / retry?" to an agent invites a retry loop against a
	// broken deployment.
	"runner.ErrInternal": "internal fault; no agent-actionable recovery",

	// A busy session and an in-use agent id are races the caller resolves by
	// waiting or by choosing another id, but both are reached through paths
	// that do not currently surface to a tool caller. Classify when they do.
	"runner.ErrSessionBusy":  "not reachable from a tool caller today",
	"runner.ErrAgentIDInUse": "not reachable from a tool caller today",
	// Starting a configured run that has already started or been discarded:
	// only the HTTP /start route returns it today. Classify when a tool can
	// start a configured run.
	"runner.ErrRunNotConfigured": "not reachable from a tool caller today",

	// Transport capability, decided before any tool call is made.
	"runner.ErrStreamingUnsupported": "transport capability, not a tool outcome",
}

// TestCategoryOf_NoUnclassifiedSentinelDrift parses the sentinel declarations
// out of runner and resolve and asserts each is either classified by
// CategoryOf or explicitly listed above.
//
// It reads the AST rather than restating a list, because a hand-maintained list
// is exactly the thing that drifts — it would pass forever while the runtime
// grew new errors underneath it.
func TestCategoryOf_NoUnclassifiedSentinelDrift(t *testing.T) {
	pkgs := map[string]map[string]error{
		"runner": {
			"ErrUnknownAgent":                 runner.ErrUnknownAgent,
			"ErrInvalidArgument":              runner.ErrInvalidArgument,
			"ErrUnknownProvider":              runner.ErrUnknownProvider,
			"ErrSessionRequired":              runner.ErrSessionRequired,
			"ErrSessionNotFound":              runner.ErrSessionNotFound,
			"ErrSessionBusy":                  runner.ErrSessionBusy,
			"ErrAgentIDInUse":                 runner.ErrAgentIDInUse,
			"ErrBackpressure":                 runner.ErrBackpressure,
			"ErrPerUserQuotaExhausted":        runner.ErrPerUserQuotaExhausted,
			"ErrProviderConcurrencyExhausted": runner.ErrProviderConcurrencyExhausted,
			"ErrRuntimePaused":                runner.ErrRuntimePaused,
			"ErrTokenLimitExceeded":           runner.ErrTokenLimitExceeded,
			"ErrInternal":                     runner.ErrInternal,
			"ErrStreamingUnsupported":         runner.ErrStreamingUnsupported,
			"ErrRunNotConfigured":             runner.ErrRunNotConfigured,
		},
		"resolve": {
			"ErrTierUnavailable":       resolve.ErrTierUnavailable,
			"ErrInvalidArgument":       resolve.ErrInvalidArgument,
			"ErrUnknownAgent":          resolve.ErrUnknownAgent,
			"ErrPinUnavailable":        resolve.ErrPinUnavailable,
			"ErrTierAgentNotAvailable": resolve.ErrTierAgentNotAvailable,
			"ErrOperatorKeyRestricted": resolve.ErrOperatorKeyRestricted,
		},
	}

	for pkg, known := range pkgs {
		declared := sentinelNamesIn(t, "../"+pkg)
		if len(declared) == 0 {
			t.Fatalf("%s: parsed zero sentinels — the AST scan is broken, so this guard proves nothing", pkg)
		}

		// A sentinel declared in source but missing from the map above means
		// the map is stale. Catching that is the whole point.
		for _, name := range declared {
			if _, ok := known[name]; !ok {
				t.Errorf("%s.%s is declared in source but not listed in this test — "+
					"add it to the map and either classify it in CategoryOf or record "+
					"why it is deliberately unclassified", pkg, name)
			}
		}

		for name, err := range known {
			qualified := pkg + "." + name
			_, classified := CategoryOf(err)
			_, exempt := deliberatelyUnclassified[qualified]
			switch {
			case classified && exempt:
				t.Errorf("%s is both classified and listed as deliberately unclassified — "+
					"remove it from deliberatelyUnclassified", qualified)
			case !classified && !exempt:
				t.Errorf("%s has no category and no recorded reason. Either classify it in "+
					"CategoryOf, or add it to deliberatelyUnclassified with why.", qualified)
			}
		}
	}
}

// sentinelNamesIn returns the `Err*` package-level var names declared in dir.
func sentinelNamesIn(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	var names []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, ident := range vs.Names {
						if strings.HasPrefix(ident.Name, "Err") && ident.IsExported() {
							names = append(names, ident.Name)
						}
					}
				}
			}
		}
	}
	return names
}
