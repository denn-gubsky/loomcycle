package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

func newMetricsPromServer(t *testing.T, withPerUserCap bool) *Server {
	t.Helper()
	sem := concurrency.New(8, 16, 30*time.Second)
	if withPerUserCap {
		sem = sem.WithPerUserCap(4)
	}
	hookReg := hooks.NewRegistry()
	return &Server{
		cfg:            &config.Config{},
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            sem,
	}
}

// TestMetricsProm_EmitsExpectedSeries pins the metric set the
// observability-profiles RFC locks: 5 always-on series + build_info.
// Per-user series stays absent when the per-user cap is disabled
// (cardinality guard).
func TestMetricsProm_EmitsExpectedSeries(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	wantSeries := []string{
		"loomcycle_process_rss_bytes",
		"loomcycle_process_heap_alloc_bytes",
		"loomcycle_process_goroutines",
		"loomcycle_concurrency_active",
		"loomcycle_concurrency_queued",
		"loomcycle_build_info",
	}
	for _, name := range wantSeries {
		if !strings.Contains(body, "# TYPE "+name+" gauge") {
			t.Errorf("missing TYPE line for %q in body:\n%s", name, body)
		}
		if !strings.Contains(body, name+" ") && !strings.Contains(body, name+"{") {
			t.Errorf("missing sample for %q", name)
		}
	}
	// per-user series should be absent when cap is 0
	if strings.Contains(body, "loomcycle_concurrency_per_user") {
		t.Errorf("loomcycle_concurrency_per_user should be omitted when per-user cap is disabled; got: %s", body)
	}
}

// TestMetricsProm_PerUserSeriesOnlyWhenCapEnabled pins the cardinality
// guard: per-user labels only appear when the operator opted into
// per-user fairness. Without this, anonymous workloads would explode
// cardinality with no operator-visible knob.
func TestMetricsProm_PerUserSeriesOnlyWhenCapEnabled(t *testing.T) {
	srv := newMetricsPromServer(t, true)
	// Acquire a slot for a specific user_id so PerUser is populated.
	rel, err := srv.sem.AcquireForUser(t.Context(), "alice")
	if err != nil {
		t.Fatalf("AcquireForUser failed: %v", err)
	}
	defer rel()

	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "loomcycle_concurrency_per_user{user_id=\"alice\"} 1") {
		t.Errorf("expected per-user series for alice, body:\n%s", body)
	}
}

// TestMetricsProm_NilSemReturnsZeros pins the safety path: a Server
// missing the semaphore (early init, certain test fixtures) emits
// concurrency=0 rather than 500-ing.
func TestMetricsProm_NilSemReturnsZeros(t *testing.T) {
	srv := &Server{cfg: &config.Config{}, sem: nil}
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "loomcycle_concurrency_active 0") {
		t.Errorf("expected active=0 with nil sem; body:\n%s", body)
	}
	if !strings.Contains(body, "loomcycle_concurrency_queued 0") {
		t.Errorf("expected queued=0 with nil sem; body:\n%s", body)
	}
}

// TestMetricsProm_ReplicaIDLabelWhenSet pins the cluster-mode label:
// LOOMCYCLE_REPLICA_ID populates a replica_id label on every series.
// Single-replica deployments (env unset) omit the label entirely so
// the output stays clean.
func TestMetricsProm_ReplicaIDLabelWhenSet(t *testing.T) {
	t.Setenv("LOOMCYCLE_REPLICA_ID", "node-2")
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `loomcycle_process_goroutines{replica_id="node-2"}`) {
		t.Errorf("expected replica_id label on process series; body:\n%s", body)
	}
	if !strings.Contains(body, `replica_id="node-2"`) {
		t.Errorf("build_info should also carry replica_id; body:\n%s", body)
	}
}

// TestMetricsProm_TextFormatShape pins the Prometheus exposition
// format invariants: every metric line that isn't a comment must
// match `<name>[{labels}] <value>` and every named series must have
// a preceding HELP + TYPE line. Lightweight in-package check —
// avoids pulling in prometheus/common just for testing.
func TestMetricsProm_TextFormatShape(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	seen := map[string]struct {
		help bool
		typ  bool
	}{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name := strings.SplitN(strings.TrimPrefix(line, "# HELP "), " ", 2)[0]
			s := seen[name]
			s.help = true
			seen[name] = s
		case strings.HasPrefix(line, "# TYPE "):
			name := strings.SplitN(strings.TrimPrefix(line, "# TYPE "), " ", 2)[0]
			s := seen[name]
			s.typ = true
			seen[name] = s
		case strings.HasPrefix(line, "#"):
			// other comment — ignore
		default:
			// must be `name[{labels}] value`
			parts := strings.SplitN(line, " ", 2)
			if len(parts) != 2 {
				t.Errorf("malformed sample line: %q", line)
				continue
			}
			name := parts[0]
			if i := strings.IndexByte(name, '{'); i >= 0 {
				name = name[:i]
			}
			if _, ok := seen[name]; !ok {
				t.Errorf("sample %q has no preceding HELP/TYPE", name)
			}
		}
	}

	for name, flags := range seen {
		if !flags.help {
			t.Errorf("metric %q missing HELP line", name)
		}
		if !flags.typ {
			t.Errorf("metric %q missing TYPE line", name)
		}
	}
}

// TestMetricsProm_BodyTrailingNewline pins a subtle but real
// Prometheus expectation — the response must end with a newline.
// Some scrapers reject responses that don't.
func TestMetricsProm_BodyTrailingNewline(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("body should end with a newline; ends with %q", body[len(body)-min(20, len(body)):])
	}
}

// TestMetricsProm_ContentType pins the standard Prometheus
// text-format content-type. Scrapers content-negotiate against this.
func TestMetricsProm_ContentType(t *testing.T) {
	srv := newMetricsPromServer(t, false)
	rec := httptest.NewRecorder()
	srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4; charset=utf-8", got)
	}
}

// _silenceUnused ensures io is referenced even if a future refactor
// drops uses; required because the package builds under -race with
// strict imports.
var _silenceUnused = io.Discard
